// Package merge is git's three-way file merge, the in-process equivalent of
// `git merge-file -p`: it combines the changes from base to ours and from base
// to theirs line by line and brackets overlapping changes with conflict
// markers, byte for byte as git writes them.
//
// It is written from git's documentation (git-merge-file, gitattributes,
// config/merge) and from the observed behaviour of the git binary, not from
// another implementation.
package merge

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
)

// Style selects how conflicts are written, as merge.conflictStyle does.
type Style int

const (
	// StyleMerge writes ours and theirs only; conflicting regions are
	// narrowed to the lines that really differ and nearby conflicts are
	// joined. It is git's default.
	StyleMerge Style = iota
	// StyleDiff3 also writes the base lines between ||||||| and =======,
	// without narrowing the conflict.
	StyleDiff3
	// StyleZDiff3 is StyleDiff3 with lines common to the start and end of
	// both sides moved out of the conflict.
	StyleZDiff3
)

// Labels name the sides on the conflict marker lines (-L in git merge-file).
// An empty label leaves the marker bare, as `-L ""` does.
type Labels struct {
	Ours, Base, Theirs string
}

// Options configure File; the zero value is git's default merge.
type Options struct {
	Labels Labels
	Style  Style
}

// ErrBinary reports an input git refuses to merge because it looks binary.
var ErrBinary = errors.New("cannot merge binary files")

// binaryProbe is how much of a file git inspects for a NUL byte when
// deciding that it is binary.
const binaryProbe = 8000

// maxSize is the largest input git merges; it reports bigger ones as binary.
const maxSize = 1023 << 20

// markerSize is the length of git's conflict markers (conflict-marker-size).
const markerSize = 7

// File merges the changes from base to theirs into ours and returns the
// result with the number of conflicts in it. git merge-file's exit status is
// that number capped at 127. Inputs git refuses as binary (a NUL byte in the
// first 8000 bytes, or more than 1023 MiB) yield an error wrapping ErrBinary.
func File(ours, base, theirs []byte, opts Options) (merged []byte, conflicts int, err error) {
	for _, in := range []struct {
		name string
		data []byte
	}{{"ours", ours}, {"base", base}, {"theirs", theirs}} {
		if len(in.data) > maxSize || bytes.IndexByte(in.data[:min(len(in.data), binaryProbe)], 0) >= 0 {
			return nil, 0, fmt.Errorf("merge: %w: %s", ErrBinary, in.name)
		}
	}
	switch opts.Style {
	case StyleMerge, StyleDiff3, StyleZDiff3:
	default:
		return nil, 0, fmt.Errorf("merge: unknown conflict style %d", opts.Style)
	}

	var tab internTable
	m := merger{
		opts: opts,
		o:    tab.lines(ours),
		b:    tab.lines(base),
		t:    tab.lines(theirs),
	}
	m.oid, m.bid, m.tid = tab.ids(m.o), tab.ids(m.b), tab.ids(m.t)
	m.plan()
	return m.write(), m.conflicts, nil
}

// region is a stretch of the three files that the merge handles as one unit:
// ours[O0:O1], base[B0:B1] and theirs[T0:T1].
type region struct {
	O0, O1, B0, B1, T0, T1 int
}

// action says how the result is built for a region.
type action int

const (
	keepOurs   action = iota // a change made on our side only
	takeTheirs               // a change made on their side only
	conflict
)

type item struct {
	region
	act action
}

// merger holds one merge: the split lines of each file, their interned ids
// and the planned sequence of non-trivial regions. Everything outside the
// planned regions is copied from ours.
type merger struct {
	opts          Options
	o, b, t       [][]byte
	oid, bid, tid []int32
	items         []item
	conflicts     int
}

