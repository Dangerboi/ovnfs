package refs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// filesBackend is git's original ref storage: one file per loose ref under
// the git directory, the packed-refs file for refs packed by git pack-refs,
// and one append-only log file per ref under logs/.
type filesBackend struct{}

func (filesBackend) read(loc location, name string) (Ref, error) {
	r, err := readLoose(loc.dir, name, loc.format)
	if !errors.Is(err, fs.ErrNotExist) || !loc.common {
		return r, err
	}
	packed, err := readPackedRefs(filepath.Join(loc.dir, "packed-refs"), loc.format, func(n []byte) bool { return string(n) == name })
	if err != nil {
		return Ref{}, err
	}
	if len(packed) == 0 {
		return Ref{}, notExist(name)
	}
	// Duplicates keep file order; the first one wins.
	return packed[0], nil
}

func (filesBackend) list(loc location, prefix string, keep func(string) bool) ([]Ref, error) {
	format := loc.format
	loose, err := listLoose(loc.dir, prefix, format)
	if err != nil {
		return nil, err
	}
	var packed []Ref
	if loc.common {
		p := []byte(prefix)
		packed, err = readPackedRefs(filepath.Join(loc.dir, "packed-refs"), format, func(n []byte) bool { return bytes.HasPrefix(n, p) })
		if err != nil {
			return nil, err
		}
	}
	// Loose refs override packed ones of the same name.
	out := make([]Ref, 0, len(loose)+len(packed))
	for len(loose) > 0 || len(packed) > 0 {
		var r Ref
		switch {
		case len(packed) == 0 || len(loose) > 0 && loose[0].Name < packed[0].Name:
			r, loose = loose[0], loose[1:]
		case len(loose) == 0 || packed[0].Name < loose[0].Name:
			r, packed = packed[0], packed[1:]
		default:
			r, loose, packed = loose[0], loose[1:], packed[1:]
		}
		if keep(r.Name) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (filesBackend) reflog(loc location, name string) ([]ReflogEntry, error) {
	data, err := readFile(filepath.Join(loc.dir, "logs", filepath.FromSlash(name)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
			return nil, fmt.Errorf("refs: no reflog for %s: %w", name, os.ErrNotExist)
		}
		return nil, err
	}
	return parseReflogFile(data, loc.format)
}

// readLoose reads the loose ref file dir/name. Like git it accepts a hex
// object name followed by end of file, NUL or any whitespace, and "ref:"
// followed by optional whitespace and a target with surrounding whitespace
// trimmed; a symbolic link whose target starts with "refs/" is a legacy
// symbolic ref. A missing file or a directory is a missing ref.
func readLoose(dir, name string, format object.Format) (Ref, error) {
	path := filepath.Join(dir, filepath.FromSlash(name))
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
			return Ref{}, notExist(name)
		}
		return Ref{}, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		if target, err := os.Readlink(path); err == nil && strings.HasPrefix(target, "refs/") {
			return Ref{Name: name, Target: target}, nil
		}
	}
	data, err := readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || isNotDir(err) || isDir(path) {
			return Ref{}, notExist(name)
		}
		return Ref{}, err
	}
	return parseLoose(name, data, format)
}

// errBroken reports a loose ref file whose content is neither an object name
// nor a symbolic ref; git ignores such refs with a warning.
var errBroken = errors.New("broken reference")

func parseLoose(name string, data []byte, format object.Format) (Ref, error) {
	if rest, ok := bytes.CutPrefix(data, []byte("ref:")); ok {
		target := string(bytes.TrimSpace(rest))
		if target == "" {
			return Ref{}, fmt.Errorf("refs: %s: %w", name, errBroken)
		}
		return Ref{Name: name, Target: target}, nil
	}
	n := format.HexSize()
	if len(data) < n || len(data) > n && !isRefTerminator(data[n]) {
		return Ref{}, fmt.Errorf("refs: %s: %w", name, errBroken)
	}
	id, err := object.ParseIDFormat(format, string(data[:n]))
	if err != nil {
		return Ref{}, fmt.Errorf("refs: %s: %w", name, errBroken)
	}
	return Ref{Name: name, ID: id}, nil
}

