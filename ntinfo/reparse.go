package ntinfo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"unicode/utf16"
)

// ReparseTagSymlink is IO_REPARSE_TAG_SYMLINK.
const ReparseTagSymlink uint32 = 0xa000000c

// ReparseTagMountPoint is IO_REPARSE_TAG_MOUNT_POINT, a junction.
const ReparseTagMountPoint uint32 = 0xa0000003

// ErrNotSymlink reports a reparse buffer that is neither a symlink nor a junction.
var ErrNotSymlink = errors.New("ntinfo: reparse tag is not IO_REPARSE_TAG_SYMLINK or IO_REPARSE_TAG_MOUNT_POINT")

// IsAbs reports whether a stored, slash-separated link target is absolute:
// rooted (/x), UNC (//srv/share) or a drive path (C:/x). Hosts store targets
// in their own form, so a server on any OS must not resolve these as relative.
func IsAbs(target string) bool {
	return strings.HasPrefix(target, "/") || isDrivePath(target)
}

// isDrivePath reports a target such as C:/x.
func isDrivePath(t string) bool {
	return len(t) >= 3 && t[1] == ':' && t[2] == '/' && ('A' <= t[0] && t[0] <= 'Z' || 'a' <= t[0] && t[0] <= 'z')
}

// MountRelative returns target relative to link when target names a path in
// link's checkout through one of the mount's roots, such as its drive (O:/)
// or share (//127.0.0.1/ovnfs/), else target. Windows tools write links on
// the mount that way (Bun resolves its working directory to the share first);
// served as stored, they would lead the daemon back into its own mount. A
// target in another checkout stays as written: each checkout resolves links
// within itself, so no relative path reaches it. link and the result's base
// are export paths (checkout/dir/name).
func MountRelative(target, link string, roots []string) string {
	for _, root := range roots {
		if len(target) < len(root) || !strings.EqualFold(target[:len(root)], root) {
			continue
		}
		inner := path.Clean(target[len(root):])
		checkout, _, _ := strings.Cut(link, "/")
		if inner != checkout && !strings.HasPrefix(inner, checkout+"/") {
			return target
		}
		from, to := strings.Split(path.Dir(link), "/"), strings.Split(inner, "/")
		common := 0
		for common < len(from) && common < len(to) && from[common] == to[common] {
			common++
		}
		rel := strings.Repeat("../", len(from)-common) + strings.Join(to[common:], "/")
		if rel = strings.TrimSuffix(rel, "/"); rel == "" {
			return "."
		}
		return rel
	}
	return target
}

