package overlay

import (
	"errors"
	"fmt"
	"io"
	maps0 "maps"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/ident"
)

// defaultModelOps is the operation count of one sequential run;
// OVERLAY_MODEL_OPS overrides it for soak runs.
const defaultModelOps = 1500

// modelSeed returns OVERLAY_MODEL_SEED, or the fixed default seed.
func modelSeed(t *testing.T) uint64 {
	t.Helper()
	if s := os.Getenv("OVERLAY_MODEL_SEED"); s != "" {
		seed, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("OVERLAY_MODEL_SEED: %v", err)
		}
		return seed
	}
	return 1
}

// modelOps returns OVERLAY_MODEL_OPS, or the default operation count.
func modelOps(t *testing.T) int {
	t.Helper()
	if s := os.Getenv("OVERLAY_MODEL_OPS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			t.Fatalf("OVERLAY_MODEL_OPS: %q", s)
		}
		return n
	}
	return defaultModelOps
}

// skipModelOnWindows skips the model tests where POSIX semantics they model
// (removing or renaming over open files, hard links, permission bits) do not
// hold.
func skipModelOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the model is POSIX: open files cannot be removed or renamed over on Windows")
	}
}

// hostUmask measures the process's file creation mask with a scratch directory.
func hostUmask(t *testing.T) os.FileMode {
	t.Helper()
	p := filepath.Join(t.TempDir(), "umask")
	must(t, os.Mkdir(p, 0o777))
	info, err := os.Stat(p)
	must(t, err)
	return 0o777 &^ info.Mode().Perm()
}

// lowerEntry is one entry of a lower level fixture: kind is "dir", "opq"
// (an opaque directory), "file", "link" (data is the target), "wh" (a
// whiteout of the name) or "hard" (data is the level path it links to).
type lowerEntry struct {
	kind string
	data string
	perm os.FileMode
}

// dirE returns a plain directory entry.
func dirE() lowerEntry { return lowerEntry{kind: "dir", perm: 0o755} }

// opqE returns an opaque directory entry.
func opqE() lowerEntry { return lowerEntry{kind: "opq", perm: 0o755} }

// fileE returns a regular file entry with the given mode.
func fileE(data string, perm os.FileMode) lowerEntry {
	return lowerEntry{kind: "file", data: data, perm: perm}
}

// linkE returns a symlink entry.
func linkE(target string) lowerEntry { return lowerEntry{kind: "link", data: target} }

// whE returns a whiteout entry.
func whE() lowerEntry { return lowerEntry{kind: "wh"} }

// hardE returns a hard link to another path of the same level.
func hardE(to string) lowerEntry { return lowerEntry{kind: "hard", data: to} }

// sortedEntries lists a level's paths, parents before children.
func sortedEntries(level map[string]lowerEntry) []string {
	paths := make([]string, 0, len(level))
	for p := range level {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		a, b := strings.Split(paths[i], "/"), strings.Split(paths[j], "/")
		return slices.Compare(a, b) < 0
	})
	return paths
}

// buildLevel writes a lower level as an AUFS host tree with every entry's
// mtime at past, and returns its directory. Every parent directory must be
// an entry of the level.
func buildLevel(t *testing.T, level map[string]lowerEntry) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "level")
	must(t, os.Mkdir(dir, 0o755))
	must(t, os.Chmod(dir, 0o755))
	paths := sortedEntries(level)
	for _, p := range paths {
		e := level[p]
		if par := mParent(p); par != "" {
			if pe, ok := level[par]; !ok || pe.kind != "dir" && pe.kind != "opq" {
				t.Fatalf("fixture %q lacks its parent directory", p)
			}
		}
		real := filepath.Join(dir, p)
		switch e.kind {
		case "dir", "opq":
			must(t, os.Mkdir(real, e.perm))
			must(t, os.Chmod(real, e.perm))
			if e.kind == "opq" {
				must(t, os.WriteFile(filepath.Join(real, opaqueName), nil, 0o644))
			}
		case "file":
			must(t, os.WriteFile(real, []byte(e.data), e.perm))
			must(t, os.Chmod(real, e.perm))
		case "link":
			must(t, os.Symlink(e.data, real))
		case "wh":
			must(t, os.WriteFile(filepath.Join(filepath.Dir(real), whPrefix+filepath.Base(real)), nil, 0o644))
		case "hard":
		default:
			t.Fatalf("fixture %q: kind %q", p, e.kind)
		}
	}
	for _, p := range paths {
		if e := level[p]; e.kind == "hard" {
			must(t, os.Link(filepath.Join(dir, e.data), filepath.Join(dir, p)))
		}
	}
	must(t, filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return hostfile.Lchtimes(p, past, past)
	}))
	return dir
}

// applyLevel merges a lower level over the model as the overlay stacks
// levels: whiteouts hide names, opaque directories hide everything below
// them, a directory over a directory merges, anything else replaces. A
// visible directory is the topmost level's, so each level's directory is a
// node of its own. Hard links of a frozen layer (layer true) are shared.
func (m *model) applyLevel(level map[string]lowerEntry, layer bool) {
	made := make(map[string]*mNode)
	for _, p := range sortedEntries(level) {
		e := level[p]
		_, _, dir, cls := m.walk(mParent(p))
		if cls != "" || dir.kind != mDir {
			panic("fixture parent missing: " + p)
		}
		name := path.Base(p)
		old := dir.kids[name]
		if old != nil {
			old.names--
		}
		var n *mNode
		switch e.kind {
		case "wh":
			delete(dir.kids, name)
			continue
		case "dir", "opq":
			n = m.newNode(mDir)
			n.perm = e.perm
			if e.kind == "dir" && old != nil && old.kind == mDir {
				n.kids = old.kids
			}
		case "file":
			n = m.newNode(mFile)
			n.perm, n.data = e.perm, []byte(e.data)
		case "link":
			n = m.newNode(mLink)
			n.target = e.data
		case "hard":
			n = made[e.data]
			n.names++
			n.shared = layer
		}
		n.mtime, n.lower = past, true
		dir.kids[name] = n
		made[p] = n
	}
}

// modelStack is a lower stack and the model of what it shows.
type modelStack struct {
	bottom Bottom
	layers []*Layer // top first
	dirs   []string // host directories of every level, checked for writes at the end
	m      *model
}

