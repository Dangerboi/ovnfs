package fetch

import (
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"slices"
	"strings"
)

// endpoint is a remote repository address after URL rewriting, split the way
// the transport for its scheme needs it.
type endpoint struct {
	// transport is the protocol name protocol.allow speaks of: "ssh", "git",
	// "http", "https", "file", or a remote helper's name.
	transport string
	// helper marks a URL git hands to git-remote-<transport>: remote is the
	// URL it is given as the remote, path the address.
	helper bool
	remote string
	// user is the ssh login ("user" or "user:password", passed to ssh as is).
	user string
	// host is the server name without brackets; for git:// hostParam is the
	// "host=" extra parameter, which keeps brackets and the port.
	host, port, hostParam string
	// path is the repository path: for ssh and git what the server is asked
	// for, for file a local filesystem path, for a remote helper its address.
	path string
	// url is the parsed URL for http and https.
	url *url.URL
}

// errNotAllowed reports a URL whose transport the caller does not permit.
var errNotAllowed = errors.New("transport not allowed")

// isSchemeChar reports bytes allowed in a URL scheme or remote helper name.
func isSchemeChar(c byte, first bool) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		return true
	case first:
		return false
	case '0' <= c && c <= '9', c == '+', c == '-', c == '.':
		return true
	}
	return false
}

// schemeOf returns the leading "<scheme>" of s when it is followed by sep.
func schemeOf(s, sep string) (string, bool) {
	i := strings.Index(s, sep)
	if i <= 0 {
		return "", false
	}
	for j := range i {
		if !isSchemeChar(s[j], j == 0) {
			return "", false
		}
	}
	return s[:i], true
}

// isLocalPath implements urls.adoc's disambiguation: an address with no
// colon, or a slash before the first colon, is a local path rather than the
// scp-like "[user@]host:path"; so, on Windows, is a valid path with a drive
// prefix. A backslash counts for nothing, as with Git for Windows.
func isLocalPath(s string) bool {
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return true
	}
	if slash := strings.IndexByte(s, '/'); slash >= 0 && slash < colon {
		return true
	}
	return runtime.GOOS == "windows" && dosDrivePrefix(s) > 0 && validWin32Path(s)
}

// dosDrivePrefix returns the length of the drive prefix ("C:") s starts
// with, or 0. Like Git for Windows, it takes any ASCII character or UTF-8
// sequence for a drive: subst can assign virtually any.
func dosDrivePrefix(s string) int {
	i := 1
	if s != "" && s[0] >= 0x80 {
		for i < min(len(s), 4) && s[i] >= 0x80 {
			i++
		}
	}
	if s != "" && i < len(s) && s[i] == ':' {
		return i + 1
	}
	return 0
}

// validWin32Path ports Git for Windows' is_valid_win32_path with the
// default core.protectNTFS: after the drive prefix, no character Windows
// forbids in names, no component ending in a space or period other than "."
// and "..", and no component naming a device.
func validWin32Path(p string) bool {
	for seg := range strings.FieldsFuncSeq(p[dosDrivePrefix(p):], isDirSep) {
		if reservedName(seg) || strings.ContainsFunc(seg, func(r rune) bool {
			return r < 0x20 || strings.ContainsRune(`:<>"|?*`, r)
		}) {
			return false
		}
		if last := seg[len(seg)-1]; (last == ' ' || last == '.') && seg != "." && seg != ".." {
			return false
		}
	}
	return true
}

func isDirSep(r rune) bool { return r == '/' || r == '\\' }

// reservedName reports a path component Windows takes for a device: AUX,
// CON, CONIN$, CONOUT$, NUL, PRN, COM1-COM9 or LPT0-LPT9 in any case,
// followed by nothing, spaces, a period or a colon.
func reservedName(seg string) bool {
	hasPrefix := func(name string) bool {
		return len(seg) >= len(name) && strings.EqualFold(seg[:len(name)], name)
	}
	var n int
	switch {
	case hasPrefix("CONIN$"):
		n = 6
	case hasPrefix("CONOUT$"):
		n = 7
	case hasPrefix("AUX"), hasPrefix("CON"), hasPrefix("NUL"), hasPrefix("PRN"):
		n = 3
	case len(seg) > 3 && (hasPrefix("COM") && '1' <= seg[3] && seg[3] <= '9' ||
		hasPrefix("LPT") && '0' <= seg[3] && seg[3] <= '9'):
		n = 4
	default:
		return false
	}
	rest := strings.TrimLeft(seg[n:], " ")
	return rest == "" || rest[0] == '.' || rest[0] == ':'
}

