//go:build darwin || linux || windows

package admin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// testHandler adapts a function to the request interface.
type testHandler func(context.Context, string, json.RawMessage) (any, error)

// Call executes the test operation.
func (h testHandler) Call(ctx context.Context, op string, args json.RawMessage) (any, error) {
	return h(ctx, op, args)
}

// testServer creates an authenticated server with a short Unix socket path.
func testServer(t *testing.T, h Handler, events func(context.Context) <-chan Event) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ovn-admin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go Serve(l, Welcome{Version: 1, Ops: []string{"echo", "slow", "subscribe"}}, h, events)
	return path
}

// testClient opens a client with cleanup and a bounded request context.
func testClient(t *testing.T, path string) (*Client, context.Context) {
	t.Helper()
	c, err := Dial(path, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return c, ctx
}

// TestRoundTrip exercises negotiation, request replies and unsupported operations.
func TestRoundTrip(t *testing.T) {
	path := testServer(t, testHandler(func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		return json.RawMessage(args), nil
	}), nil)
	c, ctx := testClient(t, path)
	if c.Welcome().Version != 1 {
		t.Fatal("missing welcome")
	}
	got, err := c.Call(ctx, "echo", map[string]string{"value": "hello"})
	if err != nil || string(got) != `{"value":"hello"}` {
		t.Fatalf("reply %s: %v", got, err)
	}
	_, err = c.Call(ctx, "missing", struct{}{})
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "unsupported" {
		t.Fatalf("unsupported: %v", err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	writeFrame(conn, map[string]any{"hello": struct{}{}})
	if _, err := readFrame(conn); err != nil {
		t.Fatal(err)
	}
	writeFrame(conn, map[string]any{"id": 1, "missing": struct{}{}})
	reply, err := readFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reply["error"], &remote); err != nil || remote.Code != "unsupported" {
		t.Fatalf("server unsupported: %s %v", reply["error"], err)
	}
	writeFrame(conn, map[string]any{"id": 2, "echo": nil})
	reply, err = readFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reply["error"], &remote); err != nil || remote.Code != "invalid" {
		t.Fatalf("invalid: %s %v", reply["error"], err)
	}
}

// TestServerHoldsHelloUntilReady accepts a client before the daemon can answer
// it: the hello waits for Ready, and a listener closed before Ready fails the
// waiting client instead of leaving it waiting.
func TestServerHoldsHelloUntilReady(t *testing.T) {
	dir, err := os.MkdirTemp("", "ovn-admin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dial := func(path string) <-chan error {
		done := make(chan error, 1)
		go func() {
			c, err := Dial(path, "test")
			if err == nil {
				if c.Welcome().Version != 1 {
					err = errors.New("missing welcome")
				}
				c.Close()
			}
			done <- err
		}()
		return done
	}
	listen := func(name string) (net.Listener, string) {
		path := filepath.Join(dir, name)
		l, err := Listen(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l, path
	}

	l, path := listen("ready")
	s := NewServer()
	go s.Serve(l)
	done := dial(path)
	select {
	case err := <-done:
		t.Fatalf("hello answered before Ready: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	s.Ready(Welcome{Version: 1}, nil, nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hello not answered after Ready")
	}

	l, path = listen("closed")
	go NewServer().Serve(l)
	done = dial(path)
	time.Sleep(200 * time.Millisecond)
	l.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("dial succeeded on a server that never became ready")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client still waiting after the listener closed")
	}
}