// newModelStack builds levels (bottom first; the rest become frozen layers)
// and the model of their merged view, with a fresh upper's root on top.
func newModelStack(t *testing.T, ids *atomic.Int64, levels ...map[string]lowerEntry) *modelStack {
	t.Helper()
	umask := hostUmask(t)
	s := &modelStack{m: &model{ids: ids, umask: umask, superuser: os.Geteuid() == 0}}
	s.m.root = s.m.newNode(mDir)
	// The upper's root is created with 0o755 less the umask and keeps the
	// lower root's mtime (overlay.go New: "A fresh upper must not change the
	// visible root's timestamp").
	s.m.root.perm, s.m.root.mtime = 0o755&^umask, past
	for i, level := range levels {
		dir := buildLevel(t, level)
		s.dirs = append(s.dirs, dir)
		s.m.applyLevel(level, i > 0)
		if i == 0 {
			s.bottom = dirBottom{dir}
			continue
		}
		l, err := IndexLayer(dir)
		must(t, err)
		s.layers = append([]*Layer{l}, s.layers...)
	}
	return s
}

// lowerSnapshots records every level directory's host state.
func lowerSnapshots(t *testing.T, dirs []string) map[string][]string {
	t.Helper()
	out := make(map[string][]string, len(dirs))
	for _, dir := range dirs {
		out[dir] = snapshot(t, dir)
	}
	return out
}

// assertLowersUnchanged fails if the overlay wrote to any immutable level.
func assertLowersUnchanged(t *testing.T, before map[string][]string) {
	t.Helper()
	for dir, want := range before {
		if got := snapshot(t, dir); !slices.Equal(got, want) {
			t.Errorf("immutable level %s changed:\nbefore %q\nafter  %q", dir, want, got)
		}
	}
}

// space is the part of the namespace one operation stream names.
type space struct {
	dirs    []string          // parent directories of generated names
	leaves  []string          // final names
	prefix  map[string]string // a prefix for leaves in some directories
	targets []string          // symlink targets
	own     func(string) bool // paths the stream may name; nil: every path
	fixed   map[string]bool   // paths never mutated or created
}

// sequentialSpace names entries anywhere in the sequential fixture, so names
// collide with lower files, whiteouts, opaque directories and each other.
func sequentialSpace() space {
	return space{
		dirs:    []string{"", "", "d1", "d1/sub", "d2", "d2/sub", "d3", "d4", "e", "a", "s", "n"},
		leaves:  []string{"a", "b", "c", "n", "x", "h1", "h2", "s", "sub", "z"},
		targets: []string{"a", "b", "../a", "d1", "sub", "missing", "s", "../d1/a", "c", "n", "h1"},
		fixed:   map[string]bool{"": true},
	}
}

// workerSpace confines stream k to its subtree gk and to names starting
// gk- in the directory all streams share (and at the root, where a link
// renamed to gk can create them).
func workerSpace(k int) space {
	g := fmt.Sprintf("g%d", k)
	return space{
		dirs:   []string{g, g, g + "/d", g + "/d/sub", g + "/e", g + "/a", "shared", "shared", "shared/" + g + "-d"},
		leaves: []string{"a", "b", "c", "n", "x", "h", "s", "sub", "d"},
		prefix: map[string]string{"shared": g + "-"},
		// Opening a dangling symlink with O_CREATE creates its target, so
		// targets carry the stream's prefix wherever the link moves.
		targets: []string{g + "-a", g + "-b", g + "-d", g + "-d/f", g + "-missing", g + "-s", g + "-n"},
		own: func(p string) bool {
			return p == g || strings.HasPrefix(p, g+"/") || strings.HasPrefix(p, g+"-") || strings.HasPrefix(p, "shared/"+g+"-")
		},
		fixed: map[string]bool{"": true, "shared": true},
	}
}

// heldFile is a handle an operation stream keeps open across operations,
// with the model node it reads and writes.
type heldFile struct {
	h        billy.File
	n        *mNode
	snap     []byte // non-nil for a read-only handle of a lower file, see [model.openFile]
	writable bool
	readable bool
	path     string
}

// runner applies one random operation stream to an overlay and its model.
type runner struct {
	name  string
	seed  uint64
	rng   *rand.Rand
	f     *FS
	m     *model
	ids   *idTracker
	mem   *Memory
	sp    space
	held  []*heldFile
	hist  []string
	step  int
	stats map[string]int
	// findings is OVERLAY_MODEL_FINDINGS: the known findings the stream
	// reproduces, see [runner.reproduces].
	findings string
	tail     int // operations a failure report shows
}

// newRunner starts a stream with its own generator.
func newRunner(name string, seed, stream uint64, f *FS, m *model, ids *idTracker, mem *Memory, sp space) *runner {
	return &runner{name: name, seed: seed, rng: rand.New(rand.NewPCG(seed, stream)), f: f, m: m, ids: ids, mem: mem, sp: sp,
		stats: make(map[string]int), findings: os.Getenv("OVERLAY_MODEL_FINDINGS"), tail: historyTail()}
}

// Known findings: overlay results that differ from the model where no doc
// or existing test justifies the overlay. The model keeps the POSIX
// expectation; the generators leave out the operations that hit a finding
// unless OVERLAY_MODEL_FINDINGS names it (a comma-separated list, or "all"),
// which reproduces it.
const (
	// findingReadDirBelowFile: ReadDir of a path below a regular file that
	// only the bottom provides fails with the bottom's ENOTDIR, while Lstat
	// of that path, and ReadDir below a file of any other level, report it
	// missing (ENOENT), the overlay's rule for such paths.
	findingReadDirBelowFile = "readdir-below-file"
	// findingRootMtime: reopening a non-empty upper (a daemon restart) and
	// freezing it (a snapshot) move the visible root's mtime to the present,
	// with no change to the root's entries: New recreates the work directory
	// inside the upper's root, and the freeze caller removes it.
	findingRootMtime = "root-mtime"
)

// reproduces reports whether OVERLAY_MODEL_FINDINGS asks for finding name.
func (r *runner) reproduces(name string) bool { return findingAsked(r.findings, name) }

// findingAsked reports whether the OVERLAY_MODEL_FINDINGS value findings
// names finding name.
func findingAsked(findings, name string) bool {
	return findings == "all" || slices.Contains(strings.Split(findings, ","), name)
}

// knownFinding reports whether the operation desc hits a known finding this
// run leaves out (hit), recording the skip.
func (r *runner) knownFinding(name, desc string, hit bool) bool {
	if !hit || r.reproduces(name) {
		return false
	}
	r.record(desc + " -> skipped, known finding " + name)
	r.stats["finding "+name]++
	return true
}

// scope is the stream's view comparison.
func (r *runner) scope(keep func(string) bool) viewScope { return viewScope{keep: keep} }

