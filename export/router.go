// Package export presents checkout filesystems through one shared export root.
package export

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/helper/chroot"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/overlay"
)

// Router dispatches export-relative paths to the registry's current checkout view.
type Router struct {
	reg    *checkout.Registry
	lifeMu sync.Mutex
	lives  map[string]*routerLife
}

// routerLife identifies one checkout incarnation for retained protocol handles.
type routerLife struct {
	retired atomic.Bool
}

// routerInterfaces checks that the export provides the filesystem and metadata operations.
var (
	_ billy.Filesystem = (*Router)(nil)
	_ billy.Change     = (*Router)(nil)
	_ billy.Capable    = (*Router)(nil)
)

// routerWriteFlags includes every open flag that can change a checkout.
const routerWriteFlags = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_TRUNC

// NewRouter returns the virtual export root backed by reg.
func NewRouter(reg *checkout.Registry) *Router {
	return &Router{reg: reg, lives: make(map[string]*routerLife)}
}

// life returns the current incarnation token while the caller holds a checkout session.
func (r *Router) life(name string) *routerLife {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()
	if token := r.lives[name]; token != nil {
		return token
	}
	token := &routerLife{}
	r.lives[name] = token
	return token
}

// retire permanently invalidates retained files before a checkout name can be reused.
func (r *Router) retire(name string) {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()
	if token := r.lives[name]; token != nil {
		token.retired.Store(true)
		delete(r.lives, name)
	}
}

// splitRouterPath canonicalizes a path and separates the checkout from its local name.
func splitRouterPath(name string) (string, string) {
	p := overlay.Clean(name)
	co, rest, _ := strings.Cut(p, "/")
	return co, rest
}

// enter translates registry lookup errors to filesystem errors.
func (r *Router) enter(co, op, name string, s *checkout.Session) error {
	err := r.reg.Enter(co, s)
	if err != nil {
		var e *checkout.Error
		if errors.As(err, &e) && e.Code == "not_found" {
			return &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
		}
	}
	return err
}

// routerDenied reports a mutation that would change the export's checkout entries.
func routerDenied(op, name string) error {
	return &fs.PathError{Op: op, Path: name, Err: os.ErrPermission}
}

// mutate runs a path mutation while holding the checkout's shared freeze lock,
// touching the checkout when it succeeds.
func (r *Router) mutate(op, name string, fn func(*overlay.FS, string) error) error {
	co, p := splitRouterPath(name)
	if co == "" || p == "" {
		return routerDenied(op, name)
	}
	var s checkout.Session
	if err := r.enter(co, op, name, &s); err != nil {
		return err
	}
	defer s.Done()
	if err := fn(s.FS, p); err != nil {
		return err
	}
	s.Touch()
	return nil
}

// Create creates or truncates a file within an existing checkout.
func (r *Router) Create(name string) (billy.File, error) {
	return r.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// Open opens a file or directory read-only.
func (r *Router) Open(name string) (billy.File, error) {
	return r.OpenFile(name, os.O_RDONLY, 0)
}

// OpenFile opens a checkout file and keeps future calls on its current generation.
func (r *Router) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	canonical := overlay.Clean(name)
	co, p, _ := strings.Cut(canonical, "/")
	if flag&routerWriteFlags != 0 && p == "" {
		return nil, routerDenied("open", name)
	}
	if co == "" {
		return &rootFile{router: r, name: name}, nil
	}
	if marker(co, p) {
		return markerFile{&rootFile{router: r, name: name}}, nil
	}
	var s checkout.Session
	if err := r.enter(co, "open", name, &s); err != nil {
		return nil, err
	}
	defer s.Done()
	f, err := s.FS.OpenFile(p, flag, perm)
	if err != nil {
		return nil, err
	}
	if flag&(os.O_CREATE|os.O_TRUNC) != 0 {
		s.Touch()
	}
	return &routerFile{router: r, co: co, path: p, name: canonical, flags: flag, perm: perm, gen: s.Gen, file: f, life: r.life(co)}, nil
}

// Stat returns current attributes, following the final symlink within its checkout.
func (r *Router) Stat(name string) (os.FileInfo, error) {
	return r.info(name, false)
}

// Lstat returns current attributes without following the final symlink.
func (r *Router) Lstat(name string) (os.FileInfo, error) {
	return r.info(name, true)
}

// info obtains real checkout attributes while synthesizing the export root.
func (r *Router) info(name string, nofollow bool) (os.FileInfo, error) {
	co, p := splitRouterPath(name)
	if co == "" {
		return rootInfo{mtime: r.reg.RootTime()}, nil
	}
	if marker(co, p) {
		return markerInfo{mtime: r.reg.RootTime()}, nil
	}
	var s checkout.Session
	err := r.enter(co, "stat", name, &s)
	if err != nil {
		return nil, err
	}
	defer s.Done()
	var fi os.FileInfo
	if nofollow {
		fi, err = s.FS.Lstat(p)
	} else {
		fi, err = s.FS.Stat(p)
	}
	if err == nil && p == "" {
		fi = checkoutRootInfo{FileInfo: fi, name: co}
	}
	return fi, err
}