// parseEndpoint classifies a (rewritten) remote address and refuses any
// transport not in allowed before anything connects.
func parseEndpoint(raw string, allowed []string) (*endpoint, error) {
	ep, err := classify(raw)
	if err != nil {
		return nil, err
	}
	for _, s := range []string{ep.user, ep.host, ep.port, ep.hostParam, ep.path, ep.remote} {
		if strings.ContainsRune(s, 0) {
			return nil, fmt.Errorf("fetch: NUL byte in %q", redactURL(raw))
		}
	}
	if slices.Contains(allowed, ep.transport) {
		return ep, nil
	}
	if ep.helper {
		return nil, fmt.Errorf("fetch: %w: remote helper '%s' (from %q)", errNotAllowed, ep.transport, redactURL(raw))
	}
	return nil, fmt.Errorf("fetch: %w: '%s' (from %q)", errNotAllowed, ep.transport, redactURL(raw))
}

func classify(raw string) (*endpoint, error) {
	// "<transport>::<address>" names a remote helper (ext::, fd::), given the
	// address. Whatever the name, it is a helper: "https::x" runs
	// git-remote-https, as with git, never the native transport.
	if helper, ok := schemeOf(raw, "::"); ok {
		return &endpoint{transport: helper, helper: true, remote: raw, path: raw[len(helper)+2:]}, nil
	}
	if scheme, ok := schemeOf(raw, "://"); ok {
		rest := raw[len(scheme)+3:]
		switch strings.ToLower(scheme) {
		case "ssh", "git+ssh", "ssh+git":
			return parseSSHURL(rest)
		case "git":
			return parseGitURL(rest)
		case "http", "https":
			return parseHTTPURL(raw, strings.ToLower(scheme))
		case "file":
			return parseFileURL(rest)
		default:
			// Any other scheme is git-remote-<scheme>'s, given the whole URL.
			return &endpoint{transport: scheme, helper: true, remote: raw, path: raw}, nil
		}
	}
	if isLocalPath(raw) {
		return &endpoint{transport: "file", path: raw}, nil
	}
	return parseSCP(raw)
}

// splitPort splits "host:port" when everything after the last colon is
// digits (possibly none: "host:" has no port). Otherwise the colon belongs
// to the host, as with git, which passes "h:abc" to ssh as the host.
func splitPort(hostport string) (host, port string) {
	if strings.HasPrefix(hostport, "[") {
		if end := strings.IndexByte(hostport, ']'); end > 0 {
			host, rest := hostport[1:end], hostport[end+1:]
			if p, ok := strings.CutPrefix(rest, ":"); ok && allDigits(p) {
				return host, p
			}
			return host + rest, ""
		}
	}
	if i := strings.LastIndexByte(hostport, ':'); i >= 0 && allDigits(hostport[i+1:]) {
		return hostport[:i], hostport[i+1:]
	}
	return hostport, ""
}

func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// percentDecode undoes %XX escapes, leaving malformed ones literal as git does.
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, ok1 := unhex(s[i+1]); ok1 {
				if lo, ok2 := unhex(s[i+2]); ok2 {
					b.WriteByte(hi<<4 | lo)
					i += 2
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// splitAuthority splits "authority/path" of a hierarchical URL; a missing
// path is an error, as git refuses "ssh://host".
func splitAuthority(rest, raw string) (authority, path string, err error) {
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", "", fmt.Errorf("fetch: no path specified in %q", redactURL(raw))
	}
	return percentDecode(rest[:i]), percentDecode(rest[i:]), nil
}

// tildePath drops the slash before "~user": ssh:// and git:// paths are
// absolute except for home-relative "~" paths (gitprotocol-pack).
func tildePath(p string) string {
	if strings.HasPrefix(p, "/~") {
		return p[1:]
	}
	return p
}

func parseSSHURL(rest string) (*endpoint, error) {
	authority, path, err := splitAuthority(rest, "ssh://"+rest)
	if err != nil {
		return nil, err
	}
	ep := &endpoint{transport: "ssh", path: tildePath(path)}
	hostport := authority
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		ep.user, hostport = authority[:at], authority[at+1:]
	}
	ep.host, ep.port = splitPort(hostport)
	return ep, ep.checkSSH()
}