// forgetRootMtime makes the root's mtime unpredictable after a reopen or a
// freeze (see findingRootMtime) unless the run reproduces that finding.
func (r *runner) forgetRootMtime(what string) {
	if r.reproduces(findingRootMtime) || r.m.root.mtime.IsZero() {
		return
	}
	r.m.root.mtime = time.Time{}
	r.record(what + ": root mtime forgotten, known finding " + findingRootMtime)
	r.stats["finding "+findingRootMtime]++
}

// record appends an entry to the operation history.
func (r *runner) record(s string) {
	r.hist = append(r.hist, fmt.Sprintf("%5d %s", r.step, s))
	if len(r.hist) > 2*r.tail {
		r.hist = slices.Clone(r.hist[len(r.hist)-r.tail:])
	}
}

// report formats a difference with the reproduction seed and the history tail.
func (r *runner) report(t *testing.T, err error) string {
	tail := r.hist[max(0, len(r.hist)-r.tail):]
	env := fmt.Sprintf("OVERLAY_MODEL_SEED=%d", r.seed)
	if r.findings != "" {
		env += " OVERLAY_MODEL_FINDINGS=" + r.findings
	}
	return fmt.Sprintf("%s: step %d: %v\nrerun: %s go test -count=1 -run '^%s$' ./overlay/\nlast operations:\n%s",
		r.name, r.step, err, env, strings.ReplaceAll(t.Name(), "/", "$/^"), strings.Join(tail, "\n"))
}

// expect compares the overlay's result class with the model's and records it.
func (r *runner) expect(desc, want string, err error) error {
	if want == "" {
		want = clOK
	}
	got := modelClass(err)
	r.record(desc + " -> " + got)
	r.stats[strings.Fields(desc)[0]+" "+got]++
	if got != want {
		return fmt.Errorf("%s: overlay %s (%v), model %s", desc, got, err, want)
	}
	return nil
}

// skip records an operation the model leaves undefined.
func (r *runner) skip(desc string) {
	r.record(desc + " -> skipped, undefined")
	r.stats["skipped"]++
}

// pick returns a random element.
func pick[T any](r *runner, items []T) T { return items[r.rng.IntN(len(items))] }

// existing returns the stream's visible paths.
func (r *runner) existing() []string {
	var out []string
	for _, p := range r.m.paths() {
		if r.sp.own == nil || r.sp.own(p) {
			out = append(out, p)
		}
	}
	return out
}

// existingKind returns the stream's visible paths of one kind, with the
// directories it may create entries in (the shared one included).
func (r *runner) existingKind(kind mKind) []string {
	var out []string
	for _, p := range r.m.paths() {
		if r.sp.own != nil && !r.sp.own(p) && !(kind == mDir && r.sp.fixed[p]) {
			continue
		}
		if _, _, n, cls := r.m.walk(p); cls == "" && n.kind == kind {
			out = append(out, p)
		}
	}
	return out
}

// randPath returns a name to operate on: an existing one, a composed one
// (often a lower name, a whiteout's or one below a file), or a directory.
func (r *runner) randPath() string {
	for {
		var p string
		switch x := r.rng.IntN(20); {
		case x < 9:
			if paths := r.existing(); len(paths) > 0 {
				p = pick(r, paths)
			}
		case x < 11:
			p = pick(r, r.sp.dirs)
		default:
			dir := pick(r, r.sp.dirs)
			p = path.Join(dir, r.sp.prefix[dir]+pick(r, r.sp.leaves))
		}
		if p != "" && !r.sp.fixed[p] {
			return p
		}
	}
}

// heldPath returns the path a random held handle was opened at, or a random path.
func (r *runner) heldPath() string {
	if len(r.held) > 0 && r.rng.IntN(3) == 0 {
		if p := pick(r, r.held).path; !r.sp.fixed[p] {
			return p
		}
	}
	return r.randPath()
}

// randData returns 1..max bytes of lowercase letters.
func (r *runner) randData(limit int) []byte {
	b := make([]byte, 1+r.rng.IntN(limit))
	for i := range b {
		b[i] = byte('a' + r.rng.IntN(26))
	}
	return b
}

// randSize returns a write or truncate size: mostly small, sometimes past
// the spilling configuration's per-file limit.
func (r *runner) randSize() int {
	switch r.rng.IntN(8) {
	case 0:
		return 1500 + r.rng.IntN(3000)
	case 1, 2:
		return 200 + r.rng.IntN(1000)
	}
	return 1 + r.rng.IntN(64)
}

// randOffset returns a write offset in or around a file of size n: its
// start, inside it, its end, or past it (a sparse write).
func (r *runner) randOffset(n int) int64 {
	switch r.rng.IntN(4) {
	case 0:
		return 0
	case 1:
		return int64(r.rng.IntN(n + 1))
	case 2:
		return int64(n)
	}
	return int64(n + 1 + r.rng.IntN(3000))
}

// Mode choices: files may lose write access; directories keep owner rwx,
// which the overlay's markers and the host's checks need.
var (
	modelFileModes   = []os.FileMode{0o644, 0o600, 0o444, 0o640, 0o755}
	modelDirModes    = []os.FileMode{0o755, 0o700, 0o750}
	modelCreateModes = []os.FileMode{0o644, 0o600, 0o444, 0o666, 0o755}
)

// checkHandleID checks a new handle's identity against its model node; with
// none (the model expects the open to fail), the caller's expect reports it.
func (r *runner) checkHandleID(desc string, h billy.File, n *mNode) error {
	if n == nil {
		return nil
	}
	if hi, ok := h.(ident.Identifier); ok {
		if err := r.ids.observe(n.id, hi.Identity()); err != nil {
			return fmt.Errorf("%s: handle: %w", desc, err)
		}
	}
	return nil
}

// addHeld keeps a new handle, closing the oldest beyond eight.
func (r *runner) addHeld(hf *heldFile) error {
	r.held = append(r.held, hf)
	if len(r.held) > 8 {
		return r.closeHeld(0)
	}
	return nil
}

// closeHeld closes held handle i.
func (r *runner) closeHeld(i int) error {
	hf := r.held[i]
	r.held = slices.Delete(r.held, i, i+1)
	return r.expect(fmt.Sprintf("close %q", hf.path), "", hf.h.Close())
}

// closeAll closes every held handle.
func (r *runner) closeAll() error {
	for len(r.held) > 0 {
		if err := r.closeHeld(len(r.held) - 1); err != nil {
			return err
		}
	}
	return nil
}

