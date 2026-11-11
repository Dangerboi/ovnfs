package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
	"golang.org/x/sys/unix"
)

// TestSecondStopForcesBusyFUSEMount checks serve's stop contract on FUSE: a
// stop while a process holds the mount fails and asks for another, and that
// second stop forces the unmount and ends serve.
func TestSecondStopForcesBusyFUSEMount(t *testing.T) {
	if f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		t.Skipf("/dev/fuse unavailable: %v", err)
	} else {
		f.Close()
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		if _, err := exec.LookPath("fusermount"); err != nil {
			t.Skip("fusermount3/fusermount not installed")
		}
	}
	var sfs unix.Statfs_t
	if err := unix.Statfs("/sys/fs/fuse/connections", &sfs); err != nil || sfs.Type != 0x65735543 { // FUSE_CTL_SUPER_MAGIC
		t.Skip("fusectl is not mounted at /sys/fs/fuse/connections")
	}
	// A short path: a Unix socket path is limited to about 100 bytes.
	state, err := os.MkdirTemp("", "ovnfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	mount := filepath.Join(state, "mnt")
	served := make(chan error, 1)
	go func() {
		served <- (&serveCmd{Proto: "fuse"}).Run(&cli{state: state, mount: mount})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !accepting(state) {
		select {
		case err := <-served:
			t.Fatalf("serve returned before it listened: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("serve did not listen within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client, err := admin.Dial(socketPath(state), adminProtocol)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	held, err := os.Open(mount)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	// Runs before the client closes: release the mount, then stop serve.
	t.Cleanup(func() {
		held.Close()
		if stopped {
			return
		}
		client.Call(context.Background(), "stop", api.StopArgs{})
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Error("serve still running at cleanup")
		}
	})

	_, err = client.Call(context.Background(), "stop", api.StopArgs{})
	if _, ok := errors.AsType[*admin.RemoteError](err); !ok || !strings.Contains(err.Error(), "stop again to force") {
		t.Fatalf("first stop on a busy mount: %v", err)
	}
	_, err = client.Call(context.Background(), "stop", api.StopArgs{})
	if _, ok := errors.AsType[*admin.RemoteError](err); ok {
		t.Fatalf("second stop: %v", err)
	}
	select {
	case err := <-served:
		stopped = true
		if err != nil {
			t.Fatalf("serve after the forced stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve still running after the forced stop")
	}
}
