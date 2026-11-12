package ntinfo

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"slices"
	"testing"
	"unicode/utf16"
)

// wireName decodes a UTF-16LE reparse name.
func wireName(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

func TestIsAbs(t *testing.T) {
	for target, want := range map[string]bool{
		"/x":            true,
		"C:/x":          true,
		"c:/":           true,
		"//srv/share/x": true,
		"//?/C:/x":      true,
		"C:x":           false,
		"C:":            false,
		"1:/x":          false,
		"x/C:/y":        false,
		"../x":          false,
	} {
		if got := IsAbs(target); got != want {
			t.Errorf("IsAbs(%q) = %v, want %v", target, got, want)
		}
	}
}

func TestSymlinkReparseDevicePath(t *testing.T) {
	for target, want := range map[string]string{
		"//?/C:/x":          `\??\C:\x`,
		"//?/UNC/srv/s":     `\??\UNC\srv\s`,
		"//?/Volume{abc}/x": `\??\Volume{abc}\x`,
		"//./C:/x":          `\??\C:\x`,
	} {
		b, err := SymlinkReparse(target)
		if err != nil {
			t.Fatalf("%q: %v", target, err)
		}
		off, n := int(binary.LittleEndian.Uint16(b[8:])), int(binary.LittleEndian.Uint16(b[10:]))
		if got := wireName(b[20+off : 20+off+n]); got != want {
			t.Errorf("%q: substitute name %q, want %q", target, got, want)
		}
		if binary.LittleEndian.Uint32(b[16:]) != 0 {
			t.Errorf("%q: marked relative", target)
		}
	}
}

func TestSymlinkReparse(t *testing.T) {
	for _, tc := range []struct {
		target, substitute, print string
		relative                  bool
	}{
		{"a/b/c", `a\b\c`, `a\b\c`, true},
		{"../x", `..\x`, `..\x`, true},
		{"/x", `\x`, `\x`, false},
		{"C:/x/y", `\??\C:\x\y`, `C:\x\y`, false},
		{"//srv/s/x", `\??\UNC\srv\s\x`, `\\srv\s\x`, false},
	} {
		b, err := SymlinkReparse(tc.target)
		if err != nil {
			t.Fatalf("%q: %v", tc.target, err)
		}
		name := func(off, n int) string { return wireName(b[20+off : 20+off+n]) }
		if got := name(int(binary.LittleEndian.Uint16(b[8:])), int(binary.LittleEndian.Uint16(b[10:]))); got != tc.substitute {
			t.Errorf("%q: substitute name %q, want %q", tc.target, got, tc.substitute)
		}
		if got := name(int(binary.LittleEndian.Uint16(b[12:])), int(binary.LittleEndian.Uint16(b[14:]))); got != tc.print {
			t.Errorf("%q: print name %q, want %q", tc.target, got, tc.print)
		}
		if relative := binary.LittleEndian.Uint32(b[16:]) == 1; relative != tc.relative {
			t.Errorf("%q: relative flag %v", tc.target, relative)
		}
		if got, err := ParseSymlinkReparse(b); err != nil || got != tc.target {
			t.Errorf("%q: parsed %q, %v", tc.target, got, err)
		}
	}
	b, _ := SymlinkReparse("x")
	if _, err := ParseSymlinkReparse(b[:len(b)-2]); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("truncated: %v", err)
	}
	binary.LittleEndian.PutUint32(b, 0x80000014)
	if _, err := ParseSymlinkReparse(b); !errors.Is(err, ErrNotSymlink) {
		t.Errorf("other tag: %v", err)
	}
}

