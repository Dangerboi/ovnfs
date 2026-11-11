#!/usr/bin/env python3
"""Walk isolated ovnfs daemons; Linux kernel NFS/FUSE, never the user's mount.

Run as root for NFS mounts. All state and source files live in a private temporary
folder. --output keeps samples, heap profiles, daemon logs and a JSON summary.
"""
import argparse
import concurrent.futures
import csv
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import importlib.util

spec = importlib.util.spec_from_file_location("nfs_wire", Path(__file__).with_name("nfs-wire.py"))
wire_module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wire_module)


def run(args, **kwargs):
    return subprocess.run(args, check=True, text=True, capture_output=True, **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--proto", choices=["fuse", "nfs"], required=True)
    parser.add_argument("--checkouts", type=int, default=24)
    parser.add_argument("--dirs", type=int, default=400)
    parser.add_argument("--files", type=int, default=128)
    parser.add_argument("--seconds", type=int, default=600)
    parser.add_argument("--rounds", type=int, default=3)
    parser.add_argument("--node-cache", default="512M")
    parser.add_argument("--workers", type=int, default=1)
    parser.add_argument("--mutate", action="store_true")
    parser.add_argument("--restart", action="store_true")
    args = parser.parse_args()
    binary = args.binary.resolve()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, OVNFS_MEMORY="0", OVNFS_BLOB_CACHE="0",
               OVNFS_NODE_CACHE=args.node_cache, OVNFS_GIT_REMOTES="false")
    errors = []
    result = dict(arguments=vars(args), errors=errors)
    result["arguments"] = {k: str(v) if isinstance(v, Path) else v for k, v in vars(args).items()}
    samples = []
    index_samples = []
    index_bytes = 0
    daemon_pid = 0
    stop = threading.Event()
    start = time.monotonic()
    with tempfile.TemporaryDirectory(prefix="ovnfs-node-stress-") as tmp:
        root = Path(tmp)
        source, state, mount = [root / name for name in ("source", "state", "mount")]
        source.mkdir()
        state.mkdir()
        mount.mkdir()
        # restart launches serve afresh, so persist its protocol in a private
        # user configuration as well as passing the first serve's flag.
        test_home = root / "home"
        (test_home / ".o2").mkdir(parents=True)
        (test_home / ".o2" / "ovnfs.yml").write_text(f"proto: {args.proto}\n")
        env["HOME"] = str(test_home)
        (source / ".gitignore").write_text("node_modules/\n")
        for tree in ("src", "node_modules"):
            for d in range(args.dirs):
                directory = source / tree / f"d{d:04d}"
                directory.mkdir(parents=True)
                for f in range(args.files):
                    relative = f"{tree}/d{d:04d}/f{f:04d}"
                    (source / relative).write_text(relative + "\n")
        (source / "wire" / "ancestor" / "held").mkdir(parents=True)
        (source / "wire" / "file").write_bytes(b"moved wire file")
        (source / "wire" / "ancestor" / "held" / "file").write_bytes(b"held wire directory")
        run(["git", "init", "-q", str(source)])
        run(["git", "-C", str(source), "add", "."])
        run(["git", "-C", str(source), "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "fixture"])
        command = [str(binary), "--state", str(state), "--mount", str(mount)]

        def cli(*argv, **kwargs):
            return run(command + list(argv), env=env, **kwargs)

        def status():
            return json.loads(cli("--json", "status").stdout)

        def profile(label):
            nonlocal index_bytes
            cli("profile", "heap", "-o", str(output / (label + ".pb.gz")))
            snapshot = status()
            index_bytes = snapshot.get("node_index_bytes", 0)
            index_samples.append(index_bytes)
            (output / (label + "-status.json")).write_text(json.dumps(snapshot, indent=2))

        def sample():
            while not stop.wait(1):
                try:
                    fields = dict(line.split(":", 1) for line in Path(f"/proc/{daemon_pid}/status").read_text().splitlines() if ":" in line)
                    samples.append([round(time.monotonic() - start, 3), daemon_pid,
                                    int(fields["VmRSS"].split()[0]), int(fields["VmHWM"].split()[0]), index_bytes])
                except (FileNotFoundError, KeyError, ValueError):
                    pass

        daemon_log = (output / "daemon.log").open("w")
        daemon = subprocess.Popen(command + ["serve", "--proto", args.proto], env=env,
                                  stdout=daemon_log, stderr=subprocess.STDOUT)
        threading.Thread(target=daemon.wait, daemon=True).start()
        sampler = threading.Thread(target=sample, daemon=True)
        holders = []
        mutate_future = None
        try:
            # status is only used after the explicit daemon has bound its admin
            # socket; never let a status poll spawn a different daemon.
            for _ in range(200):
                if daemon.poll() is not None:
                    raise RuntimeError(f"serve exited {daemon.returncode}: {(output / 'daemon.log').read_text()}")
                if (state / "admin.sock").exists() or (state / "ovnfs.sock").exists():
                    break
                time.sleep(.05)
            else:
                raise RuntimeError("daemon did not bind its admin socket")
            daemon_pid = status()["pid"]
            sampler.start()
            for c in range(args.checkouts):
                cli("new", str(source), "--name", f"c{c:03d}")
            result["paths"] = args.checkouts * args.dirs * args.files * 2
            profile("initial")
            wire_handles = None
            if args.restart and args.proto == "nfs":
                wire = wire_module.NFSWire(status()["port"])
                co = wire.lookup(wire.mount(), "c000")
                wd = wire.lookup(co, "wire")
                moved = wire.lookup(wd, "file")
                ancestor = wire.lookup(wd, "ancestor")
                held_dir = wire.lookup(ancestor, "held")
                held_child = wire.lookup(held_dir, "file")
                wire_handles = dict(moved=moved.hex(), held_dir=held_dir.hex(), held_child=held_child.hex())
                # Force the old file and directory owners cold before mutating.
                for c in range(args.checkouts):
                    for d in range(args.dirs):
                        parent = mount / f"c{c:03d}" / "src" / f"d{d:04d}"
                        list(parent.iterdir())
                        os.stat(parent / "f0000")
                (mount / "c000" / "wire" / "file").rename(mount / "c000" / "wire" / "moved")
                (mount / "c000" / "wire" / "ancestor").rename(mount / "c000" / "wire" / "relocated")
                result["retained_wire_handles"] = wire_handles
            held_path = mount / "c000" / "src" / "d0000" / "f0000"
            fd = os.open(held_path, os.O_RDONLY)
            holders.append(fd)
            # A separate process holds cwd and an fd throughout eviction and a
            # possible restart. Its checksum is checked on every iteration.
            holder_code = """import os,sys,time
os.chdir(sys.argv[1]);f=os.open('f0000',os.O_RDONLY)
while not os.path.exists(sys.argv[2]):
 assert os.pread(f,100,0)==b'src/d0000/f0000\\n'
 assert open('f0000','rb').read()==b'src/d0000/f0000\\n'
 time.sleep(.01)
"""
            holder_stop = root / "holder-stop"
            holder = subprocess.Popen(["python3", "-c", holder_code, str(held_path.parent), str(holder_stop)],
                                      stdout=(output / "holder.log").open("w"), stderr=subprocess.STDOUT)

            def mutate():
                directory = mount / "c000" / "mutations"
                directory.mkdir()
                count = 0
                while not stop.is_set():
                    a, b = directory / "a", directory / "b"
                    a.write_bytes(b"original")
                    with a.open("rb") as retained:
                        a.rename(b)
                        a.write_bytes(b"replacement")
                        actual = retained.read()
                        if actual != b"original":
                            raise RuntimeError(f"renamed open fd read {actual!r} at iteration {count}")
                    a.unlink()
                    b.unlink()
                    count += 1
                return count

            pool = concurrent.futures.ThreadPoolExecutor(max_workers=args.workers + 1)
            if args.mutate:
                mutate_future = pool.submit(mutate)

            def walk(c):
                base = mount / f"c{c:03d}"
                files = 0
                digest = hashlib.sha256()
                for tree in ("src", "node_modules"):
                    for directory, dirs, names in os.walk(base / tree):
                        dirs.sort()
                        for name in sorted(names):
                            p = Path(directory) / name
                            relative = str(p.relative_to(base))
                            data = p.read_bytes()
                            if data != (relative + "\n").encode():
                                raise RuntimeError(f"wrong-file read {p}: {data!r}")
                            if p.stat().st_size != len(data):
                                raise RuntimeError(f"wrong size: {p}")
                            digest.update(data)
                            files += 1
                expected = args.dirs * args.files * 2
                if files != expected:
                    raise RuntimeError(f"walk lost entries: {files} != {expected}")
                run(["git", "-C", str(base), "status", "--porcelain"])
                return files, digest.hexdigest()

            walked = 0
            rounds = 0
            walk_start = time.monotonic()
            reference = None
            while rounds < args.rounds or time.monotonic() - walk_start < args.seconds:
                # Small batches keep the memory progression visible across
                # checkouts while allowing concurrent walkers.
                for first in range(0, args.checkouts, args.workers):
                    futures = [pool.submit(walk, c) for c in range(first, min(first + args.workers, args.checkouts))]
                    for future in futures:
                        files, digest = future.result(timeout=600)
                        if reference is None:
                            reference = digest
                        if digest != reference:
                            raise RuntimeError("checkout checksum differs")
                        walked += files
                    if first % max(1, args.checkouts // 4) == 0:
                        profile(f"round{rounds}-checkout{first}")
                rounds += 1
                if os.pread(fd, 100, 0) != b"src/d0000/f0000\n":
                    raise RuntimeError("held fd changed content")
                if args.restart and rounds == 1:
                    cli("restart", timeout=120)
                    daemon_pid = status()["pid"]
                    result["restarted"] = True
                    if wire_handles:
                        # A new TCP request to the new server with the exact old
                        # opaque bytes cannot be answered by client page cache.
                        wire = wire_module.NFSWire(status()["port"])
                        for key, data in (("moved", b"moved wire file"), ("held_child", b"held wire directory")):
                            handle = bytes.fromhex(wire_handles[key])
                            wire.getattr(handle)
                            if wire.read(handle) != data:
                                raise RuntimeError("retained wire handle changed content: " + key)
                        directory = bytes.fromhex(wire_handles["held_dir"])
                        wire.getattr(directory)
                        if wire.lookup(directory, "file").hex() != wire_handles["held_child"]:
                            raise RuntimeError("held directory lost its child after ancestor move")
                        result["wire_restart_verified"] = ["GETATTR moved", "READ moved", "GETATTR held child", "READ held child", "GETATTR held dir", "LOOKUP held dir/file"]
                    if os.pread(fd, 100, 0) != b"src/d0000/f0000\n":
                        raise RuntimeError("held fd failed after restart")
            stop.set()
            result.update(rounds=rounds, files_checked=walked, checksum=reference,
                          walk_seconds=time.monotonic() - walk_start,
                          files_per_second=walked / (time.monotonic() - walk_start))
            if mutate_future:
                result["mutations"] = mutate_future.result(timeout=60)
            holder_stop.touch()
            if holder.wait(timeout=30) != 0:
                raise RuntimeError("cwd/fd holder failed")
            pool.shutdown()
            profile("final")
        except Exception as exc:
            errors.append(str(exc))
        finally:
            stop.set()
            for fd in holders:
                os.close(fd)
            if 'holder_stop' in locals():
                holder_stop.touch()
                holder.wait(timeout=30)
            if daemon.poll() is None or daemon_pid:
                try:
                    cli("stop", timeout=120)
                except Exception as exc:
                    errors.append("stop: " + str(exc))
                    daemon.terminate()
            daemon.wait(timeout=120)
            if sampler.is_alive():
                sampler.join(timeout=5)
            daemon_log.close()
    result["duration_seconds"] = time.monotonic() - start
    result["peak_rss_kib"] = max((s[2] for s in samples), default=0)
    result["final_rss_kib"] = samples[-1][2] if samples else 0
    result["peak_index_bytes"] = max(index_samples, default=0)
    result["final_index_bytes"] = index_samples[-1] if index_samples else 0
    with (output / "rss.csv").open("w") as out:
        writer = csv.writer(out)
        writer.writerow(["seconds", "pid", "rss_kib", "hwm_kib", "index_bytes"])
        writer.writerows(samples)
    (output / "results.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))
    if errors:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
