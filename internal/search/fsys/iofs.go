package fsys

import (
	"errors"
	"io"
	"io/fs"
	"path"
)

var errNotDir = errors.New("not a directory")

// FromIOFS adapts an io/fs.FS. Absolute paths in the walker's namespace are
// '/'-separated and map to fs names by dropping the leading '/' ("/" is
// "."). Stamps carry only modification times, so StableStamps is false and
// listing caches never serve this filesystem.
func FromIOFS(f fs.FS) FS { return &ioFS{f: f} }

type ioFS struct{ f fs.FS }

func (*ioFS) Abs(root string) (string, error) { return path.Clean("/" + root), nil }

func (x *ioFS) NewSession() Session { return &ioSession{x: x} }

func (*ioFS) Separator() byte { return '/' }

func (*ioFS) Workers() int { return 4 }

type ioSession struct {
	x  *ioFS
	ls Listing
}

func fsName(p string) string {
	if p == "/" {
		return "."
	}
	return p[1:]
}

func (s *ioSession) OpenDir(p string) (Handle, error) {
	info, err := fs.Stat(s.x.f, fsName(p))
	if err != nil {
		return Handle{}, err
	}
	if !info.IsDir() {
		return Handle{}, &fs.PathError{Op: "open", Path: p, Err: errNotDir}
	}
	return Handle{path: p, ok: true}, nil
}

func (s *ioSession) OpenChild(parent Handle, name string) (Handle, error) {
	return s.OpenDir(path.Join(parent.path, name))
}

func (*ioSession) Close(Handle) {}

func (s *ioSession) List(h Handle, detail Detail, _ uint32) (*Listing, error) {
	entries, err := fs.ReadDir(s.x.f, fsName(h.path))
	if err != nil {
		return nil, err
	}
	s.ls.reset()
	for _, e := range entries {
		var typ Type
		switch mode := e.Type(); {
		case mode.IsDir():
			typ = Dir
		case mode&fs.ModeSymlink != 0:
			typ = Symlink
		case mode.IsRegular():
			typ = File
		default:
			continue
		}
		size, mtime := int64(-1), int64(0)
		if detail == Full {
			info, err := e.Info()
			if err != nil {
				continue // vanished since the listing
			}
			mtime = info.ModTime().UnixNano()
			if typ == File {
				size = info.Size()
			}
		}
		s.ls.add([]byte(e.Name()), typ, size, mtime, 0)
	}
	return &s.ls, nil
}

// OpenFile only stats name: io/fs files are opened by each Read.
func (s *ioSession) OpenFile(dir Handle, name string) (Handle, FileInfo, error) {
	p := name
	if dir.ok {
		p = path.Join(dir.path, name)
	}
	info, err := fs.Stat(s.x.f, fsName(p))
	if err != nil {
		return Handle{}, FileInfo{}, err
	}
	if !info.Mode().IsRegular() {
		return Handle{}, FileInfo{}, ErrNotRegular
	}
	return Handle{path: p, size: info.Size(), ok: true},
		FileInfo{Type: File, Size: info.Size(), Stamp: Stamp{Mtime: info.ModTime().UnixNano()}}, nil
}

// Read reads a fresh io/fs file sequentially, so read offsets line up.
func (s *ioSession) Read(h Handle, buf []byte, limit int64) ([]byte, error) {
	f, err := s.x.f.Open(fsName(h.path))
	if err != nil {
		return buf[:0], err
	}
	defer f.Close()
	return readInto(buf, h.size, limit, func(p []byte, _ int64) (int, error) {
		n, err := f.Read(p)
		if err == io.EOF {
			err = nil
		}
		return n, err
	})
}

func (*ioSession) Map(Handle, int64) ([]byte, error) { return nil, errors.ErrUnsupported }

func (*ioSession) Unmap([]byte) {}

func (s *ioSession) Stat(p string, _ bool) (FileInfo, error) {
	info, err := fs.Stat(s.x.f, fsName(p))
	if err != nil {
		return FileInfo{}, err
	}
	var typ Type
	switch mode := info.Mode(); {
	case mode.IsDir():
		typ = Dir
	case mode&fs.ModeSymlink != 0:
		typ = Symlink
	case mode.IsRegular():
		typ = File
	}
	return FileInfo{Type: typ, Size: info.Size(), Stamp: Stamp{Mtime: info.ModTime().UnixNano()}}, nil
}

func (s *ioSession) StatDir(h Handle) (Stamp, error) {
	info, err := s.Stat(h.path, true)
	return info.Stamp, err
}

func (*ioSession) StableStamps(Handle, Stamp) bool { return false }

func (*ioSession) IdentifyDir(Handle) (ID, error) { return ID{}, errors.ErrUnsupported }

func (*ioSession) Identify(string) (ID, error) { return ID{}, errors.ErrUnsupported }
