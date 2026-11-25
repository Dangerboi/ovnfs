package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
)

// TestMain answers a spawned `serve` by exiting, so a startDaemon that spawns
// one fails instead of running the tests again in a child process.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Exit(3)
	}
	os.Exit(m.Run())
}

// TestStartWaitsForALoadingDaemon serves an admin socket that answers its
// hello only a second later, as a daemon loading its state does: startDaemon
// waits for that daemon and starts no serve of its own.
func TestStartWaitsForALoadingDaemon(t *testing.T) {
	// A short path: a Unix socket path is limited to about 100 bytes.
	state, err := os.MkdirTemp("", "ovnfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	srv := admin.NewServer()
	go srv.Serve(l)
	time.AfterFunc(time.Second, func() { srv.Ready(admin.Welcome{Version: 1}, nil, nil) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, pid, err := startDaemon(ctx, state, filepath.Join(state, "mnt"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if pid != 0 {
		t.Fatalf("started serve pid %d, want none", pid)
	}
	if client.Welcome().Version != 1 {
		t.Fatal("missing welcome")
	}
}

// TestClientQuotesLogOfAFailedLoad drops a held hello, as a daemon whose state
// load fails does: a client that did not start it quotes the daemon log.
func TestClientQuotesLogOfAFailedLoad(t *testing.T) {
	state, err := os.MkdirTemp("", "ovnfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	if err := os.WriteFile(logPath(state), []byte("read state.json: unexpected end of JSON input\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	go admin.NewServer().Serve(l)
	time.AfterFunc(200*time.Millisecond, func() { l.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := awaitDaemon(ctx, state, 0, nil)
	if err == nil {
		client.Close()
		t.Fatal("hello answered by a daemon that never loaded")
	}
	if !strings.Contains(err.Error(), "read state.json: unexpected end of JSON input") {
		t.Fatalf("error without the daemon log: %v", err)
	}
}

// TestClientWaitsOutARetiringDaemon refuses hellos as a daemon exiting after
// an idle period does, then serves another daemon on the socket under the
// start lock, as an auto-start racing this client would: the client reaches
// the new daemon instead of failing.
func TestClientWaitsOutARetiringDaemon(t *testing.T) {
	state, err := os.MkdirTemp("", "ovnfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	l, err := admin.Listen(socketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	old := admin.NewServer()
	go old.Serve(l)
	old.Ready(admin.Welcome{Version: 1}, nil, nil)
	if !old.Hold(0) {
		t.Fatal("no hold on an idle server")
	}
	old.Release(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type dialed struct {
		client *admin.Client
		err    error
	}
	done := make(chan dialed, 1)
	go func() {
		client, err := (&cli{ctx: ctx, state: state, mount: filepath.Join(state, "mnt"), autostart: true}).client()
		done <- dialed{client, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("client returned while the daemon retires: %v", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	lock, err := lockDaemon(state)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	next, err := admin.Listen(socketPath(state))
	if err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { next.Close() })
	s := admin.NewServer()
	go s.Serve(next)
	s.Ready(admin.Welcome{Version: 2}, nil, nil)
	lock.Close()
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.client.Close()
	if r.client.Welcome().Version != 2 {
		t.Fatalf("welcome %+v, want the next daemon's", r.client.Welcome())
	}
}

// TestStopWaitsForTheStartLock holds the start lock as a restart does between
// its old and new daemon: stop waits for it instead of reporting no daemon
// while the new one starts.
func TestStopWaitsForTheStartLock(t *testing.T) {
	state, err := os.MkdirTemp("", "ovnfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	var out strings.Builder
	saved := stderr
	stderr = &colorprofile.Writer{Forward: &out, Profile: colorprofile.ASCII}
	t.Cleanup(func() { stderr = saved })
	lock, err := lockDaemon(state)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- (&stopCmd{}).Run(&cli{ctx: context.Background(), state: state})
	}()
	select {
	case err := <-done:
		lock.Close()
		t.Fatalf("stop returned %v while the start lock was held", err)
	case <-time.After(300 * time.Millisecond):
	}
	lock.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stop did not return after the start lock was released")
	}
	if !strings.Contains(out.String(), "not running") {
		t.Fatalf("stop printed %q, want not running", out.String())
	}
}

// TestStatusRowsShowMemoryError shows parked memory files right after the
// memory row, and no row while none waits.
func TestStatusRowsShowMemoryError(t *testing.T) {
	row := func(rows [][2]string, label string) (int, string) {
		for i, r := range rows {
			if r[0] == label {
				return i, r[1]
			}
		}
		return -1, ""
	}
	rows := statusRows(api.Status{MemoryParked: 2, MemoryError: "no space left on device"}, "")
	i, got := row(rows, "memory error")
	if got != "2 parked: no space left on device" {
		t.Fatalf("memory error row = %q", got)
	}
	if m, _ := row(rows, "memory"); i != m+1 {
		t.Fatalf("memory error row at %d, memory row at %d", i, m)
	}
	if i, _ := row(statusRows(api.Status{}, ""), "memory error"); i != -1 {
		t.Fatal("memory error row shown with no parked file")
	}
}
