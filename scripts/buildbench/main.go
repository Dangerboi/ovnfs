// Command buildbench times cargo-shaped build work inside ovnfs checkouts
// beside the same work in a plain host directory, and reports each daemon's
// memory, so daemon changes can be checked for IO-time regressions.
//
// Every -ovnfs binary gets a private daemon (temporary state, mount and HOME,
// so its default settings) opened on one shared fixture: a git repository
// holding a crate (the -project's files, or a synthetic one) plus -files bulk
// files that make the base realistically large, built with git fast-import in
// seconds. Each round of a workload runs its phases on every target in turn,
// starting one target later than the previous round so warm-up and drift
// favor none, and phases report the median:
//
//	io     cargo's filesystem traffic without compilers: a cold build writing
//	       fingerprints, dep-info, rmeta and rlib archives through rustc-style
//	       temporary directories, build-script outputs, codegen objects and a
//	       linked binary hard-linked into place; a no-op build re-reading
//	       fingerprints and dep-info and stat'ing outputs and sources; a
//	       relink after touching one source; a clean.
//	cargo  `cargo build --offline` of the crate: cold in the first -cold
//	       rounds (default 1, as rustc dominates it), then no-op and after a
//	       touch every round.
//
// macOS mounts NFS as the user; Linux needs root for NFS mounts.
//
// Example (old binary built at the previous revision):
//
//	go run ./scripts/buildbench -ovnfs /tmp/ovnfs-old -ovnfs /tmp/ovnfs-new -project /work/jegrep -modes io,cargo
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/stencil-hq/ovnfs/scripts/buildbench/fixture"
	"github.com/stencil-hq/ovnfs/scripts/buildbench/replay"
)

