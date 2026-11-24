package nfs

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestMount checks the MOUNT v3 replies: MNT of any path returns the root
// handle with AUTH_NULL, and the other procedures answer for one export
// with no recorded mounts.
func TestMount(t *testing.T) {
	fs := newTestFS(t)
	c := v3server(t, fs)
	words := func(v ...uint32) []byte {
		var b []byte
		for _, w := range v {
			b = binary.BigEndian.AppendUint32(b, w)
		}
		return b
	}
	for _, dirpath := range []string{"/", "/some/where", ""} {
		stat, d := c.call(progMount, 3, mntMnt, func(e *encoder) { e.str(dirpath) })
		root := fs.FileID("")
		var want encoder
		want.u32(0)
		want.fh(root, fs.Generation(root))
		want.u32(1)
		want.u32(authNone)
		if stat != rpcSuccess || !bytes.Equal(d.buf, want.buf) {
			t.Errorf("MNT %q: accept_stat %d, result %x, want %x", dirpath, stat, d.buf, want.buf)
		}
	}
	export := words(1, 1)
	export = append(export, '/', 0, 0, 0)
	export = append(export, words(0, 0)...)
	for _, tc := range []struct {
		name string
		proc uint32
		args func(*encoder)
		want []byte
	}{
		{"NULL", mntNull, nil, nil},
		{"DUMP", mntDump, nil, words(0)},
		{"UMNT", mntUmnt, func(e *encoder) { e.str("/") }, nil},
		{"UMNTALL", mntUmntAll, nil, nil},
		{"EXPORT", mntExport, nil, export},
	} {
		stat, d := c.call(progMount, 3, tc.proc, tc.args)
		if stat != rpcSuccess || !bytes.Equal(d.buf, tc.want) {
			t.Errorf("%s: accept_stat %d, result %x, want %x", tc.name, stat, d.buf, tc.want)
		}
	}
	if stat, _ := c.call(progMount, 3, mntMnt, nil); stat != rpcGarbageArgs {
		t.Errorf("MNT without a path: accept_stat %d, want GARBAGE_ARGS", stat)
	}
	if stat, _ := c.call(progMount, 3, mntMnt, func(e *encoder) { e.str(strings.Repeat("a", mntPathLen+1)) }); stat != rpcGarbageArgs {
		t.Errorf("MNT of an overlong path: accept_stat %d, want GARBAGE_ARGS", stat)
	}
	if stat, _ := c.call(progMount, 3, 6, nil); stat != rpcProcUnavail {
		t.Errorf("procedure 6: accept_stat %d, want PROC_UNAVAIL", stat)
	}
	// The handle MNT returns serves NFSv3 calls.
	if st, _ := c.v3call(v3GetAttr, func(e *encoder) { e.fh(fs.FileID(""), 0) }); st != stOK {
		t.Errorf("GETATTR of the mounted root: %d", st)
	}
}
