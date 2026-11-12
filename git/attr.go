package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/index"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/repo"
	"github.com/stencil-hq/ovnfs/git/internal/rev"
)

// Attribute states Check reports besides a value, spelled as git check-attr does.
const (
	AttrUnspecified = "unspecified"
	AttrSet         = "set"
	AttrUnset       = "unset"
)

// ErrAttrUnsupported reports an attribute query that cannot be answered the
// way git would answer it: the builtin_objectmode attribute, which needs the
// work tree's file modes.
var ErrAttrUnsupported = errors.New("unsupported attribute query")

// git skips attribute lines and files reaching these sizes. A line's length
// excludes its LF and one CR before it.
const (
	attrMaxLine = 2048
	attrMaxFile = 100 << 20
)

// attrBlank separates patterns and attributes; unlike strings.Fields, git does
// not treat \v or \f as blanks here.
const attrBlank = " \t\r\n"

// attrKind is one of the four states an attribute can be in for a path.
type attrKind uint8

const (
	attrUnspecified attrKind = iota
	attrSet
	attrUnset
	attrValue
)

// attrState assigns a state to one attribute.
type attrState struct {
	name  string
	kind  attrKind
	value string
}

// String is the state as git check-attr reports it.
func (s attrState) String() string {
	switch s.kind {
	case attrSet:
		return AttrSet
	case attrUnset:
		return AttrUnset
	case attrValue:
		return s.value
	}
	return AttrUnspecified
}

// attrRule is one pattern line: the states it assigns to matching paths.
type attrRule struct {
	pattern ignoreRule
	states  []attrState
}

// attrMacro is one [attr] definition.
type attrMacro struct {
	name   string
	states []attrState
}

// attrSyntax holds what changes how a gitattributes file is read.
type attrSyntax struct {
	// fold matches patterns case-insensitively (core.ignorecase).
	fold bool
	// topLevel allows [attr] macro definitions, which git drops from
	// .gitattributes files below the top of the work tree.
	topLevel bool
}

// parse reads a gitattributes file. Lines git rejects (negative patterns,
// invalid attribute names, overly long lines) are skipped, as git skips them
// after a warning.
func (s attrSyntax) parse(data []byte) ([]attrRule, []attrMacro) {
	var rules []attrRule
	var macros []attrMacro
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	for raw := range bytes.Lines(data) {
		raw = bytes.TrimSuffix(raw, []byte("\n"))
		raw = bytes.TrimSuffix(raw, []byte("\r"))
		if len(raw) >= attrMaxLine {
			continue
		}
		line := strings.TrimLeft(string(raw), attrBlank)
		if line == "" || line[0] == '#' {
			continue
		}
		if def, ok := strings.CutPrefix(line, "[attr]"); ok {
			if !s.topLevel {
				continue
			}
			name, rest := cutBlank(def)
			states, ok := parseAttrStates(rest)
			if !ok || !validAttrName(name) {
				continue
			}
			macros = append(macros, attrMacro{name: name, states: states})
			continue
		}
		pattern, rest := attrPattern(line)
		// The check follows unquoting: "\041x" is as negative as !x.
		if strings.HasPrefix(pattern, "!") {
			continue
		}
		states, ok := parseAttrStates(rest)
		if !ok || len(states) == 0 {
			continue
		}
		if s.fold {
			pattern = asciiLower(pattern)
		}
		rules = append(rules, attrRule{pattern: newPatternRule(pattern, nil), states: states})
	}
	return rules, macros
}

// cutBlank splits s at its first blank.
func cutBlank(s string) (token, rest string) {
	if end := strings.IndexAny(s, attrBlank); end >= 0 {
		return s[:end], s[end:]
	}
	return s, ""
}

// attrPattern splits a line into its pattern and attribute list. A badly
// quoted pattern is taken literally, quotes included; after a well-quoted one
// the attribute list starts right after the closing quote.
func attrPattern(line string) (pattern, rest string) {
	if line[0] == '"' {
		if unquoted, n, ok := unquoteC(line); ok {
			return unquoted, line[n:]
		}
	}
	return cutBlank(line)
}

// unquoteC decodes the C-style quoted string at the start of s, returning it
// and the length it occupied.
func unquoteC(s string) (string, int, bool) {
	var out strings.Builder
	for i := 1; i < len(s); i++ {
		switch ch := s[i]; ch {
		case '"':
			return out.String(), i + 1, true
		case '\\':
			i++
			if i == len(s) {
				return "", 0, false
			}
			if decoded := strings.IndexByte(`abfnrtv\"`, s[i]); decoded >= 0 {
				out.WriteByte("\a\b\f\n\r\t\v\\\""[decoded])
				continue
			}
			// Octal escapes take exactly three digits, the first at most 3.
			if i+2 < len(s) && '0' <= s[i] && s[i] <= '3' && isOctal(s[i+1]) && isOctal(s[i+2]) {
				out.WriteByte((s[i]-'0')<<6 | (s[i+1]-'0')<<3 | (s[i+2] - '0'))
				i += 2
				continue
			}
			return "", 0, false
		default:
			out.WriteByte(ch)
		}
	}
	return "", 0, false
}