// plan walks both diffs against base in base order. Changes from the two sides
// that overlap or touch form a conflict unless both sides made the same edit.
func (m *merger) plan() {
	ho, ht := diffLines(m.bid, m.oid), diffLines(m.bid, m.tid)
	dO, dT := 0, 0 // side index minus base index outside hunks
	i, j := 0, 0
	for i < len(ho) || j < len(ht) {
		switch {
		case j == len(ht) || (i < len(ho) && ho[i].A+ho[i].NA < ht[j].A):
			h := ho[i]
			m.items = append(m.items, item{act: keepOurs,
				O0: h.B, O1: h.B + h.NB, B0: h.A, B1: h.A + h.NA,
				T0: h.A + dT, T1: h.A + h.NA + dT})
			dO += h.NB - h.NA
			i++
		case i == len(ho) || ht[j].A+ht[j].NA < ho[i].A:
			h := ht[j]
			m.items = append(m.items, item{act: takeTheirs,
				O0: h.A + dO, O1: h.A + h.NA + dO, B0: h.A, B1: h.A + h.NA,
				T0: h.B, T1: h.B + h.NB})
			dT += h.NB - h.NA
			j++
		default:
			lo := min(ho[i].A, ht[j].A)
			hi := max(ho[i].A+ho[i].NA, ht[j].A+ht[j].NA)
			r := region{B0: lo, O0: lo + dO, T0: lo + dT}
			i0, j0 := i, j
			for grew := true; grew; {
				grew = false
				for i < len(ho) && ho[i].A <= hi {
					hi = max(hi, ho[i].A+ho[i].NA)
					dO += ho[i].NB - ho[i].NA
					i++
					grew = true
				}
				for j < len(ht) && ht[j].A <= hi {
					hi = max(hi, ht[j].A+ht[j].NA)
					dT += ht[j].NB - ht[j].NA
					j++
					grew = true
				}
			}
			r.B1, r.O1, r.T1 = hi, hi+dO, hi+dT
			// Only a single identical hunk on each side counts as the same
			// edit; sides that reach equal text through differently shaped
			// edits still conflict (and StyleMerge narrows that to nothing).
			same := i-i0 == 1 && j-j0 == 1 &&
				ho[i0].A == ht[j0].A && ho[i0].NA == ht[j0].NA &&
				slices.Equal(m.oid[r.O0:r.O1], m.tid[r.T0:r.T1])
			if !same {
				m.addConflict(r)
			}
		}
	}
	if m.opts.Style == StyleMerge {
		m.joinConflicts()
	}
	for _, it := range m.items {
		if it.act == conflict {
			m.conflicts++
		}
	}
}

// addConflict records a conflict, narrowed as the style asks.
func (m *merger) addConflict(r region) {
	switch m.opts.Style {
	case StyleDiff3:
		m.items = append(m.items, item{region: r, act: conflict})
	case StyleZDiff3:
		for r.O0 < r.O1 && r.T0 < r.T1 && m.oid[r.O0] == m.tid[r.T0] {
			r.O0++
			r.T0++
		}
		for r.O0 < r.O1 && r.T0 < r.T1 && m.oid[r.O1-1] == m.tid[r.T1-1] {
			r.O1--
			r.T1--
		}
		m.items = append(m.items, item{region: r, act: conflict})
	default:
		// Only the lines where the two sides differ from each other conflict;
		// the base no longer matters once the conflict is narrowed.
		hs := diffLines(m.oid[r.O0:r.O1], m.tid[r.T0:r.T1])
		if len(hs) == 0 {
			// Both sides ended up equal although they edited base
			// differently. git resolves this like a one-sided change, which
			// keeps the conflicts around it apart.
			m.items = append(m.items, item{region: r, act: keepOurs})
		}
		// Narrowed conflicts carry no base range: StyleMerge never writes it.
		for _, h := range hs {
			m.items = append(m.items, item{act: conflict,
				O0: r.O0 + h.A, O1: r.O0 + h.A + h.NA,
				T0: r.T0 + h.B, T1: r.T0 + h.B + h.NB})
		}
	}
}

// joinGap is the number of common lines up to which git still joins two
// neighbouring conflicts into one.
const joinGap = 3

// joinConflicts joins neighbouring conflicts separated only by a few lines,
// or by lines without any letter or digit, which are rarely worth resolving
// separately.
func (m *merger) joinConflicts() {
	out := m.items[:0]
	for _, it := range m.items {
		if n := len(out); n > 0 && it.act == conflict && out[n-1].act == conflict {
			prev := &out[n-1]
			if gap := m.o[prev.O1:it.O0]; len(gap) <= joinGap || !hasAlnum(gap) {
				prev.O1, prev.T1, prev.B1 = it.O1, it.T1, it.B1
				continue
			}
		}
		out = append(out, it)
	}
	m.items = out
}

