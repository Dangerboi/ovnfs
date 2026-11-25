//go:build darwin || linux

package main

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/scripts/guestbench/wire"
)

// cmdline is the guest kernel command line of the rootfs's boot contract;
// quiet keeps printk off the console the events share.
const cmdline = "console=ttyS0 root=/dev/vda rw init=/init reboot=t panic=-1 quiet"

// The guest's view of user-net: libslirp maps the gateway to the host's
// loopback, where the daemon's NFS port and the echo server listen.
const (
	guestAddr = "10.0.2.15/24"
	gateway   = "10.0.2.2"
)

// remoteTag is the virtio-fs tag of the remote-fs device, the `home` device
// ovnfs shares are made for (api.ShareArgs).
const remoteTag = "home"

// consoleTail is how many console lines a failed config reports.
const consoleTail = 60

// plan describes cfg's tree to the runner. The NFS configs mount the export
// root, where every checkout is a directory named after it, and descend into
// the checkout with Subdir: MOUNT answers any path with the root's handle
// (nfs/mount.go) and PUTROOTFH yields the same root (nfs/v4.go).
// The erofs config gets the image appendImage placed on its disk.
func (b *bench) plan(cfg, checkout string, image *wire.Image) wire.Plan {
	p := wire.Plan{
		Config:    cfg,
		Net:       &wire.Network{Addr: guestAddr, Gateway: gateway},
		Echo:      net.JoinHostPort(gateway, strconv.Itoa(b.echoPort)),
		Workloads: b.o.Workloads,
		Rounds:    b.o.Runs,
		Colds:     b.o.Cold,
		Jobs:      b.o.Jobs,
		Sources:   b.repo.Sources,
		Touch:     b.repo.Touch,
	}
	switch cfg {
	case cfgRemoteFS:
		p.Kind, p.Source = wire.KindVirtiofs, remoteTag
	case cfgNFS3:
		// MOUNT is served on the NFS port (nfs/rpc.go); there is no lock manager.
		p.Kind, p.Source, p.Subdir = wire.KindNFS, gateway+":/", checkout
		p.Options = fmt.Sprintf("vers=3,proto=tcp,port=%d,mountport=%d,mountproto=tcp,nolock,hard", b.daemon.port, b.daemon.port)
	case cfgNFS4:
		p.Kind, p.Source, p.Subdir = wire.KindNFS, gateway+":/", checkout
		p.Options = fmt.Sprintf("vers=4.0,proto=tcp,port=%d,hard", b.daemon.port)
	case cfgExt4:
		p.Kind = wire.KindLocal
	case cfgErofs:
		p.Kind, p.Image = wire.KindImage, image
	case cfgKmod:
		p.Kind = wire.KindModule
	}
	return p
}

// erofsImage is the erofs config's EROFS image of the fixture's work tree,
// built once per run; -out records it.
type erofsImage struct {
	Docker  string  `json:"docker_image"`
	Bytes   int64   `json:"bytes,omitempty"`
	Seconds float64 `json:"build_seconds,omitempty"`
	Error   string  `json:"error,omitempty"`
	path    string
	err     error // fails the erofs config; the others still run
}

// buildErofs builds <root>/lower.erofs from the fixture's work tree without
// its .git, all files owned by root and their mtimes kept (the cargo
// workload compares them), with mkfs.erofs in the docker image the rootfs
// build makes. A failure is recorded in the result, not returned.
func buildErofs(ctx context.Context, image, fixture, root string) *erofsImage {
	e := &erofsImage{Docker: image, path: filepath.Join(root, "lower.erofs")}
	start := time.Now()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--platform", "linux/"+runtime.GOARCH,
		"-v", fixture+":/src:ro", "-v", root+":/out", image,
		"mkfs.erofs", "--all-root", "--exclude-path=.git", "/out/lower.erofs", "/src")
	out, err := cmd.CombinedOutput()
	if err == nil {
		var info os.FileInfo
		if info, err = os.Stat(e.path); err == nil {
			e.Bytes, e.Seconds = info.Size(), time.Since(start).Seconds()
			fmt.Fprintf(os.Stderr, "guestbench: erofs: image of %d MiB built in %.1fs\n", e.Bytes>>20, e.Seconds)
			return e
		}
	}
	e.err = fmt.Errorf("building the erofs image with docker image %s: %w\n%s\nbuild the image with scripts/guestbench/rootfs/build.sh", image, err, out)
	e.Error = e.err.Error()
	return e
}