// TestParseMalformedJunction refuses junction buffers whose names do not fit,
// without reading past them.
func TestParseMalformedJunction(t *testing.T) {
	short := make([]byte, 12)
	binary.LittleEndian.PutUint32(short, ReparseTagMountPoint)
	binary.LittleEndian.PutUint16(short[4:], 4)
	for name, mutate := range map[string]func([]byte) []byte{
		"short header": func([]byte) []byte { return short },
		"odd offset":   func(b []byte) []byte { binary.LittleEndian.PutUint16(b[8:], 1); return b },
		"long name":    func(b []byte) []byte { binary.LittleEndian.PutUint16(b[10:], uint16(len(b)-14)); return b },
		"far offset":   func(b []byte) []byte { binary.LittleEndian.PutUint16(b[8:], uint16(len(b)-14)); return b },
	} {
		b := mutate(mountPointReparse(`\??\C:\x`, `C:\x`))
		if _, err := ParseSymlinkReparse(b); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// mountPointReparse builds the IO_REPARSE_TAG_MOUNT_POINT buffer mklink /J and
// libuv send: substitute name, then print name, each NUL-terminated.
func mountPointReparse(substitute, print string) []byte {
	var names []uint16
	sub := utf16.Encode([]rune(substitute))
	prn := utf16.Encode([]rune(print))
	names = append(append(append(append(names, sub...), 0), prn...), 0)
	b := make([]byte, 16, 16+2*len(names))
	binary.LittleEndian.PutUint32(b, ReparseTagMountPoint)
	binary.LittleEndian.PutUint16(b[4:], uint16(8+2*len(names)))
	binary.LittleEndian.PutUint16(b[10:], uint16(2*len(sub)))
	binary.LittleEndian.PutUint16(b[12:], uint16(2*len(sub)+2))
	binary.LittleEndian.PutUint16(b[14:], uint16(2*len(prn)))
	for _, u := range names {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

// TestParseJunction stores a junction as a symlink to its absolute target;
// junction targets are absolute by definition.
func TestParseJunction(t *testing.T) {
	if got, err := ParseSymlinkReparse(mountPointReparse(`\??\C:\repo\packages\utils`, `C:\repo\packages\utils`)); err != nil || got != "C:/repo/packages/utils" {
		t.Errorf("junction = %q, %v", got, err)
	}
	if _, err := ParseSymlinkReparse(mountPointReparse(`packages\utils`, `packages\utils`)); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("relative junction: %v", err)
	}
}

// TestJunctionReparse encodes junctions as mklink /J does, so the daemon can
// link directories without the symlink privilege, and refuses targets a
// junction cannot hold.
func TestJunctionReparse(t *testing.T) {
	for target, print := range map[string]string{
		"C:/Users/me/ovnfs/bases/b_1/objects": `C:\Users\me\ovnfs\bases\b_1\objects`,
		"//?/C:/x":                            `C:\x`,
	} {
		b, err := JunctionReparse(target)
		if err != nil {
			t.Fatalf("%q: %v", target, err)
		}
		if want := mountPointReparse(`\??\`+print, print); !slices.Equal(b, want) {
			t.Errorf("%q: buffer %x, want %x", target, b, want)
		}
	}
	for _, target := range []string{"objects", "../objects", "/objects", "//srv/share/objects"} {
		if _, err := JunctionReparse(target); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("%q: %v, want fs.ErrInvalid", target, err)
		}
	}
}

// TestMountRelative stores links that name their own checkout through the
// mount relative to the link, and leaves every other target alone.
func TestMountRelative(t *testing.T) {
	roots := []string{"O:/", "//127.0.0.1/ovnfs/"}
	for _, tc := range []struct{ target, link, want string }{
		{"O:/co/packages/utils", "co/node_modules/@x/utils", "../../packages/utils"},
		{"o:/co/packages/utils", "co/node_modules/@x/utils", "../../packages/utils"},
		{"//127.0.0.1/OVNFS/co/packages/utils", "co/node_modules/@x/utils", "../../packages/utils"},
		{"O:/co", "co/node_modules/root", ".."},
		{"O:/co/node_modules/a", "co/node_modules/b", "a"},
		{"O:/co/node_modules", "co/node_modules/self", "."},
		{"O:/other/packages/utils", "co/node_modules/@x/utils", "O:/other/packages/utils"},
		{"O:/co/../other/x", "co/link", "O:/co/../other/x"},
		{"O:/cofee/x", "co/link", "O:/cofee/x"},
		{"C:/co/packages/utils", "co/link", "C:/co/packages/utils"},
		{"../packages/utils", "co/node_modules/utils", "../packages/utils"},
	} {
		if got := MountRelative(tc.target, tc.link, roots); got != tc.want {
			t.Errorf("MountRelative(%q, %q) = %q, want %q", tc.target, tc.link, got, tc.want)
		}
	}
}
