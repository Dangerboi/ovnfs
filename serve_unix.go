//go:build !windows

package main

import (
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/nfs"
	"github.com/stencil-hq/ovnfs/stats"
)

// serveNFS serves NFSv3 and NFSv4 on the persistent export. NFSv4 clients
// get read delegations, recalled whenever the export reports a change made
// behind their back. Without settings.Sync, stable writes and COMMIT sync
// nothing; with settings.DropAppleDouble, macOS ._ files are not kept.
func serveNFS(l net.Listener, e *export.Export, rec *stats.Recorder, settings mountSettings) error {
	srv := &nfs.Server{FS: e.NFS(), Concurrency: 64, NoSync: !settings.Sync, DropAppleDouble: settings.DropAppleDouble, Stats: rec}
	if os.Getenv("LOG_LEVEL") != "" {
		srv.Logf = log.Printf
	}
	e.OnRecall(func(r export.Recall) {
		if r.Retired {
			srv.RetireRange(r.First, r.Last)
		} else {
			srv.RecallRange(r.First, r.Last)
		}
	})
	return srv.Serve(l)
}

// refreshRoot makes the local NFS client revalidate the export root now
// rather than after its attribute timeout: the FSSTAT reply carries the
// root's attributes, and its changed mtime purges the client's cached names,
// including a negative entry for a checkout created a moment ago. The caller
// must hold no registry lock, since the server answers the FSSTAT, and must run
// it through fileListener.serving, since without a server it hangs.
func refreshRoot(mount string) {
	var st syscall.Statfs_t
	_ = syscall.Statfs(mount, &st)
}

// refreshPaths makes the local NFS client take the attributes of the paths of
// checkout the daemon changed behind it (checkout.Config.Invalidate) now
// rather than when its attribute cache expires, up to a minute later: until
// then it serves a changed file's old content and a directory's old names
// from its caches, and NFSv3 gives a server no way to say so. A SETATTR's
// reply carries the object's attributes from before and after it, and a
// client that finds they are not the ones it cached drops what it cached with
// them. So each changed file gets a chmod to the mode it already has, and
// before it, from the checkout's root down, each directory on the way to it:
// any of them may hold a new entry's name cached as missing or a removed
// one's as present. Of a tree, only the directories down to its root are
// refreshed.
//
// lstat is the daemon's own view of an export path: the mount would answer
// with the mode it has cached. A symlink is left out, since chmod would follow
// it, and a changed one is a new entry of its directory anyway. Failures are
// ignored: a path that is gone has nothing cached once its directory is fresh.
//
// The caller must hold no lock a file call takes, since the server answers
// the SETATTRs, and must run it through fileListener.serving.
func refreshPaths(mount, checkout string, paths, trees []string, lstat func(name string) (os.FileInfo, error)) {
	done := make(map[string]bool)
	refresh := func(name string) {
		if done[name] {
			return
		}
		done[name] = true
		info, err := lstat(name)
		if err != nil || !info.IsDir() && !info.Mode().IsRegular() {
			return
		}
		_ = os.Chmod(filepath.Join(mount, filepath.FromSlash(name)), info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
	}
	for _, list := range [][]string{trees, paths} {
		for _, p := range list {
			name := path.Join(checkout, p)
			for i := strings.IndexByte(name, '/'); i >= 0; {
				refresh(name[:i])
				next := strings.IndexByte(name[i+1:], '/')
				if next < 0 {
					break
				}
				i += 1 + next
			}
			refresh(name)
		}
	}
}
