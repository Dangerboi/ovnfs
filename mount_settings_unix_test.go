//go:build darwin || linux

package main

import (
	"net"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestMountCommandNFSInterruptPolicy(t *testing.T) {
	addrs := []net.Addr{&net.UnixAddr{Name: "/state/nfs.sock", Net: "unix"}}
	if runtime.GOOS == "darwin" {
		addrs = append(addrs, &net.TCPAddr{Port: 1234})
	}
	for _, addr := range addrs {
		for _, tc := range []struct {
			noIntr       bool
			want, reject string
		}{
			{noIntr: false, want: ",intr,", reject: "nointr"},
			{noIntr: true, want: ",hard,nointr,", reject: ",intr,"},
		} {
			cmd := mountCmd("nfs", addr, "/mounted", mountSettings{NFSNoIntr: tc.noIntr}, "ovnfs", "", "")
			o := ""
			if i := slices.Index(cmd, "-o"); i >= 0 && i+1 < len(cmd) {
				o = "," + cmd[i+1] + ","
			}
			if !strings.Contains(o, tc.want) || strings.Contains(o, tc.reject) {
				t.Errorf("%s nointr=%v: options %q, want %q without %q", addr.Network(), tc.noIntr, o, tc.want, tc.reject)
			}
			if !strings.Contains(o, ",nordirplus,") {
				t.Errorf("%s nointr=%v: options %q missing nordirplus", addr.Network(), tc.noIntr, o)
			}
		}
	}
}

func TestMountCommandSMBIgnoresNFSIOSize(t *testing.T) {
	addr := &net.TCPAddr{Port: 1234}
	original := mountCmd("smb", addr, "/mounted", mountSettings{}, "ovnfs", "", "")
	changed := mountCmd("smb", addr, "/mounted", mountSettings{NFSIOSize: 1 << 20}, "ovnfs", "", "")
	if !slices.Equal(original, changed) {
		t.Fatalf("NFS size changed SMB command: %v vs %v", original, changed)
	}
}