// kernelModule is the kmod config's ovnfs.ko, prebuilt (-ko) or built once
// per run, and copied into the share; -out records it.
type kernelModule struct {
	Path    string  `json:"path,omitempty"`
	Bytes   int64   `json:"bytes,omitempty"`
	SHA256  string  `json:"sha256,omitempty"`
	Seconds float64 `json:"build_seconds,omitempty"` // 0 for a prebuilt module
	Error   string  `json:"error,omitempty"`
	err     error   // fails the kmod config; the others still run
}

// prepareModule puts the kernel module into the share as wire.ModuleFile:
// ko when set, else the one kmod/build.sh of the module at mod builds for the
// guest's architecture into root. A failure is recorded in the result, not
// returned.
func prepareModule(ctx context.Context, ko, mod, root, share string) *kernelModule {
	m := &kernelModule{Path: ko}
	if m.Path == "" {
		m.Path, m.Seconds, m.err = buildModule(ctx, mod, filepath.Join(root, wire.ModuleFile))
	}
	if m.err == nil {
		m.err = clone.File(m.Path, filepath.Join(share, wire.ModuleFile))
	}
	if m.err == nil {
		var info os.FileInfo
		if info, m.err = os.Stat(m.Path); m.err == nil {
			m.Bytes = info.Size()
			m.SHA256, m.err = fileSHA256(m.Path)
		}
	}
	if m.err != nil {
		m.err = fmt.Errorf("kernel module: %w", m.err)
		m.Error = m.err.Error()
	}
	return m
}

// buildModule runs kmod/build.sh, timed, and returns the module it built:
// the last line the script prints.
func buildModule(ctx context.Context, mod, out string) (string, float64, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, filepath.Join(mod, "kmod", "build.sh"), "--arch", guestArch(), "--out", out)
	cmd.Dir = mod
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return "", 0, fmt.Errorf("kmod/build.sh: %w\n%s%s", err, stdout, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(string(stdout)), "\n")
	path := strings.TrimSpace(lines[len(lines)-1])
	if path == "" {
		return "", 0, errors.New("kmod/build.sh printed no module path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(mod, path)
	}
	seconds := time.Since(start).Seconds()
	fmt.Fprintf(os.Stderr, "guestbench: kmod: module built in %.1fs\n", seconds)
	return path, seconds, nil
}

// writePlan writes p into the share, replacing the previous config's.
func writePlan(share string, p wire.Plan) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(share, wire.PlanFile), data, 0o644)
}

// writeTar writes the hierarchy below dir as an uncompressed tar at dst,
// owned by root, for the runner to unpack on the guest's disk: the fixture's
// git directory, or its work tree with skip (".git") left out. An empty skip
// leaves nothing out.
func writeTar(dir, dst, skip string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	tw := tar.NewWriter(w)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		if skip != "" && p == filepath.Join(dir, skip) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		case !info.Mode().IsRegular() && !info.IsDir():
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
	return errors.Join(err, tw.Close(), w.Flush(), f.Close())
}

// disk is a VM's private writable copy of the base rootfs image.
type disk struct {
	kind  string   // "clone" or "overlay"
	path  string   // the image vmon attaches as /dev/vda
	args  []string // vmon flags attaching it
	files []string // files to remove once the VM is gone
}