// ReadDir lists the live checkout names or a directory in one checkout. The
// root listing leaves out the [neverIndex] marker, which is reachable by name only.
func (r *Router) ReadDir(name string) ([]os.FileInfo, error) {
	co, p := splitRouterPath(name)
	if co == "" {
		names := r.reg.Names()
		out := make([]os.FileInfo, 0, len(names))
		for _, name := range names {
			fi, err := r.Lstat(name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, fi)
		}
		return out, nil
	}
	var s checkout.Session
	if err := r.enter(co, "readdir", name, &s); err != nil {
		return nil, err
	}
	defer s.Done()
	return s.FS.ReadDir(p)
}

// TempFile creates a temporary file in an existing checkout directory.
func (r *Router) TempFile(dir, prefix string) (billy.File, error) {
	co, p := splitRouterPath(dir)
	if co == "" {
		return nil, routerDenied("tempfile", dir)
	}
	var s checkout.Session
	if err := r.enter(co, "tempfile", dir, &s); err != nil {
		return nil, err
	}
	defer s.Done()
	f, err := s.FS.TempFile(p, prefix)
	if err != nil {
		return nil, err
	}
	s.Touch()
	p = overlay.Clean(f.Name())
	return &routerFile{router: r, co: co, path: p, name: path.Join(co, p), flags: os.O_RDWR, perm: 0o600, gen: s.Gen, file: f, life: r.life(co)}, nil
}

// MkdirAll creates directories below a checkout's root.
func (r *Router) MkdirAll(name string, perm os.FileMode) error {
	return r.mutate("mkdir", name, func(f *overlay.FS, p string) error { return f.MkdirAll(p, perm) })
}

// Mknod creates a socket or FIFO below a checkout root.
func (r *Router) Mknod(name string, mode os.FileMode) error {
	return r.mutate("mknod", name, func(f *overlay.FS, p string) error { return f.Mknod(p, mode) })
}

// Remove removes a file or empty directory below a checkout's root.
func (r *Router) Remove(name string) error {
	return r.mutate("remove", name, func(f *overlay.FS, p string) error { return f.Remove(p) })
}

// Rename moves a name within one checkout and rejects cross-checkout moves.
func (r *Router) Rename(oldname, newname string) error {
	oldco, oldp := splitRouterPath(oldname)
	newco, newp := splitRouterPath(newname)
	if oldp == "" || newp == "" {
		return routerDenied("rename", oldname)
	}
	if oldco != newco {
		for _, co := range []string{oldco, newco} {
			var s checkout.Session
			if err := r.enter(co, "rename", oldname, &s); err != nil {
				return err
			}
			s.Done()
		}
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: syscall.EXDEV}
	}
	return r.mutate("rename", oldname, func(f *overlay.FS, p string) error { return f.Rename(p, newp) })
}

// Link creates a hard link within one checkout and rejects cross-checkout links.
func (r *Router) Link(oldname, newname string) error {
	oldco, oldp := splitRouterPath(oldname)
	newco, newp := splitRouterPath(newname)
	if oldp == "" || newp == "" {
		return routerDenied("link", newname)
	}
	if oldco != newco {
		for _, co := range []string{oldco, newco} {
			var s checkout.Session
			if err := r.enter(co, "link", oldname, &s); err != nil {
				return err
			}
			s.Done()
		}
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EXDEV}
	}
	return r.mutate("link", oldname, func(f *overlay.FS, p string) error { return f.Link(p, newp) })
}

// Symlink creates a symlink below a checkout root without rewriting its target.
func (r *Router) Symlink(target, link string) error {
	return r.mutate("symlink", link, func(f *overlay.FS, p string) error { return f.Symlink(target, p) })
}

// Readlink returns a checkout symlink's stored target.
func (r *Router) Readlink(name string) (string, error) {
	co, p := splitRouterPath(name)
	if co == "" || marker(co, p) {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: syscall.EINVAL}
	}
	var s checkout.Session
	if err := r.enter(co, "readlink", name, &s); err != nil {
		return "", err
	}
	defer s.Done()
	return s.FS.Readlink(p)
}

// Chmod changes permissions below a checkout root.
func (r *Router) Chmod(name string, mode os.FileMode) error {
	return r.mutate("chmod", name, func(f *overlay.FS, p string) error { return f.Chmod(p, mode) })
}

// Chown changes ownership below a checkout root, following final symlinks.
func (r *Router) Chown(name string, uid, gid int) error {
	return r.mutate("chown", name, func(f *overlay.FS, p string) error { return f.Chown(p, uid, gid) })
}

// Lchown changes ownership below a checkout root without following final symlinks.
func (r *Router) Lchown(name string, uid, gid int) error {
	return r.mutate("lchown", name, func(f *overlay.FS, p string) error { return f.Lchown(p, uid, gid) })
}

// Chtimes changes access and modification times below a checkout root.
func (r *Router) Chtimes(name string, atime, mtime time.Time) error {
	return r.mutate("chtimes", name, func(f *overlay.FS, p string) error { return f.Chtimes(p, atime, mtime) })
}

// Join joins slash-separated export paths.
func (r *Router) Join(elem ...string) string { return path.Join(elem...) }

// Chroot returns a view rooted at an export-relative path.
func (r *Router) Chroot(name string) (billy.Filesystem, error) {
	return chroot.New(r, overlay.Clean(name)), nil
}

// Root returns the virtual export root.
func (r *Router) Root() string { return "/" }

// Capabilities reports the capabilities available inside each writable checkout.
func (r *Router) Capabilities() billy.Capability { return billy.AllCapabilities }