func main() {
	var bins []string
	flag.Func("ovnfs", "ovnfs binary to measure (repeatable)", func(s string) error {
		abs, err := filepath.Abs(s)
		bins = append(bins, abs)
		return err
	})
	project := flag.String("project", "", "cargo project whose files form the fixture crate (default: a synthetic crate)")
	modes := flag.String("modes", "io", "comma-separated workloads: io, cargo")
	runs := flag.Int("runs", 3, "rounds per target; phases report the median")
	files := flag.Int("files", 100000, "bulk files in the fixture besides the crate")
	jobs := flag.Int("jobs", runtime.NumCPU(), "parallel units in io builds, like cargo -j")
	idle := flag.Duration("idle", 0, "pause before an extra idle memory sample (0: none)")
	stall := flag.Duration("stall", 30*time.Second, "phase duration past which the daemon's goroutines are saved (0: never)")
	cold := flag.Int("cold", 1, "rounds that start with a cold cargo build (cargo mode)")
	keep := flag.Bool("keep", false, "keep the temporary directory")
	flag.Parse()
	if len(bins) == 0 {
		fail(errors.New("at least one -ovnfs binary is required"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	b := &bench{runs: *runs, colds: *cold, jobs: *jobs, idle: *idle, stall: *stall}
	for m := range strings.SplitSeq(*modes, ",") {
		if m != "io" && m != "cargo" {
			fail(fmt.Errorf("unknown mode %q", m))
		}
		b.modes = append(b.modes, m)
	}
	// Unix socket paths are short, so the state lives below /tmp, not $TMPDIR.
	root, err := os.MkdirTemp("/tmp", "bb-")
	if err != nil {
		fail(err)
	}
	err = b.run(ctx, root, *project, *files, bins)
	if !*keep {
		os.RemoveAll(root)
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "buildbench:", err)
	os.Exit(1)
}

// bench holds one invocation's settings and its measurements.
type bench struct {
	modes   []string
	runs    int
	colds   int // rounds whose cold-only phases run
	jobs    int
	idle    time.Duration
	stall   time.Duration
	targets []*target
	repo    *fixture.Repo
	build   *replay.Build
}

// target is one directory the workloads run in: the host copy of the crate or
// an ovnfs checkout of the fixture.
type target struct {
	name    string
	dir     string
	bin     string   // ovnfs binary; "" for the host
	env     []string // ovnfs command environment
	pid     int
	capture time.Duration
	times   map[string][]time.Duration
	memory  [][2]string // label, value
}

func (b *bench) run(ctx context.Context, root, project string, files int, bins []string) error {
	fixtureDir := filepath.Join(root, "fixture")
	start := time.Now()
	repo, err := fixture.Make(fixtureDir, project, files)
	if err != nil {
		return fmt.Errorf("fixture: %w", err)
	}
	b.repo = repo
	fmt.Fprintf(os.Stderr, "fixture: crate %s + %d bulk files in %.1fs\n", cmp(project, "synthetic"), files, time.Since(start).Seconds())
	host := &target{name: "host", dir: filepath.Join(root, "host"), times: map[string][]time.Duration{}}
	if err := fixture.CopyCrate(fixtureDir, host.dir); err != nil {
		return fmt.Errorf("host copy: %w", err)
	}
	b.targets = append(b.targets, host)
	defer b.stopDaemons()
	for i, bin := range bins {
		t, err := startCheckout(ctx, filepath.Join(root, strconv.Itoa(i)), bin, fixtureDir)
		if t != nil {
			b.targets = append(b.targets, t)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", bin, err)
		}
		t.sampleMemory("after capture")
	}
	b.build = replay.New(b.jobs, repo.Sources, repo.Touch)
	for _, mode := range b.modes {
		start := time.Now()
		for round := range b.runs {
			for i := range b.targets {
				t := b.targets[(round+i)%len(b.targets)]
				if err := b.round(ctx, t, mode, round); err != nil {
					return fmt.Errorf("%s: %w", t.label(), err)
				}
			}
		}
		for _, t := range b.targets {
			if err := os.RemoveAll(filepath.Join(t.dir, "target")); err != nil {
				return err
			}
		}
		fmt.Fprintf(os.Stderr, "%s: %d rounds in %.1fs\n", mode, b.runs, time.Since(start).Seconds())
	}
	for _, t := range b.targets[1:] {
		t.sampleMemory("after work")
	}
	if b.idle > 0 {
		time.Sleep(b.idle)
		for _, t := range b.targets[1:] {
			t.sampleMemory(fmt.Sprintf("idle %s", b.idle))
		}
	}
	b.report()
	return nil
}

func (t *target) label() string {
	if t.bin == "" {
		return t.name
	}
	return t.name + " (" + t.bin + ")"
}

// round runs mode's phases once in t, timing each; cold-only phases run in
// the first b.colds rounds, starting from a removed target directory, and
// later rounds build on what earlier ones left.
func (b *bench) round(ctx context.Context, t *target, mode string, round int) error {
	for _, p := range b.phases(ctx, mode) {
		if p.cold {
			if round >= b.colds {
				continue
			}
			if err := os.RemoveAll(filepath.Join(t.dir, "target")); err != nil {
				return err
			}
		}
		key := mode + " " + p.name
		start := time.Now()
		var watchdog *time.Timer
		if b.stall > 0 && t.bin != "" {
			watchdog = time.AfterFunc(b.stall, func() { t.saveGoroutines(key, round) })
		}
		err := p.run(t.dir)
		if watchdog != nil {
			watchdog.Stop()
		}
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		t.times[key] = append(t.times[key], time.Since(start))
	}
	return nil
}

// phase is one timed step of a workload, run in a crate directory.
type phase struct {
	name string
	cold bool // only in the first -cold rounds
	run  func(dir string) error
}

func (b *bench) phases(ctx context.Context, mode string) []phase {
	if mode == "io" {
		return b.ioPhases()
	}
	return b.cargoPhases(ctx)
}

func (b *bench) ioPhases() []phase {
	return []phase{
		{name: "cold", run: b.build.Cold},
		{name: "no-op", run: b.build.Noop},
		{name: "touch", run: b.build.Relink},
		{name: "clean", run: b.build.Clean},
	}
}

func (b *bench) cargoPhases(ctx context.Context) []phase {
	build := func(dir string) error {
		cmd := exec.CommandContext(ctx, "cargo", "build", "--offline")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("cargo build: %w\n%s", err, tail(out, 2000))
		}
		return nil
	}
	return []phase{
		{name: "cold", cold: true, run: build},
		{name: "no-op", run: build},
		{name: "touch", run: func(dir string) error {
			if err := replay.Touch(filepath.Join(dir, b.repo.Touch)); err != nil {
				return err
			}
			return build(dir)
		}},
	}
}

// report prints one row per phase: the host's median, then every checkout's
// median and its ratio to the host; then memory samples.
func (b *bench) report() {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	header := []string{"phase"}
	for _, t := range b.targets {
		header = append(header, t.label())
	}
	fmt.Fprintln(w, strings.Join(header, "\t"))
	var keys []string
	for _, mode := range b.modes {
		for _, p := range b.phases(context.Background(), mode) {
			keys = append(keys, mode+" "+p.name)
		}
	}
	for _, key := range keys {
		row := []string{key}
		hostMedian := median(b.targets[0].times[key])
		for i, t := range b.targets {
			m := median(t.times[key])
			cell := fmt.Sprintf("%.3fs", m.Seconds())
			if i > 0 && hostMedian > 0 {
				cell += fmt.Sprintf(" (%.2fx)", float64(m)/float64(hostMedian))
			}
			if len(t.times[key]) > 1 {
				lo, hi := slices.Min(t.times[key]), slices.Max(t.times[key])
				cell += fmt.Sprintf(" [%.3f-%.3f]", lo.Seconds(), hi.Seconds())
			}
			row = append(row, cell)
		}
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	row := []string{"capture (ovnfs new)", "-"}
	for _, t := range b.targets[1:] {
		row = append(row, fmt.Sprintf("%.3fs", t.capture.Seconds()))
	}
	fmt.Fprintln(w, strings.Join(row, "\t"))
	if len(b.targets) > 1 {
		for i := range b.targets[1].memory {
			row := []string{b.targets[1].memory[i][0], "-"}
			for _, t := range b.targets[1:] {
				row = append(row, t.memory[i][1])
			}
			fmt.Fprintln(w, strings.Join(row, "\t"))
		}
	}
	w.Flush()
}

func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(ds))
	return s[len(s)/2]
}

// startCheckout starts a private daemon of bin below dir by opening a
// checkout of fixture, timing the capture.
func startCheckout(ctx context.Context, dir, bin, fixture string) (*target, error) {
	t := &target{name: filepath.Base(bin), bin: bin, times: map[string][]time.Duration{}}
	state, mount, home := filepath.Join(dir, "state"), filepath.Join(dir, "mount"), filepath.Join(dir, "home")
	for _, d := range []string{state, mount, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	t.env = append(os.Environ(), "OVNFS_STATE="+state, "OVNFS_MOUNT="+mount, "HOME="+home, "OVNFS_GIT_REMOTES=false")
	start := time.Now()
	out, err := t.ovnfs(ctx, "new", fixture, "--name", "bench", "--json")
	t.capture = time.Since(start)
	if err != nil {
		return t, err
	}
	var reply struct {
		Checkout struct {
			Path string `json:"path"`
		} `json:"checkout"`
	}
	if err := json.Unmarshal(lastJSON(out), &reply); err != nil || reply.Checkout.Path == "" {
		return t, fmt.Errorf("ovnfs new: no checkout path in %q", out)
	}
	t.dir = reply.Checkout.Path
	out, err = t.ovnfs(ctx, "status", "--json")
	if err != nil {
		return t, err
	}
	var st struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(lastJSON(out), &st); err != nil || st.PID == 0 {
		return t, fmt.Errorf("ovnfs status: no pid in %q", out)
	}
	t.pid = st.PID
	return t, nil
}

// lastJSON returns the last line of out that looks like a JSON object.
func lastJSON(out []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for _, line := range slices.Backward(lines) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("{")) {
			return line
		}
	}
	return out
}