// flagNames renders open flags for the history.
func flagNames(flag int) string {
	var out []string
	switch {
	case flag&os.O_RDWR != 0:
		out = append(out, "RDWR")
	case flag&os.O_WRONLY != 0:
		out = append(out, "WRONLY")
	default:
		out = append(out, "RDONLY")
	}
	for _, f := range []struct {
		bit  int
		name string
	}{{os.O_CREATE, "CREATE"}, {os.O_EXCL, "EXCL"}, {os.O_TRUNC, "TRUNC"}, {os.O_APPEND, "APPEND"}} {
		if flag&f.bit != 0 {
			out = append(out, f.name)
		}
	}
	return strings.Join(out, "|")
}

// runStep generates one operation, applies it to the model and the overlay
// and compares the outcomes.
func (r *runner) runStep() error {
	r.step++
	f, m := r.f, r.m
	switch op := r.rng.IntN(100); {
	case op < 10: // write a whole file through a fresh handle
		p := r.heldPath()
		flag := os.O_WRONLY | os.O_CREATE | pick(r, []int{0, os.O_TRUNC, os.O_TRUNC, os.O_APPEND, os.O_EXCL, os.O_TRUNC | os.O_EXCL})
		perm := pick(r, modelCreateModes)
		data := r.randData(r.randSize())
		desc := fmt.Sprintf("writefile %q %s %v %d bytes", p, flagNames(flag), perm, len(data))
		n, _, cls := m.openFile(p, flag, perm)
		if cls == clUndef {
			r.skip(desc)
			return nil
		}
		if cls == "" {
			off := int64(0)
			if flag&os.O_APPEND != 0 {
				off = int64(len(n.data))
			}
			n.writeAt(data, off)
		}
		h, err := f.OpenFile(p, flag, perm)
		if err == nil {
			if err := r.checkHandleID(desc, h, n); err != nil {
				return err
			}
			_, werr := h.Write(data)
			err = errors.Join(werr, h.Close())
		}
		return r.expect(desc, cls, err)
	case op < 19: // open and keep a handle
		p := r.heldPath()
		flag := pick(r, []int{os.O_RDONLY, os.O_RDONLY, os.O_RDWR, os.O_RDWR | os.O_CREATE, os.O_RDWR | os.O_CREATE | os.O_EXCL,
			os.O_RDWR | os.O_TRUNC, os.O_RDWR | os.O_CREATE | os.O_TRUNC, os.O_WRONLY | os.O_CREATE})
		perm := pick(r, modelCreateModes)
		desc := fmt.Sprintf("open %q %s %v", p, flagNames(flag), perm)
		n, snap, cls := m.openFile(p, flag, perm)
		if cls == clUndef {
			r.skip(desc)
			return nil
		}
		h, err := f.OpenFile(p, flag, perm)
		if err := r.expect(desc, cls, err); err != nil {
			if h != nil {
				h.Close()
			}
			return err
		}
		if h == nil {
			return nil
		}
		if err := r.checkHandleID(desc, h, n); err != nil {
			return err
		}
		return r.addHeld(&heldFile{h: h, n: n, snap: snap, writable: writing(flag), readable: flag&os.O_WRONLY == 0, path: p})
	case op < 28: // write through a held handle
		hf := r.pickHeld(func(hf *heldFile) bool { return hf.writable })
		if hf == nil {
			return nil
		}
		data := r.randData(r.randSize())
		off := r.randOffset(len(hf.n.data))
		hf.n.writeAt(data, off)
		desc := fmt.Sprintf("hwrite %q %d bytes at %d", hf.path, len(data), off)
		n, err := hf.h.(io.WriterAt).WriteAt(data, off)
		if err == nil && n != len(data) {
			err = fmt.Errorf("short write of %d bytes", n)
		}
		return r.expect(desc, "", err)
	case op < 33: // truncate through a held handle
		hf := r.pickHeld(func(hf *heldFile) bool { return hf.writable })
		if hf == nil {
			return nil
		}
		size := int64(r.rng.IntN(len(hf.n.data) + 1))
		if r.rng.IntN(2) == 0 {
			size = int64(len(hf.n.data) + r.rng.IntN(3000))
		}
		hf.n.truncate(size)
		return r.expect(fmt.Sprintf("htruncate %q to %d", hf.path, size), "", hf.h.Truncate(size))
	case op < 40: // read back through a held handle
		hf := r.pickHeld(func(hf *heldFile) bool { return hf.readable })
		if hf == nil {
			return nil
		}
		return r.readHeld(hf)
	case op < 44: // close a held handle
		if len(r.held) == 0 {
			return nil
		}
		return r.closeHeld(r.rng.IntN(len(r.held)))
	case op < 47: // truncate by name
		p := r.heldPath()
		size := int64(r.rng.IntN(r.randSize() + 1))
		desc := fmt.Sprintf("truncate %q to %d", p, size)
		n, _, cls := m.openFile(p, os.O_RDWR, 0)
		if cls == clUndef {
			r.skip(desc)
			return nil
		}
		if cls == "" {
			n.truncate(size)
		}
		h, err := f.OpenFile(p, os.O_RDWR, 0)
		if err == nil {
			err = errors.Join(h.Truncate(size), h.Close())
		}
		return r.expect(desc, cls, err)
	case op < 52:
		p := r.randPath()
		perm := pick(r, modelDirModes)
		desc := fmt.Sprintf("mkdirall %q %v", p, perm)
		cls := m.mkdirAll(p, perm)
		if cls == clUndef {
			r.skip(desc)
			return nil
		}
		return r.expect(desc, cls, f.MkdirAll(p, perm))
	case op < 60:
		p := r.heldPath()
		desc := fmt.Sprintf("remove %q", p)
		cls := m.remove(p)
		if cls == clUndef {
			r.skip(desc)
			return nil
		}
		return r.expect(desc, cls, f.Remove(p))
	case op < 70:
		p := r.heldPath()
		var q string
		switch r.rng.IntN(3) {
		case 0:
			dir := mParent(p)
			q = path.Join(dir, r.sp.prefix[dir]+pick(r, r.sp.leaves))
		default:
			q = r.randPath()
		}
		if r.sp.fixed[q] || r.sp.own != nil && !r.sp.own(q) {
			q = r.randPath()
		}
		desc := fmt.Sprintf("rename %q %q", p, q)
		cls := m.rename(p, q)
		if cls == clUndef {
			r.skip(desc)
			return nil
		}
		return r.expect(desc, cls, f.Rename(p, q))
	case op < 74:
		p, q := r.heldPath(), r.randPath()
		if files := r.existingKind(mFile); len(files) > 0 && r.rng.IntN(3) != 0 {
			p = pick(r, files)
		}
		if dirs := r.existingKind(mDir); len(dirs) > 0 && r.rng.IntN(2) == 0 {
			dir := pick(r, dirs)
			if q = path.Join(dir, r.sp.prefix[dir]+pick(r, r.sp.leaves)); r.sp.fixed[q] || r.sp.own != nil && !r.sp.own(q) {
				q = r.randPath()
			}
		}
		desc := fmt.Sprintf("link %q %q", p, q)
		cls := m.link(p, q)
		if cls == clUndef {
			r.skip(desc)
			return nil
		}
		return r.expect(desc, cls, f.Link(p, q))
	case op < 78:
		target, p := pick(r, r.sp.targets), r.randPath()
		desc := fmt.Sprintf("symlink %q -> %q", p, target)
		cls := m.symlink(target, p)
		if cls == clUndef {
			r.skip(desc)
			return nil
		}
		return r.expect(desc, cls, f.Symlink(target, p))
	case op < 82:
		p := r.heldPath()
		perm := pick(r, modelFileModes)
		if _, _, n, cls := m.walk(p); cls == "" && n.kind == mDir {
			perm = pick(r, modelDirModes)
		}
		desc := fmt.Sprintf("chmod %q %v", p, perm)
		cls := m.chmod(p, perm)
		if cls == clUndef {
			r.skip(desc)
			return nil
		}
		return r.expect(desc, cls, f.Chmod(p, perm))
	case op < 86:
		p := r.heldPath()
		stamp := past.Add(time.Duration(r.step) * time.Second)
		desc := fmt.Sprintf("chtimes %q %d", p, r.step)
		cls := m.chtimes(p, stamp)
		if cls == clUndef {
			r.skip(desc)
			return nil
		}
		return r.expect(desc, cls, f.Chtimes(p, stamp, stamp))
	case op < 90:
		return r.readDir(pick(r, append(slices.Clone(r.sp.dirs), r.randPath())))
	case op < 95:
		return r.lstat(r.randPath(), false)
	case op < 98:
		return r.lstat(r.randPath(), true)
	default:
		p := r.randPath()
		desc := fmt.Sprintf("readlink %q", p)
		target, cls := m.readlink(p)
		if cls == clUndef {
			r.skip(desc)
			return nil
		}
		got, err := f.Readlink(p)
		if err := r.expect(desc, cls, err); err != nil {
			return err
		}
		if err == nil && got != target {
			return fmt.Errorf("%s = %q, model %q", desc, got, target)
		}
		return nil
	}
}