// privateDisk gives the VM name a writable root disk without touching base:
// a block-sharing clone in dir where the filesystem has them (APFS, btrfs,
// XFS), which is free and as fast as the base; otherwise a vmon copy-on-write
// overlay of base. An overlay hard-links its base beside itself, so it goes
// next to base when dir is on another volume.
func privateDisk(base, dir, name string) (*disk, error) {
	img := filepath.Join(dir, name+".img")
	in, err := os.Open(base)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	err = clone.Reflink(in, img, 0o600)
	if err == nil {
		return &disk{kind: "clone", path: img, args: []string{"--rootfs", img}, files: []string{img}}, nil
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		return nil, err
	}
	same, err := sameVolume(base, dir)
	if err != nil {
		return nil, err
	}
	if !same {
		img = filepath.Join(filepath.Dir(base), fmt.Sprintf(".guestbench-%d-%s.img", os.Getpid(), name))
	}
	return &disk{kind: "overlay", path: img, args: []string{"--rootfs", img, "--disk-overlay-of", base}, files: []string{img, img + ".base"}}, nil
}

// appendImage stores the image at src on the disk behind its filesystem,
// at the disk's size rounded up to 1 MiB, and pads the disk to whole 512-byte
// sectors; the gap and the padding stay holes. It returns where the guest
// finds the image on /dev/vda. Only a clone can grow: an overlay's guest
// disk keeps its base's size.
func (d *disk) appendImage(src string) (*wire.Image, error) {
	if d.kind != "clone" {
		return nil, errors.New("erofs needs a block-sharing clone of the rootfs to append its image to; this volume cannot clone, so vmon would use an overlay of the base's size")
	}
	in, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return nil, err
	}
	out, err := os.OpenFile(d.path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	st, err := out.Stat()
	if err != nil {
		out.Close()
		return nil, err
	}
	img := &wire.Image{Device: "/dev/vda", Offset: roundUp(st.Size(), 1<<20), Size: roundUp(info.Size(), 512)}
	err = out.Truncate(img.Offset)
	if err == nil {
		_, err = io.Copy(io.NewOffsetWriter(out, img.Offset), in)
	}
	if err == nil {
		err = out.Truncate(img.Offset + img.Size)
	}
	if err = errors.Join(err, out.Close()); err != nil {
		return nil, fmt.Errorf("appending %s to %s: %w", src, d.path, err)
	}
	return img, nil
}

// roundUp rounds n up to a multiple of unit, a power of two.
func roundUp(n, unit int64) int64 { return (n + unit - 1) &^ (unit - 1) }

// release removes the disk's files.
func (d *disk) release() {
	for _, f := range d.files {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "guestbench:", err)
		}
	}
}

// sameVolume reports whether a and b live on one filesystem, where a hard
// link between them works.
func sameVolume(a, b string) (bool, error) {
	ia, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return ia.Sys().(*syscall.Stat_t).Dev == ib.Sys().(*syscall.Stat_t).Dev, nil
}

// vmonArgs is the vmon command line booting cfg's runner on d, with the
// checkout's share socket sock attached: as the remote-fs device `home` for
// remotefs, as the kernel module's relay for kmod.
func (b *bench) vmonArgs(d *disk, cfg, sock string) []string {
	args := []string{"vmm", "--kernel", b.o.Kernel}
	args = append(args, d.args...)
	args = append(args,
		"--cmdline", cmdline,
		"--mem", strconv.Itoa(b.o.Mem),
		"--cpus", strconv.Itoa(b.o.CPUs),
		"--rng",
		"--net", "user",
		"--volume", wire.Tag+":"+b.share+":ro")
	switch cfg {
	case cfgRemoteFS:
		args = append(args, "--remote-fs", remoteTag+":"+sock+":rw")
	case cfgKmod:
		args = append(args, "--ovnfs", sock)
	}
	// The Linux sandbox's Landlock rules admit the kernel, the disk, the
	// volume and the remote-fs socket's directory, so it needs no opt-out;
	// but as root vmon refuses it without a uid to drop to, which could not
	// read the run's private directory. macOS has no sandbox to opt out of.
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		args = append(args, "--no-sandbox")
	}
	return append(args, b.o.VMONArgs...)
}