func (t *target) ovnfs(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, t.bin, args...)
	cmd.Env = t.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("ovnfs %s: %w\n%s", strings.Join(args, " "), err, tail(stderr.Bytes(), 2000))
	}
	return out, nil
}

// saveGoroutines writes the daemon's goroutine profile beside the temporary
// directory, which a run removes, while a phase is stalled.
func (t *target) saveGoroutines(phase string, round int) {
	out := filepath.Join(os.TempDir(), fmt.Sprintf("buildbench-stall-%s-%s-r%d.pb.gz", t.name, strings.ReplaceAll(phase, " ", "-"), round+1))
	if _, err := t.ovnfs(context.Background(), "profile", "goroutine", "-o", out); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s stalled in round %d; goroutine profile failed: %v\n", t.label(), phase, round+1, err)
		return
	}
	fmt.Fprintf(os.Stderr, "%s: %s stalled in round %d; goroutines: go tool pprof -traces %s\n", t.label(), phase, round+1, out)
}

func (b *bench) stopDaemons() {
	for _, t := range b.targets {
		if t.bin == "" {
			continue
		}
		if _, err := t.ovnfs(context.Background(), "stop"); err != nil {
			fmt.Fprintln(os.Stderr, "buildbench:", err)
		}
	}
}

var footprintLine = regexp.MustCompile(`(?m)^\s*(phys_footprint(?:_peak)?): (\d+) B`)