// parseSCP parses "[user@]host:path", where host may be bracketed to hold
// colons ("[host:port]" or an IPv6 address).
func parseSCP(raw string) (*endpoint, error) {
	ep := &endpoint{transport: "ssh"}
	s := raw
	if at := strings.IndexByte(s, '@'); at >= 0 && at < strings.IndexByte(s, ':') {
		ep.user, s = s[:at], s[at+1:]
	}
	var hostpart string
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 || !strings.HasPrefix(s[end+1:], ":") {
			return nil, fmt.Errorf("fetch: malformed address %q", redactURL(raw))
		}
		hostpart, ep.path = s[1:end], s[end+2:]
		// Inside brackets a single colon separates a port; an IPv6
		// address has several.
		if strings.Count(hostpart, ":") == 1 {
			ep.host, ep.port = splitPort(hostpart)
		} else {
			ep.host = hostpart
		}
	} else {
		colon := strings.IndexByte(s, ':')
		ep.host, ep.path = s[:colon], s[colon+1:]
	}
	if strings.HasPrefix(ep.path, "-") {
		return nil, fmt.Errorf("fetch: strange pathname '%s' blocked", ep.path)
	}
	return ep, ep.checkSSH()
}

// checkSSH refuses a destination ssh would take for an option.
func (ep *endpoint) checkSSH() error {
	dest := ep.host
	if ep.user != "" {
		dest = ep.user + "@" + ep.host
	}
	if strings.HasPrefix(dest, "-") {
		if ep.user != "" {
			dest = "***@" + ep.host // the user part may carry a password
		}
		return fmt.Errorf("fetch: strange hostname '%s' blocked", dest)
	}
	if strings.HasPrefix(ep.port, "-") {
		return fmt.Errorf("fetch: strange port '%s' blocked", ep.port)
	}
	return nil
}

func parseGitURL(rest string) (*endpoint, error) {
	authority, path, err := splitAuthority(rest, "git://"+rest)
	if err != nil {
		return nil, err
	}
	ep := &endpoint{transport: "git", path: tildePath(path), hostParam: authority}
	ep.host, ep.port = splitPort(authority)
	if ep.host == "" {
		return nil, fmt.Errorf("fetch: no host in %q", redactURL("git://"+rest))
	}
	return ep, nil
}

// parseFileURL takes everything from the first slash after "file://" as the
// path: git ignores any host ("file://host/p" is "/p"). On Windows, as with
// Git for Windows, a host makes a UNC path ("//host/p") and a drive may
// follow "file://" directly ("file://C:/p").
func parseFileURL(rest string) (*endpoint, error) {
	rest = percentDecode(rest) // git decodes the whole URL before parsing
	if runtime.GOOS == "windows" {
		if dosDrivePrefix(rest) > 0 {
			return &endpoint{transport: "file", path: rest}, nil
		}
		if rest != "" && rest[0] != '/' && strings.ContainsAny(rest, `/\`) {
			return &endpoint{transport: "file", path: "//" + rest}, nil
		}
	}
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return nil, fmt.Errorf("fetch: no path specified in %q", redactURL("file://"+rest))
	}
	p := rest[i:]
	if runtime.GOOS == "windows" && dosDrivePrefix(p[1:]) > 0 {
		p = p[1:] // file:///C:/repo
	}
	return &endpoint{transport: "file", path: p}, nil
}

func parseHTTPURL(raw, scheme string) (*endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Error would quote the URL, credentials included.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, fmt.Errorf("fetch: invalid URL %q: %w", redactURL(raw), err)
	}
	u.Scheme = scheme
	if u.Host == "" {
		return nil, fmt.Errorf("fetch: no host in %q", redactURL(raw))
	}
	// gitprotocol-http: strip a trailing slash so "$GIT_URL/info/refs"
	// never contains "//".
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = strings.TrimSuffix(u.RawPath, "/")
	u.RawQuery, u.Fragment = "", ""
	return &endpoint{transport: scheme, url: u, host: u.Hostname(), port: u.Port()}, nil
}

// redactURL hides the userinfo of a URL ("scheme://user:pass@host/...") or
// scp-like address ("user@host:path") for error messages: a rewritten URL
// may carry a token from url.<base>.insteadOf.
func redactURL(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		authority := rest
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			authority = rest[:j]
		}
		if at := strings.LastIndexByte(authority, '@'); at >= 0 {
			return raw[:i+3] + "***" + rest[at:]
		}
		return raw
	}
	if before, _, ok := strings.Cut(raw, ":"); ok {
		if at := strings.IndexByte(before, '@'); at >= 0 {
			return "***" + raw[at:]
		}
	}
	return raw
}

// insteadOf is one url.<base>.insteadOf rule.
type insteadOf struct{ base, prefix string }

// rewriteURL applies the url.<base>.insteadOf rule with the longest
// matching prefix; ties go to the rule read first (observed with git 2.56).
func rewriteURL(raw string, rules []insteadOf) string {
	best := -1
	for i, r := range rules {
		if strings.HasPrefix(raw, r.prefix) && (best < 0 || len(r.prefix) > len(rules[best].prefix)) {
			best = i
		}
	}
	if best < 0 {
		return raw
	}
	return rules[best].base + raw[len(rules[best].prefix):]
}
