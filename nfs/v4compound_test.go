package nfs

import (
	"encoding/binary"
	"math"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// TestV4NullAndUnknownProcedures checks NULL's empty reply and that
// procedures past COMPOUND are unavailable.
func TestV4NullAndUnknownProcedures(t *testing.T) {
	_, addr := v4serve(t, newTestFS(t), nil)
	c := dial(t, addr)
	if stat, d := c.call(progNFS, 4, proc4Null, nil); stat != rpcSuccess || len(d.buf) != 0 {
		t.Fatalf("NULL: accept_stat %d, %d body bytes", stat, len(d.buf))
	}
	if stat, _ := c.call(progNFS, 4, 2, nil); stat != rpcProcUnavail {
		t.Fatalf("procedure 2: accept_stat %d, want PROC_UNAVAIL", stat)
	}
	// A COMPOUND header that does not decode is GARBAGE_ARGS.
	if stat, _ := c.call(progNFS, 4, proc4Compound, func(e *encoder) { e.str("tag") }); stat != rpcGarbageArgs {
		t.Fatalf("truncated COMPOUND header: accept_stat %d, want GARBAGE_ARGS", stat)
	}
}

// TestV4CompoundFraming checks the tag echo, the minor version check, and
// that a COMPOUND stops at its first failing operation.
func TestV4CompoundFraming(t *testing.T) {
	_, addr := v4serve(t, newTestFS(t), nil)
	c := v4dial(t, addr)

	r := c.runTagged("macOS tag", 1, v4Root())
	if r.status != stMinorVersMismatch || r.tag != "macOS tag" || r.n != 0 {
		t.Fatalf("minor version 1: status %d tag %q results %d", r.status, r.tag, r.n)
	}
	r.end()

	r = c.runTagged("x", 0, v4Root(), v4GetFH())
	if r.status != stOK || r.tag != "x" || r.n != 2 {
		t.Fatalf("status %d tag %q results %d", r.status, r.tag, r.n)
	}
	r.skip(op4PutRootFH)
	if id, _, ok := r.next(op4GetFH, stOK).fh(128); !ok || id != 1 {
		t.Fatalf("root handle id %d (%v), want 1", id, ok)
	}
	r.end()

	// The first failure ends the COMPOUND: GETFH never runs.
	r = c.run(v4GetFH(), v4Root(), v4GetFH())
	if r.status != stNoFileHandle || r.n != 1 {
		t.Fatalf("GETFH without a filehandle: status %d, %d results", r.status, r.n)
	}
	r.next(op4GetFH, stNoFileHandle)
	r.end()

	// Unknown operations are OP_ILLEGAL, numbered as such.
	for _, op := range []uint32{0, 1, 2, 40, 10044} {
		r = c.run(v4Root(), func(e *encoder) { e.u32(op) }, v4GetFH())
		if r.status != stOpIllegal || r.n != 2 {
			t.Fatalf("op %d: status %d, %d results", op, r.status, r.n)
		}
		r.skip(op4PutRootFH)
		r.next(op4Illegal, stOpIllegal)
		r.end()
	}

	// Arguments that do not decode are NFS4ERR_BADXDR for their operation;
	// a missing operation is OP_ILLEGAL with NFS4ERR_BADXDR.
	r = c.run(v4Root(), func(e *encoder) { e.u32(op4Lookup); e.u32(100) })
	if r.status != stBadXDR {
		t.Fatalf("truncated LOOKUP: status %d", r.status)
	}
	r.skip(op4PutRootFH)
	r.next(op4Lookup, stBadXDR)
	r.end()
	stat, d := c.rc.call(progNFS, 4, proc4Compound, func(e *encoder) {
		e.str("")
		e.u32(0)
		e.u32(2)
		e.u32(op4PutRootFH)
	})
	if stat != rpcSuccess {
		t.Fatalf("accept_stat %d", stat)
	}
	r = &v4res{t: t, d: d, status: status(d.u32()), tag: d.str(10), n: d.u32()}
	if r.status != stBadXDR || r.n != 2 {
		t.Fatalf("missing operation: status %d, %d results", r.status, r.n)
	}
	r.skip(op4PutRootFH)
	r.next(op4Illegal, stBadXDR)
	r.end()
}

// TestV4FileHandles checks PUTFH, GETFH, SAVEFH and RESTOREFH: handles are
// a file id and its generation; legacy 8-byte handles, gone ids and old
// generations are STALE, other lengths BADHANDLE.
func TestV4FileHandles(t *testing.T) {
	fs := newTestFS(t)
	if err := os.Mkdir(fs.host("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	id := fs.FileID("d")

	r := c.run(v4PutFH(id), v4GetFH(), v4Save(), v4Root(), v4Restore(), v4GetFH())
	r.skip(op4PutFH)
	if got, _, _ := r.next(op4GetFH, stOK).fh(128); got != id {
		t.Fatalf("GETFH %d, want %d", got, id)
	}
	r.skip(op4SaveFH, op4PutRootFH, op4RestoreFH)
	if got, _, _ := r.next(op4GetFH, stOK).fh(128); got != id {
		t.Fatalf("GETFH after RESTOREFH %d, want %d", got, id)
	}
	r.end()

	if s := c.run(func(e *encoder) { e.u32(op4PutFH); e.opaque([]byte{0, 0, 0, 1}) }).status; s != stBadHandle {
		t.Fatalf("4-byte handle: status %d, want BADHANDLE", s)
	}
	if s := c.run(v4PutFH(999)).status; s != stStale {
		t.Fatalf("unknown id: status %d, want STALE", s)
	}
	// A client upgraded across a handle-format change still holds earlier
	// handles (8-byte id-only or 16-byte unpadded): STALE makes it look the
	// name up again, from the root, whose legacy handle stays valid.
	for _, tc := range []struct {
		name string
		raw  func(uint64) []byte
	}{
		{"8-byte", func(fid uint64) []byte { return binary.BigEndian.AppendUint64(nil, fid) }},
		{"16-byte", func(fid uint64) []byte {
			return binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, fid), 0)
		}},
	} {
		if s := c.run(func(e *encoder) { e.u32(op4PutFH); e.opaque(tc.raw(id)) }).status; s != stStale {
			t.Fatalf("legacy %s handle: status %d, want STALE", tc.name, s)
		}
		if s := c.run(func(e *encoder) { e.u32(op4PutFH); e.opaque(tc.raw(fs.FileID(""))) }).status; s != stOK {
			t.Fatalf("legacy %s root handle: status %d, want OK", tc.name, s)
		}
	}
	if s := c.run(func(e *encoder) { e.u32(op4PutFH); e.fh(id, 1) }).status; s != stStale {
		t.Fatalf("wrong generation: status %d, want STALE", s)
	}
	if s := c.run(v4Root(), v4Restore()).status; s != stNoFileHandle {
		t.Fatalf("RESTOREFH without SAVEFH: status %d, want NOFILEHANDLE", s)
	}
	if err := fs.Remove("d"); err != nil {
		t.Fatal(err)
	}
	if s := c.run(v4PutFH(id)).status; s != stStale {
		t.Fatalf("removed directory: status %d, want STALE", s)
	}
}

// TestV4RetiredHandle checks that a handle RetireHandle retired is STALE
// while a fresh LOOKUP returns a working handle with the same fileid.
func TestV4RetiredHandle(t *testing.T) {
	fs := newTestFS(t)
	fs.write(t, "f", "data")
	_, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	id := fs.FileID("f")
	if !fs.RetireHandle(id) {
		t.Fatal("RetireHandle refused a live file")
	}
	if s := c.run(v4PutFH(id)).status; s != stStale {
		t.Fatalf("retired handle: status %d, want STALE", s)
	}
	r := c.run(v4Root(), v4Look("f"), v4GetFH())
	r.skip(op4PutRootFH, op4Lookup)
	got, gen, ok := r.next(op4GetFH, stOK).fh(128)
	r.end()
	if !ok || got != id || gen != fs.Generation(id) {
		t.Fatalf("fresh handle (%d, %d), want (%d, %d)", got, gen, id, fs.Generation(id))
	}
	r = c.run(func(e *encoder) { e.u32(op4PutFH); e.fh(got, gen) }, v4GetFH())
	r.skip(op4PutFH)
	if again, _, _ := r.next(op4GetFH, stOK).fh(128); again != id {
		t.Fatalf("fresh handle resolves to %d, want %d", again, id)
	}
	r.end()
}

// TestV4LookupAndLookupP walks down and up the tree.
func TestV4LookupAndLookupP(t *testing.T) {
	fs := newTestFS(t)
	if err := os.MkdirAll(fs.host("a/b"), 0o755); err != nil {
		t.Fatal(err)
	}
	v4write(t, fs, "a/f", "x")
	_, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)

	r := c.run(v4Root(), v4Look("a"), v4Look("b"), v4GetFH(), v4LookP(), v4GetFH(), v4LookP(), v4GetFH())
	r.skip(op4PutRootFH, op4Lookup, op4Lookup)
	if id, _, _ := r.next(op4GetFH, stOK).fh(fhSize); id != fs.FileID("a/b") {
		t.Fatalf("a/b id %d", id)
	}
	r.skip(op4LookupP)
	if id, _, _ := r.next(op4GetFH, stOK).fh(fhSize); id != fs.FileID("a") {
		t.Fatalf("parent id %d", id)
	}
	r.skip(op4LookupP)
	if id, _, _ := r.next(op4GetFH, stOK).fh(fhSize); id != 1 {
		t.Fatalf("root id %d", id)
	}
	r.end()

	for _, tc := range []struct {
		ops  []v4op
		want status
	}{
		{[]v4op{v4Root(), v4LookP()}, stNoEnt},
		{[]v4op{v4Root(), v4Look("missing")}, stNoEnt},
		{[]v4op{v4Root(), v4Look("a"), v4Look("f"), v4Look("x")}, stNotDir},
		{[]v4op{v4Look("a")}, stNoFileHandle},
		{[]v4op{v4Root(), v4Look("")}, stBadName},
		{[]v4op{v4Root(), v4Look("a/b")}, stBadName},
	} {
		if s := c.run(tc.ops...).status; s != tc.want {
			t.Errorf("%d ops: status %d, want %d", len(tc.ops), s, tc.want)
		}
	}
}