// pickHeld returns a random held handle that ok accepts, or nil.
func (r *runner) pickHeld(ok func(*heldFile) bool) *heldFile {
	var match []*heldFile
	for _, hf := range r.held {
		if ok(hf) {
			match = append(match, hf)
		}
	}
	if len(match) == 0 {
		return nil
	}
	return pick(r, match)
}

// readHeld reads a held handle's whole contents and size and compares them
// with its node, or with the lower bytes a read-only lower handle keeps.
func (r *runner) readHeld(hf *heldFile) error {
	want := hf.n.data
	if hf.snap != nil {
		want = hf.snap
	}
	desc := fmt.Sprintf("hread %q", hf.path)
	got, err := readAllAt(hf.h)
	if err := r.expect(desc, "", err); err != nil {
		return err
	}
	if string(got) != string(want) {
		return fmt.Errorf("%s: handle reads %s, model %s; %s", desc, describeBytes(got), describeBytes(want), firstDiff(got, want))
	}
	if st, ok := hf.h.(interface{ Stat() (os.FileInfo, error) }); ok {
		info, err := st.Stat()
		if err != nil {
			return fmt.Errorf("%s: Stat: %v", desc, err)
		}
		if info.Size() != int64(len(want)) {
			return fmt.Errorf("%s: handle Stat size %d, model %d", desc, info.Size(), len(want))
		}
	}
	return nil
}

// readDir compares one directory listing and its entries with the model.
func (r *runner) readDir(p string) error {
	desc := fmt.Sprintf("readdir %q", p)
	if r.knownFinding(findingReadDirBelowFile, desc, r.m.belowFile(p)) {
		return nil
	}
	dir, cls := r.m.readDir(p)
	if cls == clUndef {
		r.skip(desc)
		return nil
	}
	infos, err := r.f.ReadDir(p)
	if err := r.expect(desc, cls, err); err != nil || dir == nil {
		return err
	}
	keep := func(name string) bool {
		q := path.Join(p, name)
		return r.sp.own == nil || r.sp.own(q) || r.sp.fixed[q]
	}
	var got, want []string
	for _, info := range infos {
		if keep(info.Name()) {
			got = append(got, info.Name())
		}
	}
	for _, name := range sortedNames(dir) {
		if keep(name) {
			want = append(want, name)
		}
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("%s = %q, model %q", desc, got, want)
	}
	for _, info := range infos {
		if keep(info.Name()) {
			if err := infoDiff(path.Join(p, info.Name()), info, dir.kids[info.Name()], r.ids); err != nil {
				return fmt.Errorf("%s: %w", desc, err)
			}
		}
	}
	return nil
}

// lstat compares Lstat (or Stat, following symlinks) of p with the model.
func (r *runner) lstat(p string, follow bool) error {
	desc := fmt.Sprintf("lstat %q", p)
	var n *mNode
	var cls string
	if follow {
		desc = fmt.Sprintf("stat %q", p)
		_, _, _, n, cls = r.m.follow(p)
	} else {
		_, _, n, cls = r.m.walk(p)
	}
	if cls == clUndef {
		r.skip(desc)
		return nil
	}
	stat := r.f.Lstat
	if follow {
		stat = r.f.Stat
	}
	info, err := stat(p)
	if err := r.expect(desc, cls, err); err != nil || n == nil {
		return err
	}
	if err := infoDiff(p, info, n, r.ids); err != nil {
		return fmt.Errorf("%s: %w", desc, err)
	}
	return nil
}

// checkBudget fails when memory files exceed their budget.
func (r *runner) checkBudget() error {
	if r.mem == nil {
		return nil
	}
	if used, limit := r.mem.Usage(); used > limit {
		return fmt.Errorf("%d bytes in memory exceed the %d budget", used, limit)
	}
	return nil
}

// modelConfig is one memory configuration of the overlay under test.
type modelConfig struct {
	name string
	mem  func() *Memory
}

