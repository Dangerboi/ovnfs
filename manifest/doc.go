package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Setting is one value of a manifest key and the file that sets it, as
// `ovnfs config` prints it. A list key has one Setting per item, rules one per
// non-blank line.
type Setting struct {
	File  string `json:"file"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Doc is one manifest file as `ovnfs config` reads and edits it, by dotted key:
// rules, cache.ROOT.rebuild, cache.ROOT.key, env.image, env.setup and the
// daemon settings (see [UserOnly]). Edits change the parsed YAML in place, so
// comments survive [Doc.Save]; blank lines and indentation are normalised.
type Doc struct {
	Path string     // the file on disk
	Name string     // how errors and Setting.File name it: .o2/ovnfs.yml or ~/.o2/ovnfs.yml
	user bool       // the user's file, which alone may set daemon settings
	doc  *yaml.Node // the document as read and edited
	root *yaml.Node // doc's top-level mapping; nil for a missing or empty file
	err  error      // why the file as read does not load
}

// UserDoc is the unread ~/.o2/ovnfs.yml of userHome.
func UserDoc(userHome string) *Doc {
	return &Doc{Path: filepath.Join(userHome, filepath.FromSlash(File)), Name: userFile, user: true}
}

// FolderDoc is the unread .o2/ovnfs.yml at a folder's source root; when root
// is userHome it is the user's file, so it may set daemon settings.
func FolderDoc(root, userHome string) *Doc {
	return &Doc{Path: filepath.Join(root, filepath.FromSlash(File)), Name: File, user: userHome != "" && sameDir(root, userHome)}
}

// sameDir reports whether a and b name one directory, also through a symlink
// such as macOS's /var, which a source root has resolved and $HOME may not.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// Docs are the unread files a checkout of the folder with source root reads,
// highest precedence first: the folder's, then the user's unless they are one file.
func Docs(root, userHome string) []*Doc {
	folder := FolderDoc(root, userHome)
	if folder.user {
		return []*Doc{folder}
	}
	return []*Doc{folder, UserDoc(userHome)}
}

// Read loads the file; a missing one reads as empty. It fails when the file
// cannot be read or holds no YAML mapping. A file that otherwise does not load
// fails only [Doc.Check], [Doc.List] and [Resolve], so Set and Unset can repair it.
func (d *Doc) Read() error {
	data, err := os.ReadFile(d.Path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", d.Name, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return yamlError(d.Name, err)
	}
	_, d.err = parseLayer(data, d.Name, d.user)
	d.doc, d.root = &doc, nil
	if len(doc.Content) > 0 {
		switch top := doc.Content[0]; {
		case top.Kind == yaml.MappingNode:
			d.root = top
		case !null(top):
			return d.err
		}
	}
	return nil
}

// Check reports why the file as last read or saved does not load, as [Load] would.
func (d *Doc) Check() error { return d.err }

// List returns the file's settings in file order; it fails when the file does not load.
func (d *Doc) List() ([]Setting, error) {
	if d.err != nil {
		return nil, d.err
	}
	var out []Setting
	for k, v := range pairs(d.root) {
		switch k {
		case "cache":
			for name, entry := range pairs(v) {
				root, _ := cacheRoot(name)
				for field, fv := range pairs(entry) {
					out = append(out, d.settings("cache."+root+"."+field, fv)...)
				}
			}
		case "env":
			for field, fv := range pairs(v) {
				out = append(out, d.settings("env."+field, fv)...)
			}
		default:
			out = append(out, d.settings(k, v)...)
		}
	}
	return out, nil
}

// Resolve returns key's values as a checkout opened on the folder takes them
// from docs, highest precedence first (see [Load]): rules from every file in
// turn; a cache.ROOT field from the first file with an entry for ROOT, since
// an entry replaces lower ones whole; any other key from the first file that
// sets it, an empty string counting as unset. It fails on an unknown key or a
// file that does not load.
func Resolve(key string, docs ...*Doc) ([]Setting, error) {
	k, err := parseKey(key)
	if err != nil {
		return nil, err
	}
	var out []Setting
	for _, d := range docs {
		if d.err != nil {
			return nil, d.err
		}
		n := d.node(k.path)
		switch {
		case k.kind == lines:
			out = append(out, d.settings(k.name, n)...)
		case k.path[0] == "cache":
			if d.node(k.path[:2]) != nil {
				return d.settings(k.name, n), nil
			}
		case n != nil && !null(n) && (k.kind != scalar || n.Value != ""):
			return d.settings(k.name, n), nil
		}
	}
	return out, nil
}

// Set replaces key's value. rules, env.setup and cache.ROOT.key take any
// number of values (rule lines, list items); any other key exactly one.
func (d *Doc) Set(key string, values ...string) error {
	k, err := parseKey(key)
	if err != nil {
		return err
	}
	if k.kind == scalar && len(values) != 1 {
		return fmt.Errorf("%s takes one value, not %d", k.name, len(values))
	}
	k.write(d.ensure(k.path), values)
	return nil
}

// Append adds values after the current ones of rules, env.setup or cache.ROOT.key.
func (d *Doc) Append(key string, values ...string) error {
	k, err := parseKey(key)
	if err != nil {
		return err
	}
	if k.kind == scalar {
		return fmt.Errorf("%s takes one value; only rules, env.setup and cache.ROOT.key take more", k.name)
	}
	switch n := d.ensure(k.path); {
	case k.kind == list && n.Kind == yaml.SequenceNode:
		n.Content = append(n.Content, scalars(values)...)
	case k.kind == lines && n.Kind == yaml.ScalarNode && n.Value != "":
		n.Value = strings.TrimSuffix(n.Value, "\n") + "\n" + strings.Join(values, "\n") + "\n"
		n.Style = yaml.LiteralStyle
	default:
		k.write(n, values)
	}
	return nil
}

// Unset removes key, then each section it leaves empty (cache.ROOT, cache,
// env); found is false when the file does not have the key.
func (d *Doc) Unset(key string) (found bool, err error) {
	k, err := parseKey(key)
	if err != nil {
		return false, err
	}
	maps, at := d.walk(k.path)
	if len(at) < len(k.path) {
		return false, nil
	}
	for i, m := range slices.Backward(maps) {
		m.Content = slices.Delete(m.Content, at[i]-1, at[i]+1)
		if len(m.Content) > 0 {
			break
		}
	}
	return true, nil
}

// Save writes the file back, creating its .o2 directory, a file left without
// settings as an empty file. It writes nothing and fails when the result
// would not load.
func (d *Doc) Save() error {
	var data []byte
	if d.root != nil && len(d.root.Content) > 0 {
		var b bytes.Buffer
		enc := yaml.NewEncoder(&b)
		enc.SetIndent(2)
		if err := enc.Encode(d.doc); err != nil {
			return fmt.Errorf("encode %s: %w", d.Name, err)
		}
		if err := enc.Close(); err != nil {
			return fmt.Errorf("encode %s: %w", d.Name, err)
		}
		data = b.Bytes()
	}
	if _, err := parseLayer(data, d.Name, d.user); err != nil {
		return err
	}
	if err := writeFile(d.Path, data); err != nil {
		return fmt.Errorf("write %s: %w", d.Name, err)
	}
	d.err = nil
	return nil
}

// settings returns n's values as Settings of key: one per sequence item, one
// per non-blank line of rules, none for a missing or null value.
func (d *Doc) settings(key string, n *yaml.Node) []Setting {
	var out []Setting
	add := func(v string) { out = append(out, Setting{File: d.Name, Key: key, Value: v}) }
	switch {
	case n == nil || null(n):
	case n.Kind == yaml.SequenceNode:
		for _, item := range n.Content {
			add(item.Value)
		}
	case key == "rules":
		for line := range strings.Lines(n.Value) {
			if line = strings.TrimRight(line, " \t\r\n"); line != "" {
				add(line)
			}
		}
	default:
		add(n.Value)
	}
	return out
}

// node returns the value at path, or nil when a key on the way is missing.
func (d *Doc) node(path []string) *yaml.Node {
	maps, at := d.walk(path)
	if len(at) < len(path) {
		return nil
	}
	return maps[len(maps)-1].Content[at[len(at)-1]]
}

// walk follows path from the top of the file, returning the mapping holding
// each key and the index of the key's value in it; it stops at the first
// missing key.
func (d *Doc) walk(path []string) (maps []*yaml.Node, at []int) {
	n := d.root
	for i := range path {
		j := find(n, path, i)
		if j < 0 {
			break
		}
		maps, at = append(maps, n), append(at, j)
		n = n.Content[j]
	}
	return maps, at
}

// ensure returns the value node at path, adding missing keys and turning
// anything on the way into a mapping.
func (d *Doc) ensure(path []string) *yaml.Node {
	if d.root == nil {
		d.root = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		d.doc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{d.root}}
	}
	n := d.root
	for i := range path {
		if n.Kind != yaml.MappingNode {
			n.Kind, n.Tag, n.Value, n.Style, n.Content = yaml.MappingNode, "!!map", "", 0, nil
		}
		j := find(n, path, i)
		if j < 0 {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: path[i]}, &yaml.Node{})
			j = len(n.Content) - 1
		}
		n = n.Content[j]
	}
	return n
}

// find returns the index of path[i]'s value in mapping m, or -1. Cache roots
// compare cleaned, so cache.web/dist finds an entry written ./web/dist.
func find(m *yaml.Node, path []string, i int) int {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1
	}
	for j := 0; j+1 < len(m.Content); j += 2 {
		k := m.Content[j].Value
		if i == 1 && path[0] == "cache" {
			k, _ = cacheRoot(k)
		}
		if k == path[i] {
			return j + 1
		}
	}
	return -1
}

// pairs yields the keys and values of mapping m; nothing for any other node.
func pairs(m *yaml.Node) iter.Seq2[string, *yaml.Node] {
	return func(yield func(string, *yaml.Node) bool) {
		if m == nil || m.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			if !yield(m.Content[i].Value, m.Content[i+1]) {
				return
			}
		}
	}
}

// null reports a YAML null: `key:`, `key: ~` or `key: null`.
func null(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.Tag == "!!null" }

// scalars converts values into string nodes, quoted where YAML would read them otherwise.
func scalars(values []string) []*yaml.Node {
	out := make([]*yaml.Node, len(values))
	for i, v := range values {
		out[i] = new(yaml.Node)
		out[i].SetString(v)
	}
	return out
}

// plain reports whether v written bare reads back as a scalar of YAML type
// tag, so `fsync: true` stays a boolean rather than becoming the string "true".
func plain(v, tag string) bool {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(v), &doc) != nil || len(doc.Content) != 1 {
		return false
	}
	n := doc.Content[0]
	return n.Kind == yaml.ScalarNode && n.Tag == tag && n.Value == v
}

// kind is the shape of a key's value.
type kind uint8

const (
	scalar kind = iota // one string, or a daemon setting's bool or int
	list               // a sequence of strings: env.setup, cache.ROOT.key
	lines              // rules: a literal block of rule lines
)

// key is a dotted manifest key resolved against the schema.
type key struct {
	name string   // cleaned dotted form, as Setting.Key spells it
	path []string // mapping keys from the top of the file
	kind kind
	tag  string // YAML type a daemon setting may be written as besides a string
}

// parseKey resolves a dotted key. A cache root may hold dots itself
// (cache.web/.next.rebuild): the field is what follows the last one.
func parseKey(name string) (key, error) {
	switch name {
	case "rules":
		return key{name: name, path: []string{"rules"}, kind: lines}, nil
	case "env.image":
		return key{name: name, path: []string{"env", "image"}}, nil
	case "env.setup":
		return key{name: name, path: []string{"env", "setup"}, kind: list}, nil
	}
	if tag, ok := daemonKeys[name]; ok {
		return key{name: name, path: []string{name}, tag: tag}, nil
	}
	if rest, ok := strings.CutPrefix(name, "cache."); ok {
		if i := strings.LastIndexByte(rest, '.'); i > 0 {
			root, ok := cacheRoot(rest[:i])
			k := key{name: "cache." + root + "." + rest[i+1:], path: []string{"cache", root, rest[i+1:]}}
			switch {
			case ok && k.path[2] == "rebuild":
				return k, nil
			case ok && k.path[2] == "key":
				k.kind = list
				return k, nil
			}
		}
	}
	return key{}, fmt.Errorf("unknown key %q", name)
}

// write makes n hold values in k's shape, keeping n's comments and a
// sequence's flow or block style.
func (k key) write(n *yaml.Node, values []string) {
	switch k.kind {
	case scalar:
		n.Style, n.Content = 0, nil
		if v := values[0]; k.tag != "" && plain(v, k.tag) {
			n.Kind, n.Tag, n.Value = yaml.ScalarNode, k.tag, v
		} else {
			n.SetString(v)
		}
	case list:
		if n.Kind != yaml.SequenceNode {
			n.Style = 0
		}
		n.Kind, n.Tag, n.Value, n.Content = yaml.SequenceNode, "!!seq", "", scalars(values)
	case lines:
		n.Kind, n.Tag, n.Value, n.Style, n.Content = yaml.ScalarNode, "!!str", "", yaml.LiteralStyle, nil
		if len(values) > 0 {
			n.Value = strings.Join(values, "\n") + "\n"
		}
	}
}

// writeFile replaces name with data by renaming a temporary file over it, so
// a reader never sees half a file. A symlink is written through to its target,
// keeping a dotfiles link, and an existing file keeps its mode.
func writeFile(name string, data []byte) error {
	if target, err := filepath.EvalSymlinks(name); err == nil {
		name = target
	}
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(name); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".ovnfs-*.yml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}
