package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// server is a repository of 5 commits served under some URL.
type server struct {
	g   *gitTest
	dir string // the repository (work tree)
	ids []object.ID
}

func newServer(t *testing.T) *server {
	g := newGitTest(t)
	dir, ids := g.repo(5)
	// v0 servers only accept advertised wants unless told otherwise.
	g.git(dir, "config", "uploadpack.allowReachableSHA1InWant", "true")
	return &server{g: g, dir: dir, ids: ids}
}

// fetchScenario fetches by id at depth 1, then a newer commit at depth 1
// with the first fetch's shallow boundary and tip as haves, then all of the
// history, checking the target after each step.
func (s *server) fetchScenario(t *testing.T, url string, tweak func(*Options)) {
	t.Helper()
	g := s.g
	dir, gitDir, db := g.target()
	run := func(depth int, want object.ID, haves ...object.ID) Result {
		t.Helper()
		shallow, err := ReadShallow(gitDir)
		if err != nil {
			t.Fatal(err)
		}
		opts := g.options(url, gitDir, db, want)
		opts.Depth, opts.Shallow, opts.Haves = depth, shallow, haves
		if tweak != nil {
			tweak(&opts)
		}
		res, err := Fetch(context.Background(), opts)
		if err != nil {
			t.Fatalf("fetch %s depth %d: %v", want, depth, err)
		}
		g.git(dir, "update-ref", "refs/heads/main", want.String())
		g.verify(dir, gitDir, res, want)
		return res
	}

	res := run(1, s.ids[2])
	if strings.Join(idStrings(res.Shallow), ",") != s.ids[2].String() {
		t.Fatalf("depth 1: shallow %v, want [%s]", res.Shallow, s.ids[2])
	}
	if n := g.git(dir, "rev-list", "--count", "refs/heads/main"); n != "1" {
		t.Fatalf("depth 1 fetch has %s commits", n)
	}

	run(1, s.ids[4], s.ids[2])
	if n := g.git(dir, "rev-list", "--count", "refs/heads/main"); n != "1" {
		t.Fatalf("second depth 1 fetch has %s commits", n)
	}

	// Deepening past the boundary unshallows it.
	res = run(4, s.ids[4], s.ids[4])
	if len(res.Unshallow) == 0 {
		t.Fatalf("depth 4: nothing unshallowed (%+v)", res)
	}
	if n := g.git(dir, "rev-list", "--count", "refs/heads/main"); n != "4" {
		t.Fatalf("depth 4 fetch has %s commits", n)
	}
}

// startDaemon serves the parent directory of s.dir with git daemon and
// returns the git:// URL of the repository.
func (s *server) startDaemon(t *testing.T) string {
	root := filepath.Dir(s.dir)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		conns  = map[net.Conn]bool{}
		closed bool
		wg     sync.WaitGroup
	)
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		closed = true
		for conn := range conns {
			conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	env := s.g.environ()
	wg.Go(func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				conn.Close()
				return
			}
			conns[conn] = true
			mu.Unlock()
			wg.Go(func() {
				defer func() {
					mu.Lock()
					delete(conns, conn)
					mu.Unlock()
					conn.Close()
				}()
				if err := serveInetd(conn, root, env, &wg); err != nil {
					t.Errorf("git daemon --inetd: %v", err)
				}
			})
		}
	})
	return "git://" + l.Addr().String() + "/" + filepath.Base(s.dir)
}

// serveInetd answers one git:// connection with `git daemon --inetd`, which
// reads the request from its stdin and exits with the connection. The test
// owns the listener instead of a listening git daemon: that daemon is a child
// of the git wrapper, which a kill of the wrapper (or the test's own death)
// would leave running.
func serveInetd(conn net.Conn, root string, env []string, wg *sync.WaitGroup) error {
	cmd := exec.Command("git", "daemon", "--inetd", "--log-destination=none", "--export-all", "--base-path="+root, root)
	cmd.Env = env
	cmd.Stdout = conn
	// Bounds the wait for the stdout copy should a child outlive git.
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Ends when the caller closes conn after Wait.
	wg.Go(func() {
		io.Copy(stdin, conn)
		stdin.Close()
	})
	err = cmd.Wait()
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		// A refused request exits nonzero after telling the client why.
		return nil
	}
	return err
}