// modelConfigs are the memory tiers every run is repeated with.
var modelConfigs = []modelConfig{
	{"memory=off", func() *Memory { return nil }},
	// A tiny budget, per-file limit and idle age: files spill by size, by
	// eviction and by the background writeback all through the run.
	{"memory=spill", func() *Memory { return NewMemory(8<<10, 2<<10, time.Millisecond) }},
	// Room for everything: most files never reach the disk before Flush.
	{"memory=roomy", func() *Memory { return NewMemory(64<<20, 256<<10, 0) }},
}

// sequentialLevels is the lower stack of the sequential runs: a bottom and
// two frozen layers with whiteouts, an opaque directory, type replacements,
// a recreated directory, symlinks and a three-name hard link.
func sequentialLevels() []map[string]lowerEntry {
	bottom := map[string]lowerEntry{
		"a": fileE("bottom a\n", 0o444), "b": fileE("bottom b", 0o644),
		"d1": dirE(), "d1/a": fileE("d1 a", 0o600), "d1/c": fileE("d1 c", 0o644), "d1/s": linkE("../a"),
		"d1/sub": dirE(), "d1/sub/x": fileE("sub x", 0o644),
		"d2": dirE(), "d2/x": fileE("d2 x", 0o644), "d2/sub": dirE(), "d2/sub/y": fileE("d2 y", 0o644),
		"e": dirE(), "e/old": fileE("old", 0o644),
		"d4": dirE(), "d4/k": fileE("k", 0o644),
		"s": linkE("d1"), "x": linkE("missing"),
	}
	low := map[string]lowerEntry{
		"b":  fileE("layer one b", 0o644),
		"d1": dirE(), "d1/c": whE(), "d1/n": fileE("d1 n", 0o640),
		"d2": opqE(), "d2/z": fileE("d2 z", 0o644),
		"d3": dirE(), "d3/h1": fileE("linked", 0o644), "d3/h2": hardE("d3/h1"), "h1": hardE("d3/h1"),
		"e":   whE(),
		"d4":  fileE("d4 is a file now", 0o644),
		"big": fileE(strings.Repeat("0123456789abcdef", 200), 0o644),
	}
	top := map[string]lowerEntry{
		"d1": dirE(), "d1/sub": dirE(), "d1/sub/x": whE(), "d1/sub/w": fileE("w", 0o644),
		"e": dirE(), "e/f": fileE("f", 0o644),
		"n": fileE("top n", 0o644), "c": linkE("b"),
		"d3": dirE(), "d3/h2": whE(),
	}
	return []map[string]lowerEntry{bottom, low, top}
}

// seqRun is one sequential run: a stream over an overlay whose upper it
// flushes, reopens, cools, freezes and copies as after a crash at random
// points.
type seqRun struct {
	t      *testing.T
	r      *runner
	stack  *modelStack
	mem    *Memory
	lowers map[string][]string
	counts map[string]int
	// history holds, for every path, each state it had since the last Flush
	// ("absent", "dir", "symlink <target>", "node <id>" for a file), and
	// contents each file node's contents since then, see [seqRun.crash].
	history  map[string]map[string]bool
	contents map[int64]map[string]bool
}

// nodeState describes what a crash copy must show of a node at a name: its
// kind, a symlink's target, a file's model identity (its contents are the
// node's, [seqRun.contents]).
func nodeState(n *mNode) string {
	switch n.kind {
	case mDir:
		return "dir"
	case mLink:
		return "symlink " + n.target
	}
	return fmt.Sprintf("node %d", n.id)
}

// remember adds the model's current state of every path, and every file's
// contents, to the history; a path seen for the first time was absent at
// the last Flush.
func (s *seqRun) remember() {
	seen := make(map[string]bool)
	for _, p := range s.r.m.paths() {
		_, _, n, _ := s.r.m.walk(p)
		if s.history[p] == nil {
			s.history[p] = map[string]bool{"absent": true}
		}
		s.history[p][nodeState(n)] = true
		if n.kind == mFile {
			if s.contents[n.id] == nil {
				s.contents[n.id] = make(map[string]bool)
			}
			s.contents[n.id][fmt.Sprintf("%x", n.data)] = true
		}
		seen[p] = true
	}
	for p, h := range s.history {
		if !seen[p] {
			h["absent"] = true
		}
	}
}

// forget starts the history afresh after a Flush made the upper on disk
// complete.
func (s *seqRun) forget() {
	s.history = make(map[string]map[string]bool)
	s.contents = make(map[int64]map[string]bool)
	s.remember()
	for _, h := range s.history {
		delete(h, "absent")
	}
}