func isOctal(ch byte) bool { return '0' <= ch && ch <= '7' }

// parseAttrStates reads an attribute list; one invalid name rejects the line.
func parseAttrStates(list string) ([]attrState, bool) {
	var states []attrState
	for token := range strings.FieldsFuncSeq(list, func(r rune) bool { return strings.ContainsRune(attrBlank, r) }) {
		var state attrState
		switch token[0] {
		case '-':
			state.kind = attrUnset
			state.name, _, _ = strings.Cut(token[1:], "=")
		case '!':
			state.kind = attrUnspecified
			state.name, _, _ = strings.Cut(token[1:], "=")
		default:
			name, value, valued := strings.Cut(token, "=")
			state.name, state.kind, state.value = name, attrSet, value
			if valued {
				state.kind = attrValue
			}
		}
		if !validAttrName(state.name) || strings.HasPrefix(state.name, "builtin_") {
			return nil, false
		}
		states = append(states, state)
	}
	return states, true
}

// validAttrName reports whether name is made of ASCII letters, digits, '-',
// '.' and '_', not starting with '-'.
func validAttrName(name string) bool {
	if name == "" || name[0] == '-' {
		return false
	}
	for _, ch := range []byte(name) {
		if !('a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || '0' <= ch && ch <= '9' || ch == '-' || ch == '.' || ch == '_') {
			return false
		}
	}
	return true
}

// asciiLower folds ASCII letters only, as core.ignorecase does for attributes.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// envBool reads a boolean environment variable the way git does: unset is
// false, and integers count as true when not zero.
func envBool(name string) (bool, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return false, nil
	}
	switch strings.ToLower(value) {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off", "":
		return false, nil
	}
	number := strings.TrimLeft(value, " \t\n\v\f\r")
	if last := len(number) - 1; last > 0 && strings.ContainsRune("kKmMgG", rune(number[last])) {
		number = number[:last]
	}
	if n, err := strconv.ParseInt(number, 0, 64); err == nil && !strings.Contains(number, "_") {
		return n != 0, nil
	}
	return false, fmt.Errorf("bad boolean environment value %q for %q", value, name)
}

// attrDir is one work-tree directory's .gitattributes, parsed on first need.
type attrDir struct {
	once  sync.Once
	rules []attrRule
	err   error
}

// Attributes answers `git check-attr` queries for a fixed list of attribute
// names. It is safe for concurrent use; each directory's .gitattributes is read
// from the work tree when a query first needs it and kept for later queries.
type Attributes struct {
	repo   *Repo
	names  []string
	wanted map[string]bool
	syntax attrSyntax
	macros map[string][]attrState
	// info is $GIT_DIR/info/attributes, the highest precedence; outer holds
	// the global and system files, below every .gitattributes.
	info  []attrRule
	outer [][]attrRule
	mu    sync.Mutex
	dirs  map[string]*attrDir
	// fromTree holds the .gitattributes files of the tree GIT_ATTR_SOURCE or
	// attr.tree names, by slash path; nil reads the work tree.
	fromTree map[string][]byte
	// indexed lists the index's stage-0 .gitattributes blobs, the work tree's
	// fallback: a captured snapshot's, else the live index loaded once on first need.
	indexOnce sync.Once
	indexed   map[string]object.ID
	indexErr  error
}

// AttributeOptions tunes Repo.Attributes.
type AttributeOptions struct {
	// Index is the index snapshot whose .gitattributes blobs back unusable
	// work-tree files; nil reads the repository's live index on first need.
	Index *Index
}