// SymlinkReparse encodes target as an IO_REPARSE_TAG_SYMLINK REPARSE_DATA_BUFFER
// (header included), for FSCTL_GET_REPARSE_POINT. Drive and UNC targets get
// the NT substitute name Windows resolves (\??\C:\x, \??\UNC\srv\share) and
// their usual form as print name; other targets use one name for both.
func SymlinkReparse(target string) ([]byte, error) {
	print := strings.ReplaceAll(target, "/", "\\")
	substitute := print
	switch {
	case strings.HasPrefix(target, "//?/") || strings.HasPrefix(target, "//./"):
		// Win32 device paths (\\?\C:\x, \\?\Volume{…}\) name the NT \??\ directory.
		substitute = `\??\` + print[4:]
	case strings.HasPrefix(target, "//"):
		substitute = `\??\UNC\` + print[2:]
	case isDrivePath(target):
		substitute = `\??\` + print
	}
	sub, prn := utf16.Encode([]rune(substitute)), utf16.Encode([]rune(print))
	n := 2 * (len(sub) + len(prn))
	if 12+n > 65535 {
		return nil, fmt.Errorf("ntinfo: symlink target too long: %w", fs.ErrInvalid)
	}
	b := make([]byte, 20, 20+n)
	binary.LittleEndian.PutUint32(b, ReparseTagSymlink)
	binary.LittleEndian.PutUint16(b[4:], uint16(12+n))
	binary.LittleEndian.PutUint16(b[10:], uint16(2*len(sub)))
	binary.LittleEndian.PutUint16(b[12:], uint16(2*len(sub)))
	binary.LittleEndian.PutUint16(b[14:], uint16(2*len(prn)))
	if !IsAbs(target) {
		binary.LittleEndian.PutUint32(b[16:], 1) // SYMLINK_FLAG_RELATIVE
	}
	for _, name := range [2][]uint16{sub, prn} {
		for _, u := range name {
			b = binary.LittleEndian.AppendUint16(b, u)
		}
	}
	return b, nil
}

// JunctionReparse encodes target, an absolute slash-separated drive path such
// as C:/x (or //?/C:/x), as an IO_REPARSE_TAG_MOUNT_POINT REPARSE_DATA_BUFFER
// (header included), for FSCTL_SET_REPARSE_POINT on an empty directory. The
// layout is mklink /J's: substitute name \??\C:\x, then print name C:\x, each
// NUL-terminated. Junctions cannot name relative or network targets; those
// fail with [fs.ErrInvalid].
func JunctionReparse(target string) ([]byte, error) {
	if strings.HasPrefix(target, "//?/") {
		target = target[4:]
	}
	if !isDrivePath(target) {
		return nil, fmt.Errorf("ntinfo: junction target %q is not a drive path: %w", target, fs.ErrInvalid)
	}
	print := strings.ReplaceAll(target, "/", "\\")
	sub, prn := utf16.Encode([]rune(`\??\`+print)), utf16.Encode([]rune(print))
	n := 2 * (len(sub) + 1 + len(prn) + 1)
	if 8+n > 65535 {
		return nil, fmt.Errorf("ntinfo: junction target too long: %w", fs.ErrInvalid)
	}
	b := make([]byte, 16, 16+n)
	binary.LittleEndian.PutUint32(b, ReparseTagMountPoint)
	binary.LittleEndian.PutUint16(b[4:], uint16(8+n))
	binary.LittleEndian.PutUint16(b[10:], uint16(2*len(sub)))
	binary.LittleEndian.PutUint16(b[12:], uint16(2*len(sub)+2))
	binary.LittleEndian.PutUint16(b[14:], uint16(2*len(prn)))
	for _, name := range [2][]uint16{sub, prn} {
		for _, u := range name {
			b = binary.LittleEndian.AppendUint16(b, u)
		}
		b = binary.LittleEndian.AppendUint16(b, 0)
	}
	return b, nil
}

// ParseSymlinkReparse decodes an IO_REPARSE_TAG_SYMLINK buffer (header included)
// into a POSIX-style link target, for FSCTL_SET_REPARSE_POINT. A junction's
// IO_REPARSE_TAG_MOUNT_POINT buffer decodes to its absolute target, so the
// junction is stored as a symlink.
func ParseSymlinkReparse(b []byte) (string, error) {
	if len(b) < 8 || int(binary.LittleEndian.Uint16(b[4:])) != len(b)-8 {
		return "", fmt.Errorf("ntinfo: malformed reparse header: %w", fs.ErrInvalid)
	}
	// Both layouts start with the substitute name's offset and length; a
	// junction's path buffer follows at 16, a symlink's after its flags at 20.
	tag := binary.LittleEndian.Uint32(b)
	names := 20
	switch tag {
	case ReparseTagSymlink:
	case ReparseTagMountPoint:
		names = 16
	default:
		return "", ErrNotSymlink
	}
	if len(b) < names {
		return "", fmt.Errorf("ntinfo: short reparse buffer: %w", fs.ErrInvalid)
	}
	off, n := int(binary.LittleEndian.Uint16(b[8:])), int(binary.LittleEndian.Uint16(b[10:]))
	if off&1 != 0 || n&1 != 0 || off > len(b)-names || n > len(b)-names-off || tag == ReparseTagSymlink && binary.LittleEndian.Uint32(b[16:])&^uint32(1) != 0 {
		return "", fmt.Errorf("ntinfo: malformed substitute name: %w", fs.ErrInvalid)
	}
	u := make([]uint16, n/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[names+off+2*i:])
	}
	target := strings.ReplaceAll(string(utf16.Decode(u)), "\\", "/")
	if strings.HasPrefix(target, "/??/UNC/") {
		target = "//" + target[8:]
	} else {
		target = strings.TrimPrefix(target, "/??/")
	}
	if target == "" || strings.ContainsRune(target, 0) || tag == ReparseTagMountPoint && !IsAbs(target) {
		return "", fmt.Errorf("ntinfo: invalid symlink target: %w", fs.ErrInvalid)
	}
	return target, nil
}