func TestFetchGitDaemon(t *testing.T) {
	s := newServer(t)
	url := s.startDaemon(t)
	s.fetchScenario(t, url, nil)

	// A want the server does not have surfaces the server's message.
	_, gitDir, db := s.g.target()
	missing := s.g.parseID(strings.Repeat("1", 40))
	_, err := Fetch(context.Background(), s.g.options(url, gitDir, db, missing))
	if err == nil || !strings.Contains(err.Error(), "not our ref") {
		t.Fatalf("missing want: %v", err)
	}
}

// httpServer serves s through git http-backend, recording requests.
type httpServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string // "METHOD path Git-Protocol"
}

type httpConfig struct {
	forceV0 bool
	auth    bool   // require Basic u:p
	header  string // require this "Name: value" header
	// cookie requires this "name=value" cookie; every response sets visit=1.
	cookie string
	// clientCAs serves https, requiring a client certificate they issued.
	clientCAs *x509.CertPool
}

func (s *server) startHTTP(t *testing.T, hc httpConfig) *httpServer {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	env := append(s.g.environ(), "GIT_PROJECT_ROOT="+filepath.Dir(s.dir), "GIT_HTTP_EXPORT_ALL=1")
	if hc.forceV0 {
		// Overrides the HTTP_GIT_PROTOCOL the client's header becomes.
		env = append(env, "GIT_PROTOCOL=version=0")
	}
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: env}
	hs := &httpServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hs.mu.Lock()
		hs.requests = append(hs.requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Git-Protocol"))
		hs.mu.Unlock()
		if old, ok := strings.CutPrefix(r.URL.Path, "/moved/"); ok {
			if r.Method != http.MethodGet {
				http.Error(w, "POST to the old location", http.StatusTeapot)
				return
			}
			u := *r.URL
			u.Path = "/" + old
			http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
			return
		}
		if hc.auth {
			if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "p" {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				http.Error(w, "auth", http.StatusUnauthorized)
				return
			}
		}
		if hc.header != "" {
			name, value, _ := strings.Cut(hc.header, ": ")
			if r.Header.Get(name) != value || len(r.Header.Values(name)) != 1 {
				http.Error(w, "missing header", http.StatusForbidden)
				return
			}
		}
		if hc.cookie != "" {
			http.SetCookie(w, &http.Cookie{Name: "visit", Value: "1", Path: "/"})
			name, value, _ := strings.Cut(hc.cookie, "=")
			if c, err := r.Cookie(name); err != nil || c.Value != value {
				http.Error(w, "missing cookie", http.StatusForbidden)
				return
			}
		}
		backend.ServeHTTP(w, r)
	})
	hs.Server = httptest.NewUnstartedServer(mux)
	if hc.clientCAs != nil {
		hs.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: hc.clientCAs}
		hs.StartTLS()
	} else {
		hs.Start()
	}
	t.Cleanup(hs.Close)
	return hs
}

func (hs *httpServer) log() []string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return append([]string(nil), hs.requests...)
}

func TestFetchHTTPv2(t *testing.T) {
	s := newServer(t)
	hs := s.startHTTP(t, httpConfig{})
	s.fetchScenario(t, hs.URL+"/"+filepath.Base(s.dir)+"/", nil)
	for _, r := range hs.log() {
		if !strings.HasSuffix(r, " version=2") {
			t.Fatalf("request without v2 header: %s", r)
		}
	}
	if !strings.HasPrefix(hs.log()[1], "POST /src/git-upload-pack") {
		t.Fatalf("requests: %v", hs.log())
	}
}

func TestFetchHTTPv0(t *testing.T) {
	s := newServer(t)
	hs := s.startHTTP(t, httpConfig{forceV0: true})
	s.fetchScenario(t, hs.URL+"/"+filepath.Base(s.dir), nil)
	for _, r := range hs.log() {
		// Having seen a v0 advertisement the client stops asking for v2.
		if strings.HasPrefix(r, "POST") && strings.HasSuffix(r, "version=2") {
			t.Fatalf("v0 POST sent with v2 header: %s", r)
		}
	}
}