// isRefTerminator reports the bytes git accepts right after a loose ref's
// object name.
func isRefTerminator(c byte) bool {
	switch c {
	case 0, ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

// listLoose walks dir/refs for loose refs whose names start with prefix,
// returning them sorted. Broken files and names git would reject are
// skipped, as git for-each-ref skips them.
func listLoose(dir, prefix string, format object.Format) ([]Ref, error) {
	// Walk only the deepest directory that contains every match.
	root := "refs/"
	if strings.HasPrefix(prefix, root) {
		root = prefix[:strings.LastIndexByte(prefix, '/')+1]
	} else if !strings.HasPrefix(root, prefix) {
		return nil, nil
	}
	var out []Ref
	err := filepath.WalkDir(filepath.Join(dir, filepath.FromSlash(root)), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if !strings.HasPrefix(name, prefix) {
			return nil
		}
		if _, err := CheckRefFormat(name, FormatOptions{}); err != nil {
			return nil
		}
		r, err := readLoose(dir, name, format)
		if err != nil {
			return nil
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// WalkDir visits entries in lexical order per directory, which is not
	// ref-name order ("a/b" vs "a-b"), so sort explicitly.
	slices.SortFunc(out, func(a, b Ref) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// readPackedRefs parses packed-refs into name-sorted refs, retaining only
// those whose name satisfies match (nil keeps all). The whole file is still
// validated. A missing file is an empty list. Peeled ('^') lines set the
// preceding ref's Peeled when that ref was retained.
func readPackedRefs(path string, format object.Format, match func(name []byte) bool) ([]Ref, error) {
	data, err := readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var refs []Ref
	sorted := true
	seen, lastKept := false, false
	n := format.HexSize()
	for line := range bytes.Lines(data) {
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0:
		case line[0] == '#':
			// Only the header comment exists; its traits ("peeled",
			// "fully-peeled", "sorted") are hints we can do without.
		case line[0] == '^':
			if !seen {
				return nil, fmt.Errorf("refs: packed-refs: peeled line without a ref")
			}
			id, err := object.ParseIDFormat(format, string(line[1:]))
			if err != nil {
				return nil, fmt.Errorf("refs: packed-refs: %w", err)
			}
			if lastKept {
				refs[len(refs)-1].Peeled = id
			}
		default:
			if len(line) < n+2 || line[n] != ' ' {
				return nil, fmt.Errorf("refs: packed-refs: unexpected line %q", line)
			}
			id, err := object.ParseIDFormat(format, string(line[:n]))
			if err != nil {
				return nil, fmt.Errorf("refs: packed-refs: %w", err)
			}
			seen = true
			nameBytes := line[n+1:]
			lastKept = match == nil || match(nameBytes)
			if !lastKept {
				continue
			}
			name := string(nameBytes)
			if len(refs) > 0 && refs[len(refs)-1].Name >= name {
				sorted = false
			}
			refs = append(refs, Ref{Name: name, ID: id})
		}
	}
	if !sorted {
		slices.SortStableFunc(refs, func(a, b Ref) int { return strings.Compare(a.Name, b.Name) })
	}
	return refs, nil
}

// parseReflogFile decodes a logs/ file into entries, newest first. Lines are
// "<old> <new> <signature>[\t<message>]\n"; malformed lines are skipped, as
// git skips them.
func parseReflogFile(data []byte, format object.Format) ([]ReflogEntry, error) {
	var out []ReflogEntry
	n := format.HexSize()
	for line := range bytes.Lines(data) {
		line = bytes.TrimSuffix(line, []byte("\n"))
		if len(line) < 2*n+2 || line[n] != ' ' || line[2*n+1] != ' ' {
			continue
		}
		oldID, err1 := object.ParseIDFormat(format, string(line[:n]))
		newID, err2 := object.ParseIDFormat(format, string(line[n+1:2*n+1]))
		if err1 != nil || err2 != nil {
			continue
		}
		sig, msg, _ := bytes.Cut(line[2*n+2:], []byte("\t"))
		committer, err := object.ParseSignature(sig)
		if err != nil {
			continue
		}
		out = append(out, ReflogEntry{Old: oldID, New: newID, Committer: committer, Message: string(msg)})
	}
	slices.Reverse(out)
	return out, nil
}

// isDir reports whether path is an existing directory.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