// crash is checkpoint (d): the upper as it is on disk now, with host changes
// still queued (queue.go) and files still in memory, is copied as a crash
// would leave it, and an overlay opened on the copy must load and show
// every name in a state it had since the last Flush. Names are ordered,
// contents are not: a file shows contents its file had at some point since
// then, not necessarily while it held the name, as data written through a
// descriptor reaches the inode whatever the queue did with its names.
func (s *seqRun) crash() error {
	s.counts["crash"]++
	r := s.r
	r.record("checkpoint: crash copy")
	dir := filepath.Join(s.t.TempDir(), "crashed")
	release := holdApplier(r.f)
	copyTree(s.t, r.f.Upper(), dir)
	release()
	g, err := New(dir, r.f.Stack(), s.stack.bottom, nil)
	if err != nil {
		return fmt.Errorf("crash copy does not load: %v", err)
	}
	shown := make(map[string]string)
	// Names showing one identity must be one file on disk, not copies.
	inodes := make(map[ident.ID]inodeKey)
	var walk func(string) error
	walk = func(d string) error {
		infos, err := g.ReadDir(d)
		if err != nil {
			return fmt.Errorf("crash copy: ReadDir(%q): %v", d, err)
		}
		for _, info := range infos {
			p := path.Join(d, info.Name())
			switch kindOf(info.Mode()) {
			case mDir:
				shown[p] = "dir"
				if err := walk(p); err != nil {
					return err
				}
			case mLink:
				target, err := g.Readlink(p)
				if err != nil {
					return fmt.Errorf("crash copy: Readlink(%q): %v", p, err)
				}
				shown[p] = "symlink " + target
			default:
				if info.Mode().Perm()&0o400 == 0 && !r.m.superuser {
					shown[p] = "unreadable"
					continue
				}
				data, err := readWhole(g, p)
				if err != nil {
					return fmt.Errorf("crash copy: read %q: %v", p, err)
				}
				shown[p] = fmt.Sprintf("file %x", data)
				id, _ := ident.Of(info)
				if key, ok := hostKey(info); ok {
					if seen, ok := inodes[id]; ok && seen != key {
						return fmt.Errorf("crash copy: %q shows identity %v of another file on disk", p, id)
					}
					inodes[id] = key
				}
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return err
	}
	for p, state := range shown {
		if s.history[p] == nil {
			return fmt.Errorf("crash copy shows %q, a name that did not exist since the last Flush", p)
		}
		if s.history[p][state] {
			continue
		}
		if !strings.HasPrefix(state, "file ") && state != "unreadable" {
			return fmt.Errorf("crash copy shows %q as %.60s, a state it did not have since the last Flush", p, state)
		}
		ok := false
		for h := range s.history[p] {
			var id int64
			if _, err := fmt.Sscanf(h, "node %d", &id); err == nil && (state == "unreadable" || s.contents[id][strings.TrimPrefix(state, "file ")]) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("crash copy shows %q as %.60s, contents no file it held had since the last Flush", p, state)
		}
	}
	for p, h := range s.history {
		if _, ok := shown[p]; !ok && !h["absent"] {
			return fmt.Errorf("crash copy lacks %q, which existed since the last Flush", p)
		}
	}
	return nil
}

// diff compares the whole view of f with the model.
func (s *seqRun) diff(f *FS, label string) error {
	if err := viewDiff(f, s.r.m, s.r.ids, s.r.scope(nil)); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// flushReopen is checkpoint (a): after Flush a second overlay over the same
// upper and lower stack, without memory, must show the same tree.
func (s *seqRun) flushReopen() error {
	s.counts["reopen"]++
	r := s.r
	if err := r.f.Flush(); err != nil {
		return fmt.Errorf("Flush: %v", err)
	}
	r.record("checkpoint: flush and reopen")
	s.forget()
	r.forgetRootMtime("reopen")
	g, err := New(r.f.Upper(), r.f.Stack(), s.stack.bottom, nil)
	if err != nil {
		return fmt.Errorf("reopen: %v", err)
	}
	return s.diff(g, "reopened upper")
}

// cool is checkpoint (b): the upper index unloads, as the registry's idle
// check unloads it, and the tree must read the same cold and reloaded.
// Open handles pin the index, so half the time they are closed first.
func (s *seqRun) cool() error {
	r := s.r
	if r.rng.IntN(2) == 0 {
		if err := r.closeAll(); err != nil {
			return err
		}
	}
	if err := r.f.Flush(); err != nil {
		return fmt.Errorf("Flush: %v", err)
	}
	r.record(fmt.Sprintf("checkpoint: cool with %d handles held", len(r.held)))
	s.forget()
	// A writeback that picked a file before the Flush can still hold the
	// staging lock Cool only tries for.
	for range 200 {
		if r.f.Cool(time.Now().Add(time.Hour)); !indexLoaded(r.f) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if indexLoaded(r.f) {
		if len(r.held) == 0 {
			return errors.New("the upper index stays loaded with no handle open and nothing in memory")
		}
	} else {
		s.counts["cooled"]++
	}
	if err := s.diff(r.f, "cold index"); err != nil {
		return err
	}
	return nil
}

// freeze is checkpoint (c): the flushed upper becomes a frozen layer and a
// fresh upper continues on top of it, with the same memory budget.
func (s *seqRun) freeze() error {
	s.counts["freeze"]++
	r, t := s.r, s.t
	if err := r.closeAll(); err != nil {
		return err
	}
	if err := r.f.Flush(); err != nil {
		return fmt.Errorf("Flush: %v", err)
	}
	r.record("checkpoint: freeze")
	r.forgetRootMtime("freeze")
	stack := r.f.Stack()
	dir := filepath.Join(t.TempDir(), "frozen")
	if err := os.Rename(r.f.Upper(), dir); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(dir, workName)); err != nil {
		return err
	}
	l, err := r.f.Freeze(dir)
	if err != nil {
		return fmt.Errorf("Freeze: %v", err)
	}
	s.lowers[dir] = snapshot(t, dir)
	f, err := New(filepath.Join(t.TempDir(), "upper"), append([]*Layer{l}, stack...), s.stack.bottom, s.mem)
	if err != nil {
		return fmt.Errorf("New over the frozen layer: %v", err)
	}
	r.f = f
	r.m.freeze()
	s.forget()
	return s.diff(f, "after freeze")
}

// run applies ops operations with full comparisons every 25 and random
// checkpoints, then runs every checkpoint once more at the end.
func (s *seqRun) run(ops int) error {
	r := s.r
	if err := s.diff(r.f, "initial view"); err != nil {
		return err
	}
	s.forget()
	for range ops {
		if err := r.runStep(); err != nil {
			return err
		}
		s.remember()
		if err := r.checkBudget(); err != nil {
			return err
		}
		var err error
		switch x := r.rng.IntN(1000); {
		case x < 8:
			err = s.flushReopen()
		case x < 16:
			err = s.cool()
		case x < 21:
			err = s.freeze()
		case x < 27:
			err = s.crash()
		case r.step%25 == 0:
			err = s.diff(r.f, "periodic comparison")
		}
		if err != nil {
			return err
		}
	}
	if err := s.cool(); err != nil {
		return err
	}
	if err := s.freeze(); err != nil {
		return err
	}
	if err := s.flushReopen(); err != nil {
		return err
	}
	return r.closeAll()
}

// TestModelSequential drives one random operation stream through the
// overlay and the reference model in each memory configuration, failing on
// the first difference in a result, a handle's contents or the whole view.
func TestModelSequential(t *testing.T) {
	skipModelOnWindows(t)
	seed, ops := modelSeed(t), modelOps(t)
	for _, cfg := range modelConfigs {
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()
			var ids atomic.Int64
			stack := newModelStack(t, &ids, sequentialLevels()...)
			lowers := lowerSnapshots(t, stack.dirs)
			mem := cfg.mem()
			f, err := New(filepath.Join(t.TempDir(), "upper"), stack.layers, stack.bottom, mem)
			must(t, err)
			s := &seqRun{t: t, stack: stack, mem: mem, lowers: lowers, counts: make(map[string]int),
				r: newRunner(cfg.name, seed, 0, f, stack.m, newIDTracker(), mem, sequentialSpace())}
			if err := s.run(ops); err != nil {
				t.Fatal(s.r.report(t, err))
			}
			assertLowersUnchanged(t, s.lowers)
			if s.counts["cooled"] == 0 {
				t.Errorf("the upper index never cooled")
			}
			t.Logf("seed %d, %d operations, checkpoints %v, results %v", seed, ops, s.counts, s.r.stats)
		})
	}
}

// concurrentLevels is the lower stack of the concurrent run: per stream k a
// subtree gk and gk- names in the shared directory, with whiteouts, an
// opaque directory and a hard link between the two.
func concurrentLevels(streams int) []map[string]lowerEntry {
	bottom := map[string]lowerEntry{"shared": dirE()}
	low := map[string]lowerEntry{"shared": dirE()}
	for k := range streams {
		g := fmt.Sprintf("g%d", k)
		maps := map[string]lowerEntry{
			g: dirE(), g + "/a": fileE(g+" a", 0o644), g + "/b": fileE(g+" b", 0o600),
			g + "/d": dirE(), g + "/d/x": fileE("x", 0o644), g + "/d/sub": dirE(), g + "/d/sub/y": fileE("y", 0o644),
			g + "/e": dirE(), g + "/e/o": fileE("o", 0o644), g + "/s": linkE(g + "-a"), "shared/" + g + "-l": linkE(g + "-d"),
			"shared/" + g + "-a": fileE("shared a", 0o644), "shared/" + g + "-d": dirE(), "shared/" + g + "-d/f": fileE("f", 0o644),
		}
		maps0.Copy(bottom, maps)
		layer := map[string]lowerEntry{
			g: dirE(), g + "/b": whE(), g + "/d": dirE(), g + "/d/n": fileE("n", 0o644),
			g + "/e": opqE(), g + "/e/z": fileE("z", 0o644),
			g + "/h": fileE("linked", 0o644), "shared/" + g + "-h": hardE(g + "/h"),
			"shared/" + g + "-n": fileE("n", 0o644),
		}
		if k%2 == 0 {
			layer["shared/"+g+"-a"] = whE()
		}
		maps0.Copy(low, layer)
	}
	return []map[string]lowerEntry{bottom, low}
}

// TestModelConcurrent runs independent operation streams at once, each in
// its own subtree and its own names of one shared directory, with a model
// per stream; the merged models must match the final tree, before and after
// Flush and a reopen.
func TestModelConcurrent(t *testing.T) {
	skipModelOnWindows(t)
	seed, ops := modelSeed(t), modelOps(t)
	const streams = 4
	for _, cfg := range []modelConfig{modelConfigs[0], modelConfigs[1]} {
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()
			var ids atomic.Int64
			stack := newModelStack(t, &ids, concurrentLevels(streams)...)
			lowers := lowerSnapshots(t, stack.dirs)
			mem := cfg.mem()
			f, err := New(filepath.Join(t.TempDir(), "upper"), stack.layers, stack.bottom, mem)
			must(t, err)
			tracker := newIDTracker()
			if err := viewDiff(f, stack.m, tracker, viewScope{}); err != nil {
				t.Fatalf("initial view: %v", err)
			}
			// Other streams change the root and the shared directory, so
			// their mtimes are unpredictable to every stream.
			stack.m.root.mtime, stack.m.root.kids["shared"].mtime = time.Time{}, time.Time{}
			runners := make([]*runner, streams)
			for k := range runners {
				runners[k] = newRunner(fmt.Sprintf("%s stream %d", cfg.name, k), seed, uint64(k+1), f, stack.m.clone(), tracker, mem, workerSpace(k))
			}
			errs := make([]error, streams)
			var wg sync.WaitGroup
			for k, r := range runners {
				wg.Go(func() {
					for range ops / 2 {
						if err := r.runStep(); err != nil {
							errs[k] = err
							return
						}
						if r.step%50 == 0 {
							if err := viewDiff(f, r.m, tracker, r.scope(func(p string) bool { return r.sp.own(p) || p == "shared" })); err != nil {
								errs[k] = fmt.Errorf("own subtree: %w", err)
								return
							}
						}
					}
					errs[k] = r.closeAll()
				})
			}
			wg.Wait()
			for k, err := range errs {
				if err != nil {
					t.Fatal(runners[k].report(t, err))
				}
			}
			merged := mergeModels(stack.m, runners)
			if err := viewDiff(f, merged, tracker, runners[0].scope(nil)); err != nil {
				t.Fatalf("seed %d: merged view: %v", seed, err)
			}
			must(t, f.Flush())
			g, err := New(f.Upper(), f.Stack(), stack.bottom, nil)
			must(t, err)
			if err := viewDiff(g, merged, tracker, viewScope{}); err != nil {
				t.Fatalf("seed %d: reopened upper: %v", seed, err)
			}
			assertLowersUnchanged(t, lowers)
		})
	}
}