// TestV4DotNamesAreBad checks that "." and ".." name no entry in any
// operation, so REMOVE ".." cannot remove the parent nor RENAME ".." move
// it.
func TestV4DotNamesAreBad(t *testing.T) {
	fs := newTestFS(t)
	for _, d := range []string{"a/b", "c"} {
		if err := os.MkdirAll(fs.host(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "c", nil)
	inB := []v4op{v4Root(), v4Look("a"), v4Look("b")}
	withB := func(ops ...v4op) []v4op { return append(append([]v4op{}, inB...), ops...) }
	for _, name := range []string{".", ".."} {
		for op, ops := range map[string][]v4op{
			"LOOKUP": withB(v4Look(name)),
			"REMOVE": withB(v4Remove(name)),
			"CREATE": withB(v4Create(typeDir, name, "", v4fattr{})),
			"OPEN": withB(v4Open(v4openArgs{owner: c.owner("o"), access: share4AccessAll, create: true,
				how: create4Unchecked, name: name})),
			"RENAME from": withB(v4Save(), v4Root(), v4Look("c"), v4Rename(name, "x")),
			"RENAME to":   withB(v4Save(), v4Rename("b", name)),
			"LINK":        withB(v4Save(), v4Link(name)),
			"SECINFO":     withB(v4NameOp(op4SecInfo, name)),
		} {
			if s := c.run(ops...).status; s != stBadName {
				t.Errorf("%s %q: status %d, want BADNAME", op, name, s)
			}
		}
	}
	if _, err := os.Stat(fs.host("a/b")); err != nil {
		t.Fatalf("a/b is gone: %v", err)
	}
	if _, err := os.Stat(fs.host("c/x")); err == nil {
		t.Fatal(`RENAME moved a directory named by ".."`)
	}
}

// TestV4RenameKeepsHandle checks that a file's handle follows it across a
// rename: handles are file ids, not paths.
func TestV4RenameKeepsHandle(t *testing.T) {
	fs := newTestFS(t)
	v4write(t, fs, "old", "data")
	_, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	id := fs.FileID("old")
	if s := c.run(v4Root(), v4Save(), v4Rename("old", "new")).status; s != stOK {
		t.Fatalf("RENAME: status %d", s)
	}
	r := c.run(v4PutFH(id), v4GetAttr(attr4Size, attr4FileID))
	r.skip(op4PutFH)
	d := r.next(op4GetAttr, stOK)
	if mask, _ := getBitmap4(d); mask != bitmap4(attr4Size, attr4FileID) {
		t.Fatalf("mask %v", mask)
	}
	d.u32() // attr_vals length
	if size, fileid := d.u64(), d.u64(); size != 4 || fileid != id {
		t.Fatalf("size %d fileid %d through the old handle", size, fileid)
	}
	r.end()
}

// TestV4AccessAndSecInfo checks ACCESS grants the rights the file's mode
// permits and SECINFO offers AUTH_SYS and AUTH_NONE.
func TestV4AccessAndSecInfo(t *testing.T) {
	fs := newTestFS(t)
	v4write(t, fs, "f", "x")
	_, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	for _, want := range []uint32{access4All, access4Read, access4Modify | access4Extend, 0xff} {
		r := c.run(v4Root(), v4Access(want))
		r.skip(op4PutRootFH)
		d := r.next(op4Access, stOK)
		expect := want & (access4All &^ access4Execute) // writable test directory
		if supported, access := d.u32(), d.u32(); supported != expect || access != expect {
			t.Errorf("ACCESS %#x: supported %#x access %#x, want %#x", want, supported, access, expect)
		}
		r.end()
	}
	r := c.run(v4Root(), v4NameOp(op4SecInfo, "f"))
	r.skip(op4PutRootFH)
	d := r.next(op4SecInfo, stOK)
	if n, a, b := d.u32(), d.u32(), d.u32(); n != 2 || a != auth4Sys || b != auth4None {
		t.Fatalf("SECINFO %d: %d, %d", n, a, b)
	}
	r.end()
}

// TestV4GetAttrValues decodes every attribute GETATTR reports of a file and
// checks its value and the order of the values.
func TestV4GetAttrValues(t *testing.T) {
	fs := newTestFS(t)
	v4write(t, fs, "f", "hello")
	if err := os.Chmod(fs.host("f"), 0o640); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1_700_000_000, 123_400) // NTFS keeps 100ns units
	if err := os.Chtimes(fs.host("f"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	a, err := srv.stat(fs.FileID("f"), "f")
	if err != nil {
		t.Fatal(err)
	}

	var all []uint32
	for attr := range 64 {
		all = append(all, uint32(attr))
	}
	r := c.run(v4Root(), v4Look("f"), v4GetAttr(all...))
	r.skip(op4PutRootFH, op4Lookup)
	d := r.next(op4GetAttr, stOK)
	mask, _ := getBitmap4(d)
	if want := attr4Supported.andNot(attr4WriteOnly); mask != want {
		t.Fatalf("mask %v, want %v", mask, want)
	}
	vals := decoder{buf: d.opaque(v4ArgLimit)}
	r.end()

	st, _ := fs.Statfs()
	u64 := func(want uint64) func(*decoder) bool { return func(d *decoder) bool { return d.u64() == want } }
	u32 := func(want uint32) func(*decoder) bool { return func(d *decoder) bool { return d.u32() == want } }
	yes := func(d *decoder) bool { return d.bool() }
	no := func(d *decoder) bool { return !d.bool() }
	str := func(want string) func(*decoder) bool {
		return func(d *decoder) bool { return d.str(100) == want }
	}
	tm := func(d *decoder) bool { return d.u64() == uint64(mtime.Unix()) && d.u32() == 123_400 }
	owner := strconv.Itoa(os.Getuid())
	if runtime.GOOS == "windows" {
		owner = "0" // os.Getuid is -1, and a Windows host file has no uid
	}
	checks := map[uint32]func(*decoder) bool{
		attr4SupportedAttrs: func(d *decoder) bool {
			b, _ := getBitmap4(d)
			return b == attr4Supported && !b.has(25) && !b.has(39) && b.has(attr4TimeModifySet)
		},
		attr4Type:            u32(uint32(typeReg)),
		attr4FHExpireType:    u32(0),
		attr4Change:          u64(v4Change(&a)),
		attr4Size:            u64(5),
		attr4LinkSupport:     yes,
		attr4SymlinkSupport:  yes,
		attr4NamedAttr:       no,
		attr4FSID:            func(d *decoder) bool { return d.u64() == 0 && d.u64() == 1 },
		attr4UniqueHandles:   yes,
		attr4LeaseTime:       u32(90),
		attr4RDAttrError:     u32(0),
		attr4ACLSupport:      u32(0),
		attr4CanSetTime:      yes,
		attr4CaseInsensitive: no,
		attr4CasePreserving:  yes,
		attr4ChownRestricted: yes,
		attr4FileHandle:      func(d *decoder) bool { id, _, ok := d.fh(128); return ok && id == a.fileid },
		attr4FileID:          u64(a.fileid),
		attr4FilesAvail:      u64(st.FilesFree),
		attr4FilesFree:       u64(st.FilesFree),
		attr4FilesTotal:      u64(st.Files),
		attr4Homogeneous:     yes,
		attr4MaxFileSize:     u64(math.MaxInt64),
		attr4MaxLink:         u32(32767),
		attr4MaxName:         u32(255),
		attr4MaxRead:         u64(1 << 20),
		attr4MaxWrite:        u64(1 << 20),
		attr4Mode:            u32(uint32(hostPerm(0o640, false))),
		attr4NoTrunc:         yes,
		attr4NumLinks:        u32(1),
		attr4Owner:           str(owner),
		attr4OwnerGroup:      str(strconv.FormatUint(uint64(a.gid), 10)),
		attr4RawDev:          func(d *decoder) bool { major, minor := d.u32(), d.u32(); return major == 0 && minor == 0 },
		attr4SpaceAvail:      u64(st.Avail * uint64(st.BlockSize)),
		attr4SpaceFree:       u64(st.Free * uint64(st.BlockSize)),
		attr4SpaceTotal:      u64(st.Blocks * uint64(st.BlockSize)),
		attr4SpaceUsed:       u64(5),
		attr4TimeAccess:      tm,
		attr4TimeDelta:       func(d *decoder) bool { return d.u64() == 0 && d.u32() == 1 },
		attr4TimeMetadata:    tm,
		attr4TimeModify:      tm,
		attr4MountedOnFileID: u64(a.fileid),
	}
	for attr := range mask.all() {
		check := checks[attr]
		if check == nil {
			t.Fatalf("attribute %d reported without a check", attr)
		}
		if !check(&vals) || vals.err != nil {
			t.Fatalf("attribute %d has a wrong value", attr)
		}
		delete(checks, attr)
	}
	if len(checks) != 0 || len(vals.buf) != 0 {
		t.Fatalf("attributes %v not reported, %d bytes left", checks, len(vals.buf))
	}

	// The root is a directory; requesting nothing returns an empty fattr4.
	r = c.run(v4Root(), v4GetAttr(attr4Type), v4GetAttr())
	r.skip(op4PutRootFH)
	d = r.next(op4GetAttr, stOK)
	if mask, _ := getBitmap4(d); mask != bitmap4(attr4Type) || d.u32() != 4 || d.u32() != uint32(typeDir) {
		t.Fatal("root type")
	}
	d = r.next(op4GetAttr, stOK)
	if mask, _ := getBitmap4(d); mask != (v4bitmap{}) || d.u32() != 0 {
		t.Fatal("empty GETATTR")
	}
	r.end()
}

// TestV4SupportedAttrsIncludeSettableTimes: Linux masks SETATTR by
// supported_attrs, so without these it never sends times and touch -d
// silently does nothing.
func TestV4SupportedAttrsIncludeSettableTimes(t *testing.T) {
	if !attr4Supported.has(attr4TimeAccessSet) || !attr4Supported.has(attr4TimeModifySet) {
		t.Error("supported_attrs lacks the settable times")
	}
	if !attr4Writable.within(attr4Supported) {
		t.Error("a writable attribute is not in supported_attrs")
	}
}

// TestV4CompoundLimits: a COMPOUND of too many operations fails with
// NFS4ERR_RESOURCE before any runs, and its results stay within a budget of
// one full READ: later READs are cut short, then refused.
func TestV4CompoundLimits(t *testing.T) {
	fs := newTestFS(t)
	if err := os.WriteFile(fs.host("big"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(fs.host("big"), 3*maxIO); err != nil {
		t.Fatal(err)
	}
	_, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)

	ops := make([]v4op, v4MaxOps)
	for i := range ops {
		ops[i] = v4Root()
	}
	if r := c.run(ops...); r.status != stOK || r.n != v4MaxOps {
		t.Fatalf("%d operations: status %d, %d results", v4MaxOps, r.status, r.n)
	}
	r := c.run(append(ops, v4GetFH())...)
	if r.status != stResource || r.n != 1 {
		t.Fatalf("%d operations: status %d, %d results", v4MaxOps+1, r.status, r.n)
	}
	r.next(op4PutRootFH, stResource)
	r.end()

	r = c.run(v4Root(), v4Look("big"), v4Read(stateid4Anon, 0, maxIO),
		v4Read(stateid4Anon, maxIO, maxIO), v4Read(stateid4Anon, 2*maxIO, maxIO))
	if r.status != stResource || r.n != 5 {
		t.Fatalf("three full READs: status %d, %d results", r.status, r.n)
	}
	r.skip(op4PutRootFH, op4Lookup)
	d := r.next(op4Read, stOK)
	if eof, n := d.bool(), len(d.opaque(maxIO)); eof || n != maxIO {
		t.Fatalf("first READ: eof %v, %d bytes", eof, n)
	}
	d = r.next(op4Read, stOK)
	if eof, n := d.bool(), len(d.opaque(maxIO)); eof || n == 0 || n >= v4ReplyBudget-maxIO {
		t.Fatalf("second READ: eof %v, %d bytes, want a short read", eof, n)
	}
	r.next(op4Read, stResource)
	r.end()
	if len(r.d.buf) != 0 {
		t.Fatal("trailing bytes")
	}
}