// sampleMemory records the daemon's footprint (macOS) or resident set
// (Linux), its peak, and the cache usage `ovnfs status` reports.
func (t *target) sampleMemory(label string) {
	add := func(name, value string) { t.memory = append(t.memory, [2]string{label + ": " + name, value}) }
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("footprint", "--noCategories", "-f", "bytes", strconv.Itoa(t.pid)).Output()
		values := map[string]string{"phys_footprint": "?", "phys_footprint_peak": "?"}
		if err == nil {
			for _, m := range footprintLine.FindAllStringSubmatch(string(out), -1) {
				n, _ := strconv.ParseInt(m[2], 10, 64)
				values[m[1]] = mib(n)
			}
		}
		add("footprint", values["phys_footprint"])
		add("footprint peak", values["phys_footprint_peak"])
	} else {
		status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", t.pid))
		for _, key := range []string{"VmRSS", "VmHWM"} {
			v := "?"
			if m := regexp.MustCompile(key + `:\s+(\d+) kB`).FindSubmatch(status); m != nil {
				n, _ := strconv.ParseInt(string(m[1]), 10, 64)
				v = mib(n << 10)
			}
			add(key, v)
		}
	}
	out, err := t.ovnfs(context.Background(), "status", "--json")
	var st map[string]any
	if err == nil && json.Unmarshal(lastJSON(out), &st) == nil {
		keys := slices.Sorted(func(yield func(string) bool) {
			for k := range st {
				if !yield(k) {
					return
				}
			}
		})
		var parts []string
		for _, k := range keys {
			if u, ok := st[k].(map[string]any); ok && u["used"] != nil {
				parts = append(parts, fmt.Sprintf("%s %s", k, mib(int64(u["used"].(float64)))))
			} else if strings.Contains(k, "heap") || strings.Contains(k, "footprint") {
				if n, ok := st[k].(float64); ok {
					parts = append(parts, fmt.Sprintf("%s %s", k, mib(int64(n))))
				}
			}
		}
		add("status", strings.Join(parts, ", "))
	}
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

func cmp(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