// TestHoldRetiresAnIdleServer holds hellos only once no connection has been
// open for the idle period; a released hold welcomes the held client, and a
// retiring one answers it with CodeRetired.
func TestHoldRetiresAnIdleServer(t *testing.T) {
	dir, err := os.MkdirTemp("", "ovn-admin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	s := NewServer()
	go s.Serve(l)
	s.Ready(Welcome{Version: 1}, nil, nil)
	dial := func() <-chan error {
		done := make(chan error, 1)
		go func() {
			c, err := Dial(path, "test")
			if err == nil {
				c.Close()
			}
			done <- err
		}()
		return done
	}
	await := func(done <-chan error) error {
		t.Helper()
		select {
		case err := <-done:
			return err
		case <-time.After(3 * time.Second):
			t.Fatal("hello not answered")
			return nil
		}
	}

	if s.Hold(time.Hour) {
		t.Fatal("held a server that just became ready")
	}
	c, err := Dial(path, "test")
	if err != nil {
		t.Fatal(err)
	}
	if s.Hold(0) {
		c.Close()
		t.Fatal("held a server with an open connection")
	}
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !s.Hold(0) {
		if time.Now().After(deadline) {
			t.Fatal("no hold after the last connection closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	done := dial()
	select {
	case err := <-done:
		t.Fatalf("hello answered during a hold: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	s.Release(false)
	if err := await(done); err != nil {
		t.Fatalf("held hello after the hold ended: %v", err)
	}

	for !s.Hold(0) {
		time.Sleep(10 * time.Millisecond)
	}
	done = dial()
	time.Sleep(100 * time.Millisecond)
	s.Release(true)
	for _, done := range []<-chan error{done, dial()} {
		err := await(done)
		if remote, ok := errors.AsType[*RemoteError](err); !ok || remote.Code != CodeRetired {
			t.Fatalf("hello to a retiring server: %v", err)
		}
	}
}

// TestConcurrentOutOfOrder verifies a slow call cannot block a later fast reply.
func TestConcurrentOutOfOrder(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	path := testServer(t, testHandler(func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		if op == "slow" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return map[string]string{"op": op}, nil
	}), nil)
	c, ctx := testClient(t, path)
	slow := make(chan error, 1)
	go func() { _, err := c.Call(ctx, "slow", struct{}{}); slow <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	got, err := c.Call(ctx, "echo", struct{}{})
	close(release)
	if err != nil || string(got) != `{"op":"echo"}` {
		t.Fatalf("fast reply %s %v", got, err)
	}
	select {
	case err := <-slow:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// TestOversizedFrameCloses verifies that frame limits terminate the connection.
func TestOversizedFrameCloses(t *testing.T) {
	path := testServer(t, testHandler(func(context.Context, string, json.RawMessage) (any, error) { return struct{}{}, nil }), nil)
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	writeFrame(conn, map[string]any{"hello": struct{}{}})
	if _, err := readFrame(conn); err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], maxFrame+1)
	if _, err := conn.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	var buf [1]byte
	if _, err := conn.Read(buf[:]); err == nil {
		t.Fatal("oversized connection remains open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("connection was not closed")
	}
}

// TestSubscribeReceivesEvents verifies acknowledgment precedes queued notifications.
func TestSubscribeReceivesEvents(t *testing.T) {
	path := testServer(t, testHandler(func(context.Context, string, json.RawMessage) (any, error) { return struct{}{}, nil }), func(ctx context.Context) <-chan Event {
		ch := make(chan Event, 1)
		ch <- Event{Name: "created", Payload: map[string]string{"name": "agent"}}
		return ch
	})
	c, ctx := testClient(t, path)
	stream, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-stream:
		if string(event) != `{"created":{"name":"agent"}}` {
			t.Fatalf("event %s", event)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// TestPeerSameUser verifies a same-user peer passes the credential check and a
// live socket is never replaced.
func TestPeerSameUser(t *testing.T) {
	path := testServer(t, testHandler(func(context.Context, string, json.RawMessage) (any, error) { return struct{}{}, nil }), nil)
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := CheckPeer(conn); err != nil {
		t.Fatal(err)
	}
	if l, err := Listen(path); err == nil {
		l.Close()
		t.Fatal("replaced a listening socket")
	}
}

// TestListenStaleRace starts servers together on one stale socket: exactly
// one may listen, or a loser removes the winner's socket and both listen.
func TestListenStaleRace(t *testing.T) {
	dir, err := os.MkdirTemp("", "ovn-race-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	// Each round's winner closes without unlinking: the next round's stale socket.
	for range 200 {
		start := make(chan struct{})
		listeners := make(chan net.Listener, 8)
		var wg sync.WaitGroup
		for range cap(listeners) {
			wg.Go(func() {
				<-start
				if l, err := Listen(path); err == nil {
					listeners <- l
				}
			})
		}
		close(start)
		wg.Wait()
		close(listeners)
		won := 0
		for l := range listeners {
			won++
			l.(*net.UnixListener).SetUnlinkOnClose(false)
			l.Close()
		}
		if won != 1 {
			t.Fatalf("%d servers listen on one socket, want 1", won)
		}
	}
}

// TestListenStaleAndRegular verifies stale cleanup never removes a regular file.
func TestListenStaleAndRegular(t *testing.T) {
	dir, err := os.MkdirTemp("", "ovn-stale-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	fresh, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Close()
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if l, err := Listen(path); err == nil {
		l.Close()
		t.Fatal("replaced regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("regular file changed: %s %v", data, err)
	}
}
