package checkout

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/store"
)

// mirror is where an attached checkout's committed content comes from when this
// machine can reach the source's git upstream: one shallow, bare repository
// (<state>/mirror.git) into which the commits remote bases name
// (base.Remote.Origins) are fetched in the background. Reads use the link until
// a commit has arrived, and for anything no fetched commit holds: dirty and
// untracked files, or every file when this machine lacks the credentials.
// It implements base.Mirror for every remote base of the registry.
type mirror struct {
	dir     string
	fault   func(string)
	ctx     context.Context
	cancel  context.CancelFunc
	pending sync.WaitGroup
	fetchMu sync.Mutex // fetches share the repository's shallow file

	mu      sync.RWMutex
	repo    *git.Repo // nil until the first origin is wanted
	objects *git.Objects
	blobs   map[store.OID]struct{} // every blob of every fetched commit
	commits map[string]bool        // fetched, or being fetched
}

func newMirror(dir string, fault func(string)) *mirror {
	ctx, cancel := context.WithCancel(context.Background())
	return &mirror{dir: dir, fault: fault, ctx: ctx, cancel: cancel, blobs: map[store.OID]struct{}{}, commits: map[string]bool{}}
}

// want starts fetching each origin's commit unless it is fetched or on its way.
// A commit already in the repository (from before a restart) is only indexed.
func (m *mirror) want(origins []git.Origin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return
	}
	for _, o := range origins {
		if m.commits[o.Commit] {
			continue
		}
		m.commits[o.Commit] = true
		m.pending.Go(func() {
			if err := m.load(o); err != nil && m.ctx.Err() == nil {
				m.mu.Lock()
				delete(m.commits, o.Commit) // the next attach naming it tries again
				m.mu.Unlock()
				m.fault(fmt.Sprintf("git mirror: fetch %s from %s: %v; its files come over the link instead", o.Commit, o.URL, err))
			}
		})
	}
}

// load fetches o's commit if needed and indexes its blobs.
func (m *mirror) load(o git.Origin) error {
	repo, err := m.open()
	if err != nil {
		return err
	}
	m.fetchMu.Lock()
	have, err := repo.HasOrigin(m.ctx, o.Commit)
	if err == nil && !have {
		err = repo.FetchOrigin(m.ctx, o)
	}
	m.fetchMu.Unlock()
	if err != nil {
		return err
	}
	entries, err := repo.LsTree(m.ctx, o.Commit)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		if e.Type == "blob" {
			m.blobs[store.OID(e.OID)] = struct{}{}
		}
	}
	return nil
}

// open creates the repository on first use, so a machine that never attaches
// from a repository with an upstream never creates one.
func (m *mirror) open() (*git.Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.repo == nil {
		repo, err := git.InitBare(filepath.Clean(m.dir))
		if err != nil {
			return nil, err
		}
		m.repo = repo
	}
	return m.repo, nil
}

// Has reports whether a fetched commit holds blob id.
func (m *mirror) Has(id store.OID) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.blobs[id]
	return ok
}

// Blob reads blob id from the repository.
func (m *mirror) Blob(id store.OID) ([]byte, error) {
	m.mu.Lock()
	if m.objects == nil {
		if m.repo == nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("git mirror has no repository")
		}
		objects, err := m.repo.Objects()
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		m.objects = objects
	}
	objects := m.objects
	m.mu.Unlock()
	return objects.Blob(string(id))
}

// close stops running fetches and the object reader.
func (m *mirror) close() {
	m.mu.Lock()
	m.cancel()
	m.mu.Unlock()
	m.pending.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objects != nil {
		m.objects.Close()
	}
}
