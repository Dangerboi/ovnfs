package export

import (
	"errors"
	"io"
	"os"
	"sync"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/internal/ident"
	"github.com/stencil-hq/ovnfs/overlay"
)

// routerFile serializes file calls and redirects stale descriptors into the current upper.
type routerFile struct {
	mu     sync.RWMutex
	router *Router
	co     string
	path   string
	name   string
	flags  int
	perm   os.FileMode
	gen    uint64
	offset int64
	file   billy.File
	closed bool
	life   *routerLife
	// identity is the entry's, read at open, which finds it again after a
	// freeze closed file (see relocate).
	identity ident.ID
}

// tryShared enters op on f's current checkout generation under a shared lock.
// When ready is true, the caller holds both f.mu.RLock and s, and must
// release s before calling f.mu.RUnlock. When ready is false and err is nil,
// f needs an exclusive reopen via current.
func (f *routerFile) tryShared(op string, s *checkout.Session) (ready bool, err error) {
	f.mu.RLock()
	if f.closed || f.life.retired.Load() {
		f.mu.RUnlock()
		return false, &os.PathError{Op: op, Path: f.Name(), Err: os.ErrClosed}
	}
	if err := f.router.enter(f.co, op, f.Name(), s); err != nil {
		f.mu.RUnlock()
		if errors.Is(err, os.ErrNotExist) {
			return false, &os.PathError{Op: op, Path: f.Name(), Err: os.ErrClosed}
		}
		return false, err
	}
	if f.life.retired.Load() {
		s.Done()
		f.mu.RUnlock()
		return false, &os.PathError{Op: op, Path: f.Name(), Err: os.ErrClosed}
	}
	if s.Gen == f.gen {
		return true, nil
	}
	s.Done()
	f.mu.RUnlock()
	return false, nil
}

// routerFileInterface checks that generation-aware files provide all billy operations.
var _ billy.File = (*routerFile)(nil)

// current holds the freeze lock and replaces a stale descriptor before any I/O.
// The caller holds mu and owns s, releasing it after its I/O if current succeeds.
func (f *routerFile) current(op string, s *checkout.Session) error {
	if f.closed || f.life.retired.Load() {
		return &os.PathError{Op: op, Path: f.Name(), Err: os.ErrClosed}
	}
	if err := f.router.enter(f.co, op, f.Name(), s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &os.PathError{Op: op, Path: f.Name(), Err: os.ErrClosed}
		}
		return err
	}
	if f.life.retired.Load() {
		s.Done()
		return &os.PathError{Op: op, Path: f.Name(), Err: os.ErrClosed}
	}
	if s.Gen == f.gen {
		return nil
	}
	p, ok := f.relocate(s.FS)
	if !ok {
		s.Done()
		return &os.PathError{Op: op, Path: f.Name(), Err: os.ErrClosed}
	}
	f.path = p
	// Creation and truncation are one-time open effects, never replayed at a freeze.
	flags := f.flags &^ (os.O_CREATE | os.O_EXCL | os.O_TRUNC)
	next, err := s.FS.OpenFile(f.path, flags, f.perm)
	if err != nil {
		s.Done()
		return err
	}
	if _, err := next.Seek(f.offset, io.SeekStart); err != nil {
		next.Close()
		s.Done()
		return err
	}
	old := f.file
	f.file = next
	f.gen = s.Gen
	old.Close()
	return nil
}

// relocate finds where the stale descriptor's entry is now in fs, the
// checkout's current generation: it may have been renamed since the open,
// and keeps its identity through the freeze. False when the entry is gone,
// so a removed or replaced file is never revived under its old name. A
// file without an identity is no checkout entry but a host file a symlink
// to an absolute path led to, which its path finds again.
func (f *routerFile) relocate(fs *overlay.FS) (string, bool) {
	if f.identity == 0 {
		return f.path, true
	}
	return fs.Locate(f.identity)
}

// Name returns the file's canonical export-relative path.
func (f *routerFile) Name() string { return f.name }

// Read reads at the wrapper's current offset from the current checkout generation.
func (f *routerFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s checkout.Session
	if err := f.current("read", &s); err != nil {
		return 0, err
	}
	defer s.Done()
	n, err := f.file.Read(p)
	f.offset += int64(n)
	return n, err
}

