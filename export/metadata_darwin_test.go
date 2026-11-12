//go:build darwin

package export_test

import (
	"encoding/binary"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/overlay"
)

// smbNativeTime converts a native timestamp to SMB's 100ns Windows epoch.
func smbNativeTime(ts syscall.Timespec) uint64 {
	return uint64(ts.Sec)*10_000_000 + uint64(ts.Nsec)/100 + 116_444_736_000_000_000
}

// TestSMBNativeMetadata survives writes and renames without conflating creation,
// access, or change time with modification time. It exercises actual SMB packets
// and compares their metadata with the checkout filesystem's native stat data.
func TestSMBNativeMetadata(t *testing.T) {
	for _, storage := range []struct {
		name   string
		memory *overlay.Memory
	}{
		{"memory", overlay.NewMemory(64<<20, 1<<20, 0)},
		{"disk", nil},
	} {
		t.Run(storage.name, func(t *testing.T) {
			f := newStackWithMemory(t, nil, storage.memory)
			f.create(t, "agent")
			c := startSMB(t, f)
			id := c.mustOpen("agent/output.tmp", 2)

			nativeInfo := func(name string) os.FileInfo {
				t.Helper()
				var s checkout.Session
				if err := f.reg.Enter("agent", &s); err != nil {
					t.Fatal(err)
				}
				info, err := s.FS.Lstat(name)
				s.Done()
				if err != nil {
					t.Fatal(err)
				}
				return info
			}
			created := smbNativeTime(nativeInfo("output.tmp").Sys().(*syscall.Stat_t).Birthtimespec)
			c.close(id)
			openMetadata := func(name string) smbID {
				t.Helper()
				body := make([]byte, 56)
				binary.LittleEndian.PutUint16(body, 57)
				binary.LittleEndian.PutUint32(body[4:], 2)
				binary.LittleEndian.PutUint32(body[24:], 0x80000000)
				binary.LittleEndian.PutUint32(body[32:], 7)
				binary.LittleEndian.PutUint32(body[36:], 1)
				encoded := utf16le("agent/" + name)
				binary.LittleEndian.PutUint16(body[44:], 120)
				binary.LittleEndian.PutUint16(body[46:], uint16(len(encoded)))
				reply := c.exchange(5, append(body, encoded...))
				smbExpect(t, reply, smbOK)
				if len(reply.body) < 88 {
					t.Fatal("short SMB CREATE response")
				}
				var id smbID
				copy(id[:], reply.body[64:80])
				return id
			}
			assertMetadata := func(name string) {
				t.Helper()
				id := openMetadata(name)
				defer c.close(id)
				native := nativeInfo(name)
				stat := native.Sys().(*syscall.Stat_t)
				basic := c.queryInfo(id, 1, 4, 0)
				if len(basic) != 40 {
					t.Fatalf("SMB basic information size %d, want 40", len(basic))
				}
				want := []uint64{created, smbNativeTime(stat.Atimespec), smbNativeTime(stat.Mtimespec), smbNativeTime(stat.Ctimespec)}
				for i, label := range []string{"creation", "access", "modification", "change"} {
					if got := binary.LittleEndian.Uint64(basic[i*8:]); got != want[i] {
						t.Errorf("%s %s time %d, want %d", name, label, got, want[i])
					}
				}

				security := c.queryInfo(id, 3, 0, 3)
				if len(security) < 20 {
					t.Fatal("short SMB security descriptor")
				}
				for _, owner := range []struct {
					label       string
					offset      int
					kind, value uint32
				}{
					{"uid", 4, 1, stat.Uid},
					{"gid", 8, 2, stat.Gid},
				} {
					off := int(binary.LittleEndian.Uint32(security[owner.offset:]))
					if off < 20 || off > len(security)-16 {
						t.Fatalf("invalid SMB %s SID offset %d", owner.label, off)
					}
					sid := security[off : off+16]
					if sid[0] != 1 || sid[1] != 2 || sid[7] != 22 || binary.LittleEndian.Uint32(sid[8:]) != owner.kind {
						t.Fatalf("invalid SMB Unix %s SID %x", owner.label, sid)
					}
					if got := binary.LittleEndian.Uint32(sid[12:]); got != owner.value {
						t.Errorf("SMB %s %d, want native %d", owner.label, got, owner.value)
					}
				}
			}

			assertMetadata("output.tmp")
			id = c.mustOpen("agent/output.tmp", 1)
			modified := time.Now().Add(time.Hour)
			data := make([]byte, 40)
			stamp := smbNativeTime(syscall.NsecToTimespec(modified.UnixNano()))
			binary.LittleEndian.PutUint64(data[8:], stamp)
			binary.LittleEndian.PutUint64(data[16:], stamp)
			body := make([]byte, 32)
			binary.LittleEndian.PutUint16(body, 33)
			body[2], body[3] = 1, 4
			binary.LittleEndian.PutUint32(body[4:], uint32(len(data)))
			binary.LittleEndian.PutUint16(body[8:], 96)
			copy(body[16:], id[:])
			smbExpect(t, c.exchange(17, append(body, data...)), smbOK)
			c.close(id)
			assertMetadata("output.tmp")
			id = c.mustOpen("agent/output.tmp", 1)
			c.write(id, "before rename")
			c.close(id)
			assertMetadata("output.tmp")
			id = c.mustOpen("agent/output.tmp", 1)
			smbExpect(t, c.rename(id, "agent/output"), smbOK)
			c.close(id)
			assertMetadata("output")
			id = c.mustOpen("agent/output", 1)
			c.write(id, "after rename!")
			c.close(id)
			assertMetadata("output")
			id = openMetadata("output")
			c.contents(id, "after rename!")
			c.close(id)
		})
	}
}
