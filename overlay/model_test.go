package overlay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// The reference model below is an in-memory POSIX tree written for the
// differential tests in model_run_test.go. It knows nothing of layers,
// uppers, memory files or indexes, except where the overlay deliberately
// differs from POSIX; each such rule cites its source.

// Result classes an operation can end in, shared by the model and the overlay.
const (
	clOK       = "ok"
	clNoent    = "noent"
	clExist    = "exist"
	clNotdir   = "notdir"
	clIsdir    = "isdir"
	clNotempty = "notempty"
	clInval    = "inval"
	clPerm     = "perm"
	clAcces    = "acces"
	clBusy     = "busy"
	clLoop     = "loop"
	// clUndef marks an operation whose outcome the model does not define
	// (see [model.walk]); generators drop it before the overlay runs it.
	clUndef = "undefined"
)

// modelClass reduces an overlay error to a result class.
func modelClass(err error) string {
	if err == nil {
		return clOK
	}
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		switch errno {
		case syscall.ENOENT:
			return clNoent
		case syscall.EEXIST:
			return clExist
		case syscall.ENOTDIR:
			return clNotdir
		case syscall.EISDIR:
			return clIsdir
		case syscall.ENOTEMPTY:
			return clNotempty
		case syscall.EINVAL:
			return clInval
		case syscall.EPERM:
			return clPerm
		case syscall.EACCES:
			return clAcces
		case syscall.EBUSY:
			return clBusy
		case syscall.ELOOP:
			return clLoop
		}
		return "errno " + errno.Error()
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return clNoent
	case errors.Is(err, fs.ErrExist):
		return clExist
	}
	return "error " + err.Error()
}

// mKind is a model node's type.
type mKind uint8

// Model node kinds.
const (
	mDir mKind = iota + 1
	mFile
	mLink
)

// String names a kind in difference reports.
func (k mKind) String() string {
	switch k {
	case mDir:
		return "dir"
	case mFile:
		return "file"
	case mLink:
		return "symlink"
	}
	return "?"
}

// kindOf maps a file mode to a model kind; 0 for anything else.
func kindOf(mode os.FileMode) mKind {
	switch {
	case mode.IsDir():
		return mDir
	case mode.IsRegular():
		return mFile
	case mode&os.ModeSymlink != 0:
		return mLink
	}
	return 0
}

// modeBits are the mode bits the model tracks for files and directories.
const modeBits = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky

// mNode is one model inode: a directory, a regular file or a symlink. Hard
// links share one node.
type mNode struct {
	id     int64 // model identity, unique across every model of a run
	kind   mKind
	perm   os.FileMode       // mode bits (modeBits); not tracked for symlinks, whose bits are the host's
	data   []byte            // file contents
	target string            // symlink target
	kids   map[string]*mNode // directory entries
	mtime  time.Time         // zero: not predictable (the host's clock set it)
	names  int               // visible names of the node
	// lower is set while the node's data is served by an immutable level
	// (a frozen layer or the bottom) and was never copied up.
	lower bool
	// shared marks a lower file whose frozen-layer host file has several
	// links, so copying up one name splits it off, see [model.copyUp].
	shared bool
}

// model is the reference tree and the host facts it depends on.
type model struct {
	root      *mNode
	ids       *atomic.Int64 // shared by every model of a run, so node ids never collide
	umask     os.FileMode   // the process creation mask, which the overlay applies like the host
	superuser bool          // root skips permission checks, as the kernel does
}

// newNode allocates a node with a fresh model identity.
func (m *model) newNode(kind mKind) *mNode {
	n := &mNode{id: m.ids.Add(1), kind: kind, names: 1}
	if kind == mDir {
		n.kids = make(map[string]*mNode)
	}
	return n
}

// mClean canonicalizes a slash path relative to the root, as POSIX resolves
// ".." at the root to the root.
func mClean(p string) string { return strings.TrimPrefix(path.Clean("/"+p), "/") }