// Attributes prepares queries for names with r's attribute files and
// core.ignorecase. The system file is /etc/gitattributes (git's own path depends
// on its install prefix), skipped when GIT_ATTR_NOSYSTEM is true.
func (r *Repo) Attributes(names []string, opts AttributeOptions) (*Attributes, error) {
	if len(names) == 0 {
		return nil, errors.New("no attribute specified")
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		if !validAttrName(name) {
			return nil, fmt.Errorf("%s: not a valid attribute name", name)
		}
		if name == "builtin_objectmode" {
			return nil, fmt.Errorf("%w: %s", ErrAttrUnsupported, name)
		}
		wanted[name] = true
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return nil, err
	}
	fold, _, err := cfg.Bool("core.ignorecase")
	if err != nil {
		return nil, err
	}
	noSystem, err := envBool("GIT_ATTR_NOSYSTEM")
	if err != nil {
		return nil, err
	}
	global, _, err := cfg.Path("core.attributesFile")
	if err != nil {
		return nil, err
	}
	if global == "" {
		global = defaultAttributesFile()
	} else if !filepath.IsAbs(global) {
		global = filepath.Join(r.Top, global)
	}

	a := &Attributes{
		repo:   r,
		names:  names,
		wanted: wanted,
		syntax: attrSyntax{fold: fold, topLevel: true},
		macros: map[string][]attrState{"binary": {
			{name: "diff", kind: attrUnset}, {name: "merge", kind: attrUnset}, {name: "text", kind: attrUnset},
		}},
		dirs: make(map[string]*attrDir),
	}
	if opts.Index != nil {
		a.indexed = opts.Index.attrFiles
		a.indexOnce.Do(func() {})
	}
	if err := a.useTreeSource(cfg); err != nil {
		return nil, err
	}
	// Macros are global, a later definition replacing an earlier one, in this
	// order: system, global, top-level .gitattributes, info/attributes.
	read := func(data []byte) []attrRule {
		rules, macros := a.syntax.parse(data)
		for _, m := range macros {
			a.macros[m.name] = m.states
		}
		return rules
	}
	var system []attrRule
	if !noSystem {
		system = read(readHostAttributes("/etc/gitattributes"))
	}
	if global != "" {
		a.outer = append(a.outer, read(readHostAttributes(global)))
	}
	a.outer = append(a.outer, system)
	top := &attrDir{}
	data, err := a.treeAttributes("")
	if err != nil {
		return nil, err
	}
	top.once.Do(func() { top.rules = read(data) })
	a.dirs[""] = top
	a.info = read(readHostAttributes(filepath.Join(r.CommonDir, "info", "attributes")))
	return a, nil
}

// defaultAttributesFile is core.attributesFile's default, "" without a home.
func defaultAttributesFile() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "git", "attributes")
}

// readHostAttributes reads an attribute file outside the work tree. Git warns
// about and then ignores one it cannot read, so errors leave it empty.
func readHostAttributes(name string) []byte {
	info, err := os.Stat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() >= attrMaxFile {
		return nil
	}
	data, err := os.ReadFile(name)
	if err != nil || len(data) >= attrMaxFile {
		return nil
	}
	return data
}

// useTreeSource makes a read .gitattributes files from a tree instead of the
// work tree when GIT_ATTR_SOURCE or attr.tree names one, as git does: a
// GIT_ATTR_SOURCE that names no tree is an error, an attr.tree that names none
// is ignored (git 2.56 reads the work tree then, whatever config/attr.adoc
// says). The tree's files are read once, here.
func (a *Attributes) useTreeSource(cfg *config.Config) error {
	spec, fromEnv := os.LookupEnv("GIT_ATTR_SOURCE")
	if !fromEnv {
		var ok bool
		if spec, ok = cfg.Get("attr.tree"); !ok {
			return nil
		}
	}
	return a.repo.sessionObjects(func(s *rev.Source, objects *repo.Objects) error {
		root, err := rev.Resolve(s, spec+"^{tree}", rev.Options{})
		if err != nil {
			if fromEnv {
				return fmt.Errorf("bad GIT_ATTR_SOURCE %q: %w", spec, err)
			}
			return nil
		}
		a.fromTree = map[string][]byte{}
		for e, err := range objects.Walk(root, object.WalkOptions{}) {
			if err != nil {
				return err
			}
			if path.Base(e.Name) != ".gitattributes" || e.Mode.Kind() != object.Blob || e.Mode == object.ModeSymlink {
				continue
			}
			_, data, err := objects.Read(e.ID)
			if err != nil {
				return err
			}
			a.fromTree[e.Name] = data
		}
		return nil
	})
}

// treeAttributes reads dir's .gitattributes. From the work tree, a symbolic link
// is not followed and a directory in its place counts as an empty file; any
// other file git cannot open falls back to the index.
func (a *Attributes) treeAttributes(dir string) ([]byte, error) {
	name := path.Join(dir, ".gitattributes")
	if a.fromTree != nil {
		return a.fromTree[name], nil
	}
	full := filepath.Join(a.repo.Top, filepath.FromSlash(name))
	info, err := os.Lstat(full)
	if err == nil && info.IsDir() {
		return nil, nil
	}
	if err == nil && info.Mode().IsRegular() && info.Size() < attrMaxFile {
		if data, err := os.ReadFile(full); err == nil && len(data) < attrMaxFile {
			return data, nil
		}
	}
	return a.indexAttributes(name)
}

