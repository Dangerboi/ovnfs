//go:build !windows

// Package nfstest drives an in-process NFSv3 server without a kernel mount.
package nfstest

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	client "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"

	"github.com/stencil-hq/ovnfs/nfs"
)

// NFSv3 status values are retained separately from transport and decoding errors.
const (
	OK     uint32 = 0
	NoEnt  uint32 = 2
	Access uint32 = 13
	Exist  uint32 = 17
	XDev   uint32 = 18
	IsDir  uint32 = 21
	Inval  uint32 = 22
	Stale  uint32 = 70
)

// Stability levels for NFSv3 WRITE.
const (
	Unstable uint32 = iota
	DataSync
	FileSync
)

// Client is an unprivileged TCP RPC client and its mounted export root handle.
// The helpers return the wire status unchanged; err describes only RPC or XDR failures.
type Client struct {
	RPC  *rpc.Client
	Root []byte
	auth rpc.Auth
}

// Start serves backend on an ephemeral loopback port, mounts its root, and registers cleanup.
func Start(t testing.TB, backend nfs.Backend) *Client {
	t.Helper()
	return Serve(t, &nfs.Server{FS: backend})
}

// Serve drives a configured server over the same TCP transport as Start.
func Serve(t testing.TB, srv *nfs.Server) *Client {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	t.Cleanup(func() { _ = l.Close(); <-done })
	// The client binds a random local port from the ephemeral range and
	// retries a taken one only for privileged ports, so tests running in
	// parallel sometimes collide.
	raw, err := rpc.DialTCP("tcp", l.Addr().String(), false)
	for range 10 {
		if !errors.Is(err, syscall.EADDRINUSE) {
			break
		}
		raw, err = rpc.DialTCP("tcp", l.Addr().String(), false)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(raw.Close)
	c := &Client{RPC: raw, auth: rpc.NewAuthUnix("nfstest", uint32(os.Getuid()), uint32(os.Getgid())).Auth()}
	root, status, err := c.Mount("/")
	if err != nil || status != OK {
		t.Fatalf("MOUNT: status %d, error %v", status, err)
	}
	c.Root = root
	return c
}

// call performs a procedure and consumes only the leading NFS or MOUNT status.
func (c *Client) call(program, procedure uint32, args any) (io.ReadSeeker, uint32, error) {
	req := struct {
		rpc.Header
		Args any
	}{
		Rpcvers: 2, Prog: program, Vers: 3, Proc: procedure, Cred: c.auth, Verf: rpc.AuthNull,
		Args: args,
	}
	r, err := c.RPC.Call(&req)
	if err != nil {
		return nil, 0, err
	}
	status, err := xdr.ReadUint32(r)
	return r, status, err
}

// Mount performs MOUNTv3 directly on the server port, without consulting rpcbind.
func (c *Client) Mount(path string) ([]byte, uint32, error) {
	r, status, err := c.call(client.MountProg, client.MountProc3MNT, path)
	if err != nil || status != OK {
		return nil, status, err
	}
	var result struct {
		Handle []byte
		Auth   []uint32
	}
	err = xdr.Read(r, &result)
	return result.Handle, status, err
}

// Lookup resolves one directory entry, returning its opaque handle and attributes.
func (c *Client) Lookup(dir []byte, name string) ([]byte, client.Fattr, uint32, error) {
	r, status, err := c.call(client.Nfs3Prog, client.NFSProc3Lookup, client.Diropargs3{FH: dir, Filename: name})
	var result struct {
		Handle []byte
		Attr   client.PostOpAttr
		Dir    client.PostOpAttr
	}
	if err == nil && status == OK {
		err = xdr.Read(r, &result)
	}
	return result.Handle, result.Attr.Attr, status, err
}

// GetAttr retrieves attributes even for handles no longer reachable by name.
func (c *Client) GetAttr(handle []byte) (client.Fattr, uint32, error) {
	r, status, err := c.call(client.Nfs3Prog, client.NFSProc3GetAttr, handle)
	var attr client.Fattr
	if err == nil && status == OK {
		err = xdr.Read(r, &attr)
	}
	return attr, status, err
}

// SetSize performs NFSv3 SETATTR setting only size on handle.
func (c *Client) SetSize(handle []byte, size uint64) (client.WccData, uint32, error) {
	args := struct {
		Handle []byte
		Attr   client.Sattr3
		Guard  bool
	}{
		Handle: handle,
		Attr:   client.Sattr3{Size: client.SetSize{SetIt: true, Size: size}},
	}
	r, status, err := c.call(client.Nfs3Prog, client.NFSProc3SetAttr, args)
	var wcc client.WccData
	if err == nil && status == OK {
		err = xdr.Read(r, &wcc)
	}
	return wcc, status, err
}

// Access performs NFSv3 ACCESS for mask on handle and returns the granted bits.
func (c *Client) Access(handle []byte, mask uint32) (uint32, uint32, error) {
	args := struct {
		Handle []byte
		Access uint32
	}{handle, mask}
	r, status, err := c.call(client.Nfs3Prog, client.NFSProc3Access, args)
	var result struct {
		Attr   client.PostOpAttr
		Access uint32
	}
	if err == nil && status == OK {
		err = xdr.Read(r, &result)
	}
	return result.Access, status, err
}

// Read reads a bounded range and reports the protocol EOF flag.
func (c *Client) Read(handle []byte, offset uint64, count uint32) ([]byte, bool, uint32, error) {
	args := struct {
		Handle []byte
		Offset uint64
		Count  uint32
	}{handle, offset, count}
	r, status, err := c.call(client.Nfs3Prog, client.NFSProc3Read, args)
	var result struct {
		Attr  client.PostOpAttr
		Count uint32
		EOF   bool
		Data  []byte
	}
	if err == nil && status == OK {
		err = xdr.Read(r, &result)
		if err == nil && result.Count != uint32(len(result.Data)) {
			err = fmt.Errorf("READ count %d differs from data length %d", result.Count, len(result.Data))
		}
	}
	return result.Data, result.EOF, status, err
}

// Write issues WRITE at the requested stability and returns the count and
// stability level acknowledged by the server.
func (c *Client) Write(handle []byte, offset uint64, data []byte, stable uint32) (count, committed, status uint32, err error) {
	args := struct {
		Handle        []byte
		Offset        uint64
		Count, Stable uint32
		Data          []byte
	}{handle, offset, uint32(len(data)), stable, data}
	r, status, err := c.call(client.Nfs3Prog, client.NFSProc3Write, args)
	var result struct {
		Wcc              client.WccData
		Count, Committed uint32
		Verifier         uint64
	}
	if err == nil && status == OK {
		err = xdr.Read(r, &result)
	}
	return result.Count, result.Committed, status, err
}

// Commit requests a barrier for the range; a zero count means through EOF.
func (c *Client) Commit(handle []byte, offset uint64, count uint32) (uint32, error) {
	args := struct {
		Handle []byte
		Offset uint64
		Count  uint32
	}{handle, offset, count}
	r, status, err := c.call(client.Nfs3Prog, client.NFSProc3Commit, args)
	if err == nil && status == OK {
		var result struct {
			Wcc      client.WccData
			Verifier uint64
		}
		err = xdr.Read(r, &result)
	}
	return status, err
}

// modeAttrs sets only the requested permission bits, leaving ownership and times unchanged.
func modeAttrs(mode os.FileMode) client.Sattr3 {
	return client.Sattr3{Mode: client.SetMode{SetIt: true, Mode: uint32(mode.Perm())}}
}

// created decodes the common successful CREATE, MKDIR and SYMLINK response.
func created(r io.Reader, status uint32, err error) ([]byte, uint32, error) {
	var result struct {
		Handle client.PostOpFH3
		Attr   client.PostOpAttr
		Wcc    client.WccData
	}
	if err == nil && status == OK {
		err = xdr.Read(r, &result)
	}
	return result.Handle.FH, status, err
}

// Create creates or truncates a regular file using UNCHECKED mode.
func (c *Client) Create(dir []byte, name string, mode os.FileMode) ([]byte, uint32, error) {
	args := struct {
		Where client.Diropargs3
		How   uint32
		Attr  client.Sattr3
	}{client.Diropargs3{FH: dir, Filename: name}, 0, modeAttrs(mode)}
	r, status, err := c.call(client.Nfs3Prog, client.NFSProc3Create, args)
	return created(r, status, err)
}

// Mkdir creates a directory under an existing directory handle.
func (c *Client) Mkdir(dir []byte, name string, mode os.FileMode) ([]byte, uint32, error) {
	args := struct {
		Where client.Diropargs3
		Attr  client.Sattr3
	}{client.Diropargs3{FH: dir, Filename: name}, modeAttrs(mode)}
	r, status, err := c.call(client.Nfs3Prog, client.NFSProc3Mkdir, args)
	return created(r, status, err)
}

// Remove unlinks a non-directory entry.
func (c *Client) Remove(dir []byte, name string) (uint32, error) {
	_, status, err := c.call(client.Nfs3Prog, client.NFSProc3Remove, client.Diropargs3{FH: dir, Filename: name})
	return status, err
}

// Rmdir removes an empty directory, allowing opaque-directory recreation to be exercised.
func (c *Client) Rmdir(dir []byte, name string) (uint32, error) {
	_, status, err := c.call(client.Nfs3Prog, client.NFSProc3RmDir, client.Diropargs3{FH: dir, Filename: name})
	return status, err
}

// Rename moves a directory entry and preserves EXDEV and permission wire statuses.
func (c *Client) Rename(from []byte, old string, to []byte, name string) (uint32, error) {
	args := struct{ From, To client.Diropargs3 }{client.Diropargs3{FH: from, Filename: old}, client.Diropargs3{FH: to, Filename: name}}
	_, status, err := c.call(client.Nfs3Prog, client.NFSProc3Rename, args)
	return status, err
}

// ReadDirPlus retrieves every page, including attributes and handles for exposed children.
func (c *Client) ReadDirPlus(dir []byte) ([]client.EntryPlus, uint32, error) {
	var entries []client.EntryPlus
	var cookie, verifier uint64
	for {
		args := struct {
			Handle             []byte
			Cookie, Verifier   uint64
			DirCount, MaxCount uint32
		}{dir, cookie, verifier, 4096, 32768}
		r, status, err := c.call(client.Nfs3Prog, client.NFSProc3ReadDirPlus, args)
		if err != nil || status != OK {
			return entries, status, err
		}
		var head struct {
			Attr     client.PostOpAttr
			Verifier uint64
		}
		if err := xdr.Read(r, &head); err != nil {
			return entries, status, err
		}
		verifier = head.Verifier
		previous := cookie
		for {
			var follows bool
			if err := xdr.Read(r, &follows); err != nil {
				return entries, status, err
			}
			if !follows {
				break
			}
			var entry client.EntryPlus
			if err := xdr.Read(r, &entry); err != nil {
				return entries, status, err
			}
			cookie = entry.Cookie
			entries = append(entries, entry)
		}
		var eof bool
		if err := xdr.Read(r, &eof); err != nil {
			return entries, status, err
		}
		if eof {
			return entries, status, nil
		}
		if cookie == previous {
			return entries, status, fmt.Errorf("READDIRPLUS did not advance cookie %d", cookie)
		}
	}
}

// Symlink creates a symbolic link without resolving its target.
func (c *Client) Symlink(dir []byte, name, target string) ([]byte, uint32, error) {
	args := struct {
		Where  client.Diropargs3
		Attr   client.Sattr3
		Target string
	}{client.Diropargs3{FH: dir, Filename: name}, modeAttrs(0777), target}
	r, status, err := c.call(client.Nfs3Prog, client.NFSProc3Symlink, args)
	return created(r, status, err)
}

// Readlink returns a symbolic link's stored target.
func (c *Client) Readlink(handle []byte) (string, uint32, error) {
	r, status, err := c.call(client.Nfs3Prog, client.NFSProc3Readlink, handle)
	var result struct {
		Attr   client.PostOpAttr
		Target string
	}
	if err == nil && status == OK {
		err = xdr.Read(r, &result)
	}
	return result.Target, status, err
}