// mParent returns the directory part of a clean path ("" for the root's entries).
func mParent(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// walk resolves p without following a final symlink, returning the parent
// directory node, the final name and the node, or a class.
//
// Rules beyond POSIX:
//   - A path below a regular file is missing (ENOENT), not ENOTDIR: a
//     non-directory ancestor hides the subtree (overlay.go `missing`;
//     layers_test.go TestThreeLayerMerge expects "block/child" gone).
//   - A path through a symlink is undefined: the overlay is "not a symlink
//     sandbox" (overlay.go package doc) and its bottom resolves intermediate
//     symlinks on the host, so generators never issue such paths.
func (m *model) walk(p string) (dir *mNode, name string, n *mNode, cls string) {
	n = m.root
	if p == "" {
		return nil, "", n, ""
	}
	for c := range strings.SplitSeq(p, "/") {
		switch n.kind {
		case mLink:
			return nil, "", nil, clUndef
		case mFile:
			return nil, "", nil, clNoent
		}
		dir, name = n, c
		if n = n.kids[c]; n == nil {
			return dir, name, nil, clNoent
		}
	}
	return dir, name, n, ""
}

// belowFile reports whether a proper ancestor of p is a regular file
// (reached without passing a symlink).
func (m *model) belowFile(p string) bool {
	n := m.root
	for c := range strings.SplitSeq(p, "/") {
		if n == nil || n.kind != mDir {
			return n != nil && n.kind == mFile
		}
		n = n.kids[c]
	}
	return false
}

// dirAt checks that new entries can go into d, as the overlay's parent
// check (ensureDir) does: missing parents are ENOENT, non-directories
// ENOTDIR. A symlink parent is undefined (see [model.walk]).
func (m *model) dirAt(d string) (*mNode, string) {
	_, _, n, cls := m.walk(d)
	if cls != "" {
		return nil, cls
	}
	switch n.kind {
	case mLink:
		return nil, clUndef
	case mFile:
		return nil, clNotdir
	}
	return n, ""
}

// follow resolves p and then its final symlinks, at most 40 of them.
// Targets are relative to the link's directory and stay inside the tree.
func (m *model) follow(p string) (dir *mNode, name, q string, n *mNode, cls string) {
	q = p
	dir, name, n, cls = m.walk(q)
	for links := 0; cls == "" && n.kind == mLink; links++ {
		if links == 40 {
			return nil, "", q, nil, clLoop
		}
		q = mClean(path.Join(mParent(q), n.target))
		dir, name, n, cls = m.walk(q)
	}
	return dir, name, q, n, cls
}

// copyUp models copying name in dir up before a change and returns the node
// the name then holds. A file of a frozen layer with several host links is
// copied on its own: the copy gets a new identity and its data, the other
// names keep the old node (identity_test.go TestHardLinkCopyLeavesIdentity;
// identity.go `inherited`). Anything else keeps its node.
func (m *model) copyUp(dir *mNode, name string) *mNode {
	n := dir.kids[name]
	if n.kind == mFile && n.lower && n.shared {
		c := m.newNode(mFile)
		c.perm, c.data, c.mtime = n.perm, slices.Clone(n.data), n.mtime
		n.names--
		dir.kids[name] = c
		return c
	}
	n.lower = false
	return n
}

// sortedNames lists a directory node's entries in name order.
func sortedNames(dir *mNode) []string {
	out := make([]string, 0, len(dir.kids))
	for name := range dir.kids {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// writing reports whether an open flag asks for write access.
func writing(flag int) bool {
	return flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0
}

// openFile models OpenFile. A successful open returns the node a writable or
// live handle works on; snap is non-nil (possibly empty) for a read-only
// handle of a lower file, which keeps reading the immutable level's bytes
// while later writes copy the name up: a lower file's content "never changes
// under f" and a copy-up "orders after this open" (overlay.go `openLower`).
func (m *model) openFile(p string, flag int, perm os.FileMode) (n *mNode, snap []byte, cls string) {
	write := writing(flag)
	_, _, _, cls = m.walk(p)
	if cls == clUndef {
		return nil, nil, cls
	}
	if cls == "" && write && flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0 {
		return nil, nil, clExist
	}
	dir, name, q, n, cls := m.follow(p)
	if cls == clUndef || cls == clLoop {
		return nil, nil, cls
	}
	if !write {
		if cls != "" {
			return nil, nil, cls
		}
		if n.kind == mDir {
			// Read-only opens of directories differ by level (an upper
			// directory opens, a bottom one is EISDIR): not modeled.
			return nil, nil, clUndef
		}
		if !m.superuser && n.perm&0o400 == 0 {
			return nil, nil, clAcces
		}
		if n.lower {
			return n, append([]byte{}, n.data...), ""
		}
		return n, nil, ""
	}
	if cls == "" {
		if n.kind == mDir {
			return nil, nil, clIsdir
		}
		// A refused open changes nothing: no copy-up, no truncation.
		if !m.superuser && n.perm&0o200 == 0 {
			return nil, nil, clAcces
		}
		n = m.copyUp(dir, name)
		if flag&os.O_TRUNC != 0 {
			n.data, n.mtime = nil, time.Time{}
		}
		return n, nil, ""
	}
	if flag&os.O_CREATE == 0 {
		return nil, nil, cls
	}
	d, dcls := m.dirAt(mParent(q))
	if dcls != "" {
		return nil, nil, dcls
	}
	n = m.newNode(mFile)
	n.perm = perm & modeBits &^ m.umask
	d.kids[path.Base(q)] = n
	d.mtime = time.Time{}
	return n, nil, ""
}

// mkdirAll models MkdirAll: existing directories are kept, the first
// non-directory prefix fails with ENOTDIR (layers_test.go
// TestLayerDeletionAndRecreation: MkdirAll below a file), and missing ones
// are created with perm less the umask.
func (m *model) mkdirAll(p string, perm os.FileMode) string {
	comps := strings.Split(p, "/")
	n := m.root
	i := 0
	for ; i < len(comps); i++ {
		kid := n.kids[comps[i]]
		if kid == nil {
			break
		}
		switch kid.kind {
		case mLink:
			// MkdirAll does not follow symlinks; os.MkdirAll does: not modeled.
			return clUndef
		case mFile:
			return clNotdir
		}
		n = kid
	}
	for ; i < len(comps); i++ {
		kid := m.newNode(mDir)
		kid.perm = perm & os.ModePerm &^ m.umask
		n.kids[comps[i]] = kid
		n.mtime = time.Time{}
		n = kid
	}
	return ""
}

// symlink models Symlink: the name must not exist, its parent must.
func (m *model) symlink(target, p string) string {
	_, _, _, cls := m.walk(p)
	switch cls {
	case "":
		return clExist
	case clUndef:
		return cls
	}
	d, cls := m.dirAt(mParent(p))
	if cls != "" {
		return cls
	}
	n := m.newNode(mLink)
	n.target = target
	d.kids[path.Base(p)] = n
	d.mtime = time.Time{}
	return ""
}

// remove models Remove: unlink of a file or symlink, rmdir of an empty
// directory. A removed file's node lives on in the handles that hold it.
func (m *model) remove(p string) string {
	if p == "" {
		return clBusy
	}
	dir, name, n, cls := m.walk(p)
	if cls != "" {
		return cls
	}
	if n.kind == mDir && len(n.kids) != 0 {
		return clNotempty
	}
	delete(dir.kids, name)
	n.names--
	dir.mtime = time.Time{}
	return ""
}

// link models Link. Hard links of symlinks are host-defined (macOS link(2)
// follows them), so they are not modeled.
func (m *model) link(src, dst string) string {
	if src == "" || dst == "" {
		return clPerm
	}
	sdir, sname, n, cls := m.walk(src)
	if cls != "" {
		return cls
	}
	switch n.kind {
	case mDir:
		return clPerm
	case mLink:
		return clUndef
	}
	_, _, _, cls = m.walk(dst)
	switch cls {
	case "":
		return clExist
	case clUndef:
		return cls
	}
	d, dcls := m.dirAt(mParent(dst))
	if dcls == clUndef {
		return dcls
	}
	// The source is copied up before the destination's parent is checked.
	n = m.copyUp(sdir, sname)
	if dcls != "" {
		return dcls
	}
	d.kids[path.Base(dst)] = n
	n.names++
	d.mtime = time.Time{}
	return ""
}

// rename models rename(2): onto itself or another name of the same file it
// does nothing; a directory moves only onto an empty directory or a missing
// name and never below itself; a non-directory never replaces a directory.
func (m *model) rename(src, dst string) string {
	if src == "" || dst == "" {
		return clPerm
	}
	sdir, sname, n, cls := m.walk(src)
	if cls != "" {
		return cls
	}
	ddir, dname, old, dcls := m.walk(dst)
	if dcls == clUndef {
		return dcls
	}
	if src == dst {
		return ""
	}
	if strings.HasPrefix(dst, src+"/") {
		return clInval
	}
	if dcls == "" && old == n {
		return ""
	}
	if dcls == "" {
		switch {
		case n.kind == mDir && old.kind != mDir:
			return clNotdir
		case n.kind != mDir && old.kind == mDir:
			return clIsdir
		case old.kind == mDir && len(old.kids) != 0:
			return clNotempty
		}
	}
	pd, pcls := m.dirAt(mParent(dst))
	if pcls == clUndef {
		return pcls
	}
	// The source is copied up, a directory without its tree (it redirects
	// there instead), before the destination's parent is checked.
	n = m.copyUp(sdir, sname)
	if pcls != "" {
		return pcls
	}
	if dcls == "" {
		delete(ddir.kids, dname)
		old.names--
	}
	delete(sdir.kids, sname)
	pd.kids[path.Base(dst)] = n
	sdir.mtime, pd.mtime = time.Time{}, time.Time{}
	return ""
}

// chmod models Chmod of a file or directory. Symlink modes are host-defined
// (Linux refuses lchmod), so chmod of a symlink is not modeled.
func (m *model) chmod(p string, perm os.FileMode) string {
	dir, name, n, cls := m.walk(p)
	if cls != "" {
		return cls
	}
	if n.kind == mLink {
		return clUndef
	}
	if dir != nil {
		n = m.copyUp(dir, name)
	}
	n.perm = perm & modeBits
	return ""
}

// chtimes models Chtimes, which sets a symlink's own times
// (overlay.go `Chtimes`).
func (m *model) chtimes(p string, t time.Time) string {
	dir, name, n, cls := m.walk(p)
	if cls != "" {
		return cls
	}
	if dir != nil {
		n = m.copyUp(dir, name)
	}
	n.mtime = t
	return ""
}

// readDir models ReadDir, which does not follow a final symlink
// (listing_fast_test.go TestReadDirBottomOnlyRejectsInvalidDirectories;
// [Bottom] ReadDir rejects final symlinks).
func (m *model) readDir(p string) (*mNode, string) {
	_, _, n, cls := m.walk(p)
	if cls != "" {
		return nil, cls
	}
	if n.kind != mDir {
		return nil, clNotdir
	}
	return n, ""
}

// readlink models Readlink of p.
func (m *model) readlink(p string) (string, string) {
	_, _, n, cls := m.walk(p)
	if cls != "" {
		return "", cls
	}
	if n.kind != mLink {
		return "", clInval
	}
	return n.target, ""
}

// writeAt models a positioned write through a handle: the file grows with
// zeros up to off when off is past its end.
func (n *mNode) writeAt(data []byte, off int64) {
	if end := off + int64(len(data)); end > int64(len(n.data)) {
		n.data = append(n.data, make([]byte, end-int64(len(n.data)))...)
	}
	copy(n.data[off:], data)
	n.mtime = time.Time{}
}

// truncate models ftruncate: shrinking drops bytes, growing adds zeros.
func (n *mNode) truncate(size int64) {
	if size <= int64(len(n.data)) {
		n.data = n.data[:size]
	} else {
		n.data = append(n.data, make([]byte, size-int64(len(n.data)))...)
	}
	n.mtime = time.Time{}
}

// freeze models freezing the upper into a layer: every visible node is
// served by an immutable level again, and an upper file with several names
// becomes a frozen file with several host links.
func (m *model) freeze() {
	var walk func(n *mNode)
	walk = func(n *mNode) {
		if n.kind == mFile && !n.lower && n.names > 1 {
			n.shared = true
		}
		n.lower = true
		for _, kid := range n.kids {
			walk(kid)
		}
	}
	walk(m.root)
}

// clone deep-copies the model, keeping hard links shared and node ids equal.
func (m *model) clone() *model {
	seen := make(map[*mNode]*mNode)
	var copyNode func(n *mNode) *mNode
	copyNode = func(n *mNode) *mNode {
		if c := seen[n]; c != nil {
			return c
		}
		c := *n
		c.data = slices.Clone(n.data)
		seen[n] = &c
		if n.kids != nil {
			c.kids = make(map[string]*mNode, len(n.kids))
			for name, kid := range n.kids {
				c.kids[name] = copyNode(kid)
			}
		}
		return &c
	}
	c := *m
	c.root = copyNode(m.root)
	return &c
}

// paths lists every visible path below the root in name order.
func (m *model) paths() []string {
	var out []string
	var walk func(p string, n *mNode)
	walk = func(p string, n *mNode) {
		for _, name := range sortedNames(n) {
			q := path.Join(p, name)
			out = append(out, q)
			if kid := n.kids[name]; kid.kind == mDir {
				walk(q, kid)
			}
		}
	}
	walk("", m.root)
	return out
}

// idTracker binds model nodes to overlay identities: a node's identity
// never changes (copy-up, spill, freeze and rename carry it, internals.md
// "File identity"), and no two nodes ever share one (a removed file's id
// "never reaches a new file").
type idTracker struct {
	mu     sync.Mutex
	byNode map[int64]ident.ID
	byID   map[ident.ID]int64
}

// newIDTracker returns an empty tracker.
func newIDTracker() *idTracker {
	return &idTracker{byNode: make(map[int64]ident.ID), byID: make(map[ident.ID]int64)}
}

// observe records that node shows identity id, describing a contradiction.
func (t *idTracker) observe(node int64, id ident.ID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == 0 {
		return fmt.Errorf("model node %d has no identity", node)
	}
	if old, ok := t.byNode[node]; ok && old != id {
		return fmt.Errorf("model node %d changed identity from %#x to %#x", node, old, id)
	}
	if other, ok := t.byID[id]; ok && other != node {
		return fmt.Errorf("identity %#x of model node %d is also shown by model node %d", id, other, node)
	}
	t.byNode[node], t.byID[id] = id, node
	return nil
}

// of returns the identity recorded for node.
func (t *idTracker) of(node int64) (ident.ID, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.byNode[node]
	return id, ok
}

// infoDiff compares what the overlay reports about one entry with node n.
func infoDiff(p string, info os.FileInfo, n *mNode, ids *idTracker) error {
	if k := kindOf(info.Mode()); k != n.kind {
		return fmt.Errorf("%q is a %v (mode %v), model has a %v", p, k, info.Mode(), n.kind)
	}
	if n.kind != mLink && info.Mode()&modeBits != n.perm {
		return fmt.Errorf("%q has mode bits %v, model %v", p, info.Mode()&modeBits, n.perm)
	}
	switch n.kind {
	case mFile:
		if info.Size() != int64(len(n.data)) {
			return fmt.Errorf("%q has size %d, model %d", p, info.Size(), len(n.data))
		}
		// The index counts an upper file's names, in memory or on disk.
		if got := hardLinks(info); !n.lower && runtime.GOOS != "windows" && got != uint64(n.names) {
			return fmt.Errorf("%q has %d links, model %d names", p, got, n.names)
		}
	case mLink:
		if info.Size() != int64(len(n.target)) {
			return fmt.Errorf("symlink %q has size %d, model target %q", p, info.Size(), n.target)
		}
	}
	if !n.mtime.IsZero() && !info.ModTime().Equal(n.mtime) {
		return fmt.Errorf("%q has mtime %v, model %v", p, info.ModTime().UTC(), n.mtime.UTC())
	}
	id, ok := ident.Of(info)
	if !ok {
		return fmt.Errorf("%q has no identity", p)
	}
	if err := ids.observe(n.id, id); err != nil {
		return fmt.Errorf("%q: %w", p, err)
	}
	return nil
}

// viewScope narrows a [viewDiff].
type viewScope struct {
	// keep, when not nil, limits the comparison to the paths it accepts.
	keep func(p string) bool
}

// viewDiff walks f's visible tree against m and describes the first
// difference: names per directory in order, kinds, mode bits, sizes,
// contents, symlink targets, predictable mtimes and identities, including
// that hard-linked names share one and that Locate finds each, and that a
// listing agrees with Lstat.
func viewDiff(f *FS, m *model, ids *idTracker, scope viewScope) error {
	keep := scope.keep
	type seenNode struct {
		n     *mNode
		names []string
	}
	var order []*seenNode
	seen := make(map[*mNode]*seenNode)
	note := func(p string, n *mNode) {
		s := seen[n]
		if s == nil {
			s = &seenNode{n: n}
			seen[n] = s
			order = append(order, s)
		}
		s.names = append(s.names, p)
	}
	info, err := f.Lstat("")
	if err != nil {
		return fmt.Errorf("Lstat(root): %v", err)
	}
	if err := infoDiff("", info, m.root, ids); err != nil {
		return err
	}
	note("", m.root)
	var walk func(p string, dir *mNode) error
	walk = func(p string, dir *mNode) error {
		infos, err := f.ReadDir(p)
		if err != nil {
			return fmt.Errorf("ReadDir(%q): %v", p, err)
		}
		var got, all []string
		var kept []os.FileInfo
		for _, info := range infos {
			all = append(all, info.Name())
			if keep == nil || keep(path.Join(p, info.Name())) {
				got = append(got, info.Name())
				kept = append(kept, info)
			}
		}
		if !slices.IsSorted(all) {
			return fmt.Errorf("ReadDir(%q) is not in name order: %q", p, all)
		}
		var want []string
		for _, name := range sortedNames(dir) {
			if keep == nil || keep(path.Join(p, name)) {
				want = append(want, name)
			}
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("ReadDir(%q) = %q, model %q", p, got, want)
		}
		for _, listed := range kept {
			q := path.Join(p, listed.Name())
			n := dir.kids[listed.Name()]
			lst, err := f.Lstat(q)
			if err != nil {
				return fmt.Errorf("Lstat(%q) of a listed name: %v", q, err)
			}
			// In a partial view other streams may change a directory between
			// its listing and the Lstat.
			stable := n.kind != mDir || keep == nil
			if stable && (listed.Mode() != lst.Mode() || !listed.ModTime().Equal(lst.ModTime()) || n.kind != mDir && listed.Size() != lst.Size()) {
				return fmt.Errorf("%q listed as %v %d %v, Lstat says %v %d %v", q, listed.Mode(), listed.Size(), listed.ModTime().UTC(), lst.Mode(), lst.Size(), lst.ModTime().UTC())
			}
			if a, _ := ident.Of(listed); a != 0 {
				if b, _ := ident.Of(lst); a != b {
					return fmt.Errorf("%q listed with identity %#x, Lstat says %#x", q, a, b)
				}
			}
			if err := infoDiff(q, lst, n, ids); err != nil {
				return err
			}
			note(q, n)
			switch n.kind {
			case mFile:
				data, err := readWhole(f, q)
				if err != nil {
					return fmt.Errorf("reading %q: %v", q, err)
				}
				if !bytes.Equal(data, n.data) {
					return fmt.Errorf("%q holds %s, model %s", q, describeBytes(data), describeBytes(n.data))
				}
			case mLink:
				target, err := f.Readlink(q)
				if err != nil || target != n.target {
					return fmt.Errorf("Readlink(%q) = %q, %v; model %q", q, target, err, n.target)
				}
			case mDir:
				if err := walk(q, n); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk("", m.root); err != nil {
		return err
	}
	for _, s := range order {
		id, _ := ids.of(s.n.id)
		got, ok := f.Locate(id)
		if !ok || !slices.Contains(s.names, got) {
			return fmt.Errorf("Locate(%#x) = %q, %v; the identity belongs to %q", id, got, ok, s.names)
		}
	}
	return nil
}

// readWhole reads a merged file's entire contents through a fresh handle.
func readWhole(f *FS, p string) ([]byte, error) {
	h, err := f.Open(p)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(h)
	return data, errors.Join(err, h.Close())
}

// readAllAt reads a handle's entire contents with positioned reads, leaving
// its offset alone.
func readAllAt(h io.ReaderAt) ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := h.ReadAt(buf, int64(len(out)))
		out = append(out, buf[:n]...)
		if err == io.EOF || err == nil && n == 0 {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// describeBytes summarizes contents for a difference report: the length and
// a bounded prefix.
func describeBytes(b []byte) string {
	if len(b) <= 48 {
		return fmt.Sprintf("%d bytes %q", len(b), b)
	}
	return fmt.Sprintf("%d bytes %q...", len(b), b[:48])
}

// firstDiff locates the first differing byte of two contents for reports.
func firstDiff(a, b []byte) string {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return fmt.Sprintf("first difference at byte %d (%q vs %q)", i, a[i], b[i])
		}
	}
	return fmt.Sprintf("lengths %d and %d", len(a), len(b))
}