// mergeModels assembles the final model of a concurrent run: each stream's
// subtree and its names in the shared directory, under the initial root.
func mergeModels(initial *model, runners []*runner) *model {
	m := *initial
	root, shared := *initial.root, *initial.root.kids["shared"]
	root.kids, shared.kids = map[string]*mNode{"shared": &shared}, make(map[string]*mNode)
	for _, r := range runners {
		for name, n := range r.m.root.kids {
			if r.sp.own(name) {
				root.kids[name] = n
			}
		}
		for name, n := range r.m.root.kids["shared"].kids {
			if r.sp.own("shared/" + name) {
				shared.kids[name] = n
			}
		}
	}
	m.root = &root
	return &m
}

// historyTail is how many recent operations a failure report shows:
// OVERLAY_MODEL_HISTORY, or 40.
func historyTail() int {
	if n, err := strconv.Atoi(os.Getenv("OVERLAY_MODEL_HISTORY")); err == nil && n > 0 {
		return n
	}
	return 40
}

// TestModelDeniedOpenKeepsIdentity: a refused write-open of one name of a
// frozen, read-only hard link changes nothing, so the name keeps its
// identity (it is not copied up apart from its other name).
func TestModelDeniedOpenKeepsIdentity(t *testing.T) {
	skipModelOnWindows(t)
	if os.Geteuid() == 0 {
		t.Skip("root may write a read-only file")
	}
	memories(t, func(t *testing.T, mem func() *Memory) {
		var ids atomic.Int64
		stack := newModelStack(t, &ids, map[string]lowerEntry{"keep": fileE("keep", 0o644)},
			map[string]lowerEntry{"a": fileE("linked", 0o444), "b": hardE("a")})
		f, err := New(filepath.Join(t.TempDir(), "upper"), stack.layers, stack.bottom, mem())
		must(t, err)
		before := idOf(t, f, "a")
		if _, err := f.OpenFile("a", os.O_RDWR, 0); modelClass(err) != clAcces {
			t.Fatalf("OpenFile(a, O_RDWR) = %v, want EACCES", err)
		}
		if after := idOf(t, f, "a"); after != before {
			t.Fatalf("the refused open changed a's identity from %#x to %#x", before, after)
		}
	})
}