func TestFetchHTTPRedirectMovesBase(t *testing.T) {
	s := newServer(t)
	hs := s.startHTTP(t, httpConfig{})
	_, gitDir, db := s.g.target()
	opts := s.g.options(hs.URL+"/moved/src", gitDir, db, s.ids[4])
	opts.Depth = 1
	if _, err := Fetch(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	log := hs.log()
	if len(log) != 3 || !strings.HasPrefix(log[2], "POST /src/git-upload-pack") {
		t.Fatalf("requests: %v", log)
	}
	// http.followRedirects=false refuses the redirect.
	opts.Config = s.g.config("[http]\nfollowRedirects = false\n")
	if _, err := Fetch(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "301") {
		t.Fatalf("redirect with followRedirects=false: %v", err)
	}
}

// credentialHelper installs git-credential-fake on PATH, answering get with
// password and logging every operation and its input.
func credentialHelper(t *testing.T, g *gitTest, password string) (logPath string) {
	dir := t.TempDir()
	logPath = filepath.Join(dir, "log")
	// Like git, the operation comes after the configured arguments.
	script := fmt.Sprintf("#!/bin/sh\nfor op; do :; done\necho \"args=$*\" >> %q\ncat >> %q\nif [ \"$op\" = get ]; then echo username=u; echo password=%s; fi\n", logPath, logPath, password)
	if err := os.WriteFile(filepath.Join(dir, "git-credential-fake"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	g.vars["PATH"] = dir + string(os.PathListSeparator) + g.vars["PATH"]
	return logPath
}

// needScripts skips tests whose stand-in programs are #!/bin/sh scripts.
func needScripts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stand-in programs are shell scripts")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
}

func TestFetchHTTPCredentials(t *testing.T) {
	needScripts(t)
	s := newServer(t)
	hs := s.startHTTP(t, httpConfig{auth: true, header: "X-Extra: yes"})
	logPath := credentialHelper(t, s.g, "p")
	cfg := s.g.config(`[credential]
	helper = /nonexistent/reset-below
	helper =
	helper = fake --flag
[http]
	extraHeader = X-Extra: dropped
	extraHeader =
	extraHeader = X-Extra: yes
`)
	s.fetchScenario(t, hs.URL+"/src", func(o *Options) { o.Config = cfg })
	logData, _ := os.ReadFile(logPath)
	log := string(logData)
	host := strings.TrimPrefix(hs.URL, "http://")
	wantGet := "args=--flag get\nprotocol=http\nhost=" + host + "\nwwwauth[]=Basic realm=\"test\"\n"
	if !strings.HasPrefix(log, wantGet) {
		t.Fatalf("helper log:\n%s\nwant prefix:\n%s", log, wantGet)
	}
	if !strings.Contains(log, "args=--flag store\nprotocol=http\nhost="+host+"\nusername=u\npassword=p\n") {
		t.Fatalf("credential not stored:\n%s", log)
	}
	if strings.Contains(log, "erase") {
		t.Fatalf("accepted credential erased:\n%s", log)
	}
}

func TestFetchHTTPRejectedCredentials(t *testing.T) {
	needScripts(t)
	s := newServer(t)
	hs := s.startHTTP(t, httpConfig{auth: true})
	logPath := credentialHelper(t, s.g, "wrong")
	_, gitDir, db := s.g.target()
	opts := s.g.options(hs.URL+"/src", gitDir, db, s.ids[4])
	opts.Config = s.g.config("[credential]\nhelper = fake\n")
	_, err := Fetch(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("wrong password: %v", err)
	}
	log, _ := os.ReadFile(logPath)
	if !strings.Contains(string(log), "args=erase\n") || !strings.Contains(string(log), "password=wrong") {
		t.Fatalf("refused credential not erased:\n%s", log)
	}

	// Without helpers, askpass answers; with neither, fetching fails
	// instead of prompting.
	askpass := filepath.Join(t.TempDir(), "askpass")
	os.WriteFile(askpass, []byte("#!/bin/sh\ncase \"$1\" in Username*) echo u;; *) echo p;; esac\n"), 0o755)
	opts.Config = nil
	s.g.vars["GIT_ASKPASS"] = askpass
	if _, err := Fetch(context.Background(), opts); err != nil {
		t.Fatalf("askpass: %v", err)
	}
	delete(s.g.vars, "GIT_ASKPASS")
	_, gitDir, db = s.g.target()
	opts = s.g.options(hs.URL+"/src", gitDir, db, s.ids[4])
	if _, err := Fetch(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Fatalf("no credentials: %v", err)
	}
}

// fakeSSH writes an ssh stand-in that runs the requested upload-pack
// locally, logging the GIT_PROTOCOL it was given.
func fakeSSH(t *testing.T) (cmd, logPath string) {
	dir := t.TempDir()
	logPath = filepath.Join(dir, "log")
	cmd = filepath.Join(dir, "ssh")
	script := fmt.Sprintf(`#!/bin/sh
echo "args=$* GIT_PROTOCOL=$GIT_PROTOCOL" >> %q
for last; do :; done
eval "exec git upload-pack ${last#git-upload-pack }"
`, logPath)
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, logPath
}

func TestFetchSSH(t *testing.T) {
	needScripts(t)
	for _, tc := range []struct {
		name, variant, want string
	}{
		{"v2", "", "args=-o SendEnv=GIT_PROTOCOL host git-upload-pack '%s' GIT_PROTOCOL=version=2"},
		{"v0", "simple", "args=host git-upload-pack '%s' GIT_PROTOCOL="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t)
			cmd, logPath := fakeSSH(t)
			s.g.vars["GIT_SSH_COMMAND"] = cmd
			if tc.variant != "" {
				s.g.vars["GIT_SSH_VARIANT"] = tc.variant
			}
			s.fetchScenario(t, "host:"+s.dir, nil)
			log, _ := os.ReadFile(logPath)
			first, _, _ := strings.Cut(string(log), "\n")
			if want := fmt.Sprintf(tc.want, s.dir); first != want {
				t.Fatalf("ssh invoked as\n%s\nwant\n%s", first, want)
			}
		})
	}
}