// vm is one running vmon and its console: every line goes to the console
// log and the tail, and the runner's events to events.
type vm struct {
	cmd    *exec.Cmd
	events chan wire.Event // closed when vmon's stdout ends
	stop   chan struct{}   // closed when events are no longer wanted
	once   sync.Once
	exited chan struct{} // closed once vmon has been waited for
	err    error         // vmon's exit, set before exited closes

	mu   sync.Mutex
	log  *os.File
	tail []string
}

// startVM starts vmon with args in its own process group, so a terminal's
// interrupt reaches only guestbench, teeing its output to logPath.
func startVM(bin string, args []string, logPath string) (*vm, error) {
	log, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	v := &vm{cmd: exec.Command(bin, args...), events: make(chan wire.Event, 64), stop: make(chan struct{}), exited: make(chan struct{}), log: log}
	v.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := v.cmd.StdoutPipe()
	if err == nil {
		var stderr io.ReadCloser
		if stderr, err = v.cmd.StderrPipe(); err == nil {
			err = v.cmd.Start()
		}
		if err == nil {
			var wg sync.WaitGroup
			wg.Go(func() {
				v.read(stdout, v.decode)
				close(v.events)
			})
			wg.Go(func() { v.read(stderr, nil) })
			go func() {
				wg.Wait()
				v.err = v.cmd.Wait()
				log.Close()
				close(v.exited)
			}()
			return v, nil
		}
	}
	log.Close()
	return nil, fmt.Errorf("starting vmon: %w", err)
}

// read records every line of r, handing each to decode when it is set, and
// drains whatever is left once a line is too long to scan.
func (v *vm) read(r io.Reader, decode func(string)) {
	lines := bufio.NewScanner(r)
	lines.Buffer(make([]byte, 64<<10), 4<<20)
	for lines.Scan() {
		line := strings.TrimRight(lines.Text(), "\r")
		v.record(line)
		if decode != nil {
			decode(line)
		}
	}
	io.Copy(io.Discard, r)
}

// decode delivers the event a console line carries. Kernel and vmon log
// lines share the console, so the prefix may follow other output; a line
// whose JSON does not decode is logged and skipped.
func (v *vm) decode(line string) {
	_, after, ok := strings.Cut(line, wire.Prefix)
	if !ok {
		return
	}
	var ev wire.Event
	if err := json.Unmarshal([]byte(after), &ev); err != nil {
		fmt.Fprintf(os.Stderr, "guestbench: skipping undecodable event line %q: %v\n", line, err)
		return
	}
	select {
	case v.events <- ev:
	case <-v.stop:
	}
}

// record appends a line to the console log and the tail.
func (v *vm) record(line string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	fmt.Fprintln(v.log, line)
	v.tail = append(v.tail, line)
	if len(v.tail) > consoleTail {
		v.tail = v.tail[len(v.tail)-consoleTail:]
	}
}

// failure wraps err with the console's last lines.
func (v *vm) failure(err error) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return fmt.Errorf("%w\nlast %d console lines:\n%s", err, len(v.tail), strings.Join(v.tail, "\n"))
}

// terminate stops event delivery, gives vmon grace to exit on its own, then
// sends its process group SIGTERM and, 5s later, SIGKILL, and waits for it.
func (v *vm) terminate(grace time.Duration) {
	v.once.Do(func() { close(v.stop) })
	for _, sig := range []syscall.Signal{0, syscall.SIGTERM, syscall.SIGKILL} {
		if sig != 0 {
			syscall.Kill(-v.cmd.Process.Pid, sig)
		}
		select {
		case <-v.exited:
			return
		case <-time.After(grace):
		}
		grace = 5 * time.Second
	}
	<-v.exited
}