func hasAlnum(lines [][]byte) bool {
	for _, l := range lines {
		for _, c := range l {
			if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
				return true
			}
		}
	}
	return false
}

// write renders the planned merge.
func (m *merger) write() []byte {
	var out []byte
	o := 0
	for _, it := range m.items {
		for _, l := range m.o[o:it.O0] {
			out = append(out, l...)
		}
		o = it.O1
		switch it.act {
		case keepOurs:
			for _, l := range m.o[it.O0:it.O1] {
				out = append(out, l...)
			}
		case takeTheirs:
			for _, l := range m.t[it.T0:it.T1] {
				out = append(out, l...)
			}
		case conflict:
			out = m.writeConflict(out, it.region)
		}
	}
	for _, l := range m.o[o:] {
		out = append(out, l...)
	}
	return out
}

func (m *merger) writeConflict(out []byte, r region) []byte {
	eol := []byte("\n")
	if m.markersNeedCR(r) {
		eol = []byte("\r\n")
	}
	marker := func(c byte, label string) {
		for range markerSize {
			out = append(out, c)
		}
		if label != "" {
			out = append(out, ' ')
			out = append(out, label...)
		}
		out = append(out, eol...)
	}
	section := func(lines [][]byte) {
		for _, l := range lines {
			out = append(out, l...)
		}
		// A side ending without a newline still needs the marker on a line
		// of its own.
		if n := len(lines); n > 0 && !bytes.HasSuffix(lines[n-1], []byte("\n")) {
			out = append(out, eol...)
		}
	}
	marker('<', m.opts.Labels.Ours)
	section(m.o[r.O0:r.O1])
	if m.opts.Style != StyleMerge {
		marker('|', m.opts.Labels.Base)
		section(m.b[r.B0:r.B1])
	}
	marker('=', "")
	section(m.t[r.T0:r.T1])
	marker('>', m.opts.Labels.Theirs)
	return out
}

// markersNeedCR reports whether a conflict's marker lines (and newlines added
// after a final line without one) end in CRLF. git decides from the line
// before the conflict in ours and in theirs (their first line when the
// conflict starts the file) and from the first line of base: CRLF only if
// base's line ends in CRLF and neither side's line ends in a bare LF. Lines
// without a newline, and empty files, do not count against CRLF on the sides.
func (m *merger) markersNeedCR(r region) bool {
	v := lineEnding(m.o, max(r.O0-1, 0))
	if v != 0 {
		v = lineEnding(m.t, max(r.T0-1, 0))
	}
	if v != 0 {
		v = lineEnding(m.b, 0)
	}
	return v == 1
}

// lineEnding is 1 if lines[i] ends in CRLF, 0 if in a bare LF, and -1 if it
// has no newline or does not exist.
func lineEnding(lines [][]byte, i int) int {
	if i >= len(lines) {
		return -1
	}
	l := lines[i]
	switch {
	case !bytes.HasSuffix(l, []byte("\n")):
		return -1
	case bytes.HasSuffix(l, []byte("\r\n")):
		return 1
	}
	return 0
}

// internTable numbers distinct lines so the diff compares integers. A line
// includes its newline, so "x" at the end of a file differs from "x\n".
type internTable struct {
	table map[string]int32
}

// lines splits data after each newline; a final line may lack one.
func (in *internTable) lines(data []byte) [][]byte {
	var ls [][]byte
	for l := range bytes.Lines(data) {
		ls = append(ls, l)
	}
	return ls
}

// ids returns the interned id of each line.
func (in *internTable) ids(lines [][]byte) []int32 {
	if in.table == nil {
		in.table = make(map[string]int32)
	}
	out := make([]int32, len(lines))
	for i, l := range lines {
		id, ok := in.table[string(l)]
		if !ok {
			id = int32(len(in.table))
			in.table[string(l)] = id
		}
		out[i] = id
	}
	return out
}