func TestFetchSSHStderrInError(t *testing.T) {
	needScripts(t)
	s := newServer(t)
	cmd := filepath.Join(t.TempDir(), "ssh")
	os.WriteFile(cmd, []byte("#!/bin/sh\necho 'Permission denied (publickey).' >&2\nexit 255\n"), 0o755)
	s.g.vars["GIT_SSH_COMMAND"] = cmd
	_, gitDir, db := s.g.target()
	_, err := Fetch(context.Background(), s.g.options("ssh://git@example.com/r.git", gitDir, db, s.ids[4]))
	if err == nil || !strings.Contains(err.Error(), "Permission denied (publickey).") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchLocalURLs(t *testing.T) {
	s := newServer(t)
	t.Run("file", func(t *testing.T) { s.fetchScenario(t, "file://"+filepath.ToSlash(s.dir), nil) })
	t.Run("path", func(t *testing.T) { s.fetchScenario(t, s.dir, nil) })
	t.Run("insteadOf", func(t *testing.T) {
		cfg := s.g.config(fmt.Sprintf("[url %q]\n\tinsteadOf = short:\n[url \"bogus::\"]\n\tinsteadOf = sh\n", "file://"+filepath.ToSlash(filepath.Dir(s.dir))+"/"))
		s.fetchScenario(t, "short:"+filepath.Base(s.dir), func(o *Options) { o.Config = cfg })
	})
}

func TestFetchRefusesTransports(t *testing.T) {
	s := newServer(t)
	_, gitDir, db := s.g.target()
	for _, url := range []string{"ext::sh -c touch% /tmp/pwned", "fd::0", "ftp://example.com/r"} {
		opts := s.g.options(url, gitDir, db, s.ids[0])
		opts.OpenLocal = func(string) (*object.DB, []object.ID, error) {
			t.Fatalf("%s: opened something", url)
			return nil, nil, nil
		}
		if _, err := Fetch(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%s: %v", url, err)
		}
	}
	// insteadOf rewriting happens before the check.
	opts := s.g.options("evil:x", gitDir, db, s.ids[0])
	opts.Config = s.g.config("[url \"ext::sh -c \"]\n\tinsteadOf = evil:\n")
	if _, err := Fetch(context.Background(), opts); !errors.Is(err, errNotAllowed) || !strings.Contains(err.Error(), "remote helper 'ext'") {
		t.Errorf("rewritten to ext: %v", err)
	}
}