// indexAttributes reads the stage-0 regular file name from the index, nil when
// the index has none. The index's .gitattributes entries are listed on first
// need; a blob is read only for a directory whose work-tree file is unusable.
func (a *Attributes) indexAttributes(name string) ([]byte, error) {
	a.indexOnce.Do(func() {
		idx, err := index.Read(a.repo.IndexPath(), a.repo.repo.Format, index.ReadOptions{})
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			a.indexErr = err
			return
		}
		a.indexed = map[string]object.ID{}
		for _, e := range idx.Entries {
			if isIndexedAttrFile(e) {
				a.indexed[e.Path] = e.ID
			}
		}
	})
	id, ok := a.indexed[name]
	if a.indexErr != nil || !ok {
		return nil, a.indexErr
	}
	objects, err := a.repo.repo.OpenObjects()
	if err != nil {
		return nil, err
	}
	defer objects.Close()
	_, data, err := objects.Read(id)
	if errors.Is(err, os.ErrNotExist) || len(data) >= attrMaxFile {
		return nil, nil
	}
	return data, err
}

// dirRules returns the rules of dir's .gitattributes, reading it once.
func (a *Attributes) dirRules(dir string) ([]attrRule, error) {
	a.mu.Lock()
	entry := a.dirs[dir]
	if entry == nil {
		entry = &attrDir{}
		a.dirs[dir] = entry
	}
	a.mu.Unlock()
	entry.once.Do(func() {
		var data []byte
		if data, entry.err = a.treeAttributes(dir); entry.err == nil {
			syntax := a.syntax
			syntax.topLevel = false
			entry.rules, _ = syntax.parse(data)
		}
	})
	return entry.rules, entry.err
}

// Check returns the state of each of a's names for p, a clean work-tree-relative
// slash path, ending in a slash for a directory: AttrUnspecified, AttrSet,
// AttrUnset or the attribute's value, as `git check-attr` reports them.
func (a *Attributes) Check(p string) ([]string, error) {
	dir := strings.HasSuffix(p, "/")
	clean := strings.TrimSuffix(p, "/")
	parts := strings.Split(clean, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("attribute path %q is not a clean relative path", p)
		}
	}
	match := parts
	if a.syntax.fold {
		if folded := asciiLower(clean); folded != clean {
			match = strings.Split(folded, "/")
		}
	}
	fill := attrFill{decided: make(map[string]attrState), macros: a.macros, wanted: a.wanted, remaining: len(a.wanted)}
	// Sources in falling precedence: info/attributes, then each .gitattributes
	// from the path's own directory up to the top, then global and system.
	fill.apply(a.info, match, dir)
	end := len(clean)
	for depth := len(parts) - 1; depth >= 0 && fill.remaining > 0; depth-- {
		end -= len(parts[depth])
		if end > 0 {
			end--
		}
		rules, err := a.dirRules(clean[:end])
		if err != nil {
			return nil, err
		}
		fill.apply(rules, match[depth:], dir)
	}
	for _, rules := range a.outer {
		fill.apply(rules, match, dir)
	}
	values := make([]string, len(a.names))
	for n, name := range a.names {
		values[n] = fill.decided[name].String()
	}
	return values, nil
}

// attrFill decides attributes from the highest precedence down: the first
// decision for an attribute, including one that returns it to unspecified,
// stands.
type attrFill struct {
	decided   map[string]attrState
	macros    map[string][]attrState
	wanted    map[string]bool
	remaining int
}

// apply visits a file's rules matching parts, last line first and each line's
// last attribute first, since later ones override earlier ones.
func (f *attrFill) apply(rules []attrRule, parts []string, dir bool) {
	for n := len(rules) - 1; n >= 0 && f.remaining > 0; n-- {
		if !rules[n].pattern.matches(parts, dir) {
			continue
		}
		for s := len(rules[n].states) - 1; s >= 0; s-- {
			f.decide(rules[n].states[s])
		}
	}
}

// decide records state unless its attribute is decided already; setting a
// macro (not unsetting it or giving it a value) also decides its members.
func (f *attrFill) decide(state attrState) {
	if _, ok := f.decided[state.name]; ok {
		return
	}
	f.decided[state.name] = state
	if f.wanted[state.name] {
		f.remaining--
	}
	if state.kind != attrSet {
		return
	}
	members := f.macros[state.name]
	for n := len(members) - 1; n >= 0; n-- {
		f.decide(members[n])
	}
}