// ReadAt reads without changing the offset remembered for a later reopen.
func (f *routerFile) ReadAt(p []byte, off int64) (int, error) {
	var s checkout.Session
	if ready, err := f.tryShared("readat", &s); err != nil {
		return 0, err
	} else if ready {
		defer f.mu.RUnlock()
		defer s.Done()
		return f.file.ReadAt(p, off)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.current("readat", &s); err != nil {
		return 0, err
	}
	defer s.Done()
	return f.file.ReadAt(p, off)
}

// Write writes into the current upper and remembers the resulting file position.
func (f *routerFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s checkout.Session
	if err := f.current("write", &s); err != nil {
		return 0, err
	}
	defer s.Done()
	s.Touch()
	n, err := f.file.Write(p)
	if f.flags&os.O_APPEND == 0 {
		f.offset += int64(n)
		return n, err
	}
	pos, seekErr := f.file.Seek(0, io.SeekCurrent)
	if seekErr == nil {
		f.offset = pos
	} else if err == nil {
		err = seekErr
	}
	return n, err
}

// WriteAt performs a positioned write without changing the remembered sequential offset.
func (f *routerFile) WriteAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s checkout.Session
	if err := f.current("writeat", &s); err != nil {
		return 0, err
	}
	defer s.Done()
	s.Touch()
	if writer, ok := f.file.(io.WriterAt); ok {
		return writer.WriteAt(p, off)
	}
	if _, err := f.file.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := f.file.Write(p)
	_, restoreErr := f.file.Seek(f.offset, io.SeekStart)
	if err == nil {
		err = restoreErr
	}
	return n, err
}

// Sync flushes the current descriptor for SMB write-through and flush requests.
func (f *routerFile) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s checkout.Session
	if err := f.current("sync", &s); err != nil {
		return err
	}
	defer s.Done()
	if syncer, ok := f.file.(interface{ Sync() error }); ok {
		return syncer.Sync()
	}
	return billy.ErrNotSupported
}

// Seek changes the position preserved across checkout freezes.
func (f *routerFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s checkout.Session
	if err := f.current("seek", &s); err != nil {
		return 0, err
	}
	defer s.Done()
	pos, err := f.file.Seek(offset, whence)
	if err == nil {
		f.offset = pos
	}
	return pos, err
}

// Truncate resizes the file in the checkout's current upper.
func (f *routerFile) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s checkout.Session
	if err := f.current("truncate", &s); err != nil {
		return err
	}
	defer s.Done()
	s.Touch()
	return f.file.Truncate(size)
}

// Lock passes advisory locking through to the current descriptor.
func (f *routerFile) Lock() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s checkout.Session
	if err := f.current("lock", &s); err != nil {
		return err
	}
	defer s.Done()
	return f.file.Lock()
}

// Unlock passes advisory unlocking through to the current descriptor.
func (f *routerFile) Unlock() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s checkout.Session
	if err := f.current("unlock", &s); err != nil {
		return err
	}
	defer s.Done()
	return f.file.Unlock()
}

// Stat reports the current file attributes for backends that inspect open files.
func (f *routerFile) Stat() (os.FileInfo, error) {
	var s checkout.Session
	if ready, err := f.tryShared("stat", &s); err != nil {
		return nil, err
	} else if ready {
		defer f.mu.RUnlock()
		defer s.Done()
		if statter, ok := f.file.(interface{ Stat() (os.FileInfo, error) }); ok {
			return statter.Stat()
		}
		return s.FS.Stat(f.path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.current("stat", &s); err != nil {
		return nil, err
	}
	defer s.Done()
	if statter, ok := f.file.(interface{ Stat() (os.FileInfo, error) }); ok {
		return statter.Stat()
	}
	return s.FS.Stat(f.path)
}

// Close releases the descriptor even when its checkout was removed.
// A close never needs to copy an obsolete descriptor into a fresh upper.
func (f *routerFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return &os.PathError{Op: "close", Path: f.Name(), Err: os.ErrClosed}
	}
	f.closed = true
	return f.file.Close()
}

// identityOf returns the identity of the entry an overlay file was opened
// on, which the overlay knows without a syscall; 0 for a file without one.
func identityOf(f billy.File) ident.ID {
	if i, ok := f.(ident.Identifier); ok {
		return i.Identity()
	}
	return 0
}
