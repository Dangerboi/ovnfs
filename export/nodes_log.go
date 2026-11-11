package export

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/internal/durable"
)

// nodeLogMagic starts the binary identity log. Records follow back to back:
// an op byte, then uvarint fields, names as uvarint length and bytes, each
// relative to the parent directory's key.
var nodeLogMagic = []byte("ovnfs nodes 2\n")

// Identity log operations.
const (
	opCounter byte = 1 + iota // id (= key), parent key, name
	opPin                     // parent key, name, key; the id derives from parent key and name
	opPinAt                   // id, parent key, name, key: a derived id away from its name
	opMove                    // key, parent key, name
	opRemove                  // key
	opNext                    // counter watermark
)

// errNodeLog reports a log that is inconsistent rather than torn.
var errNodeLog = errors.New("invalid node identity log")

// openNodeTable replays complete records and atomically compacts the logged
// identities. A log of the earlier JSON format contributes only its counter
// watermark: its ids go stale rather than ever naming another file.
func openNodeTable(name string, list func(dir string) ([]os.FileInfo, error)) (*nodeTable, error) {
	t := newNodeTable(list)
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(name)
	switch {
	case errors.Is(err, os.ErrNotExist), err == nil && len(data) == 0:
	case err != nil:
		return nil, err
	case bytes.HasPrefix(data, nodeLogMagic):
		if err := t.replay(data[len(nodeLogMagic):]); err != nil {
			return nil, err
		}
	case data[0] == '{':
		t.next = max(t.next, legacyWatermark(data))
	default:
		return nil, errNodeLog
	}
	if err := t.compact(name); err != nil {
		return nil, err
	}
	t.log, err = os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	// The registry owns the state lock before opening the export. Distinct
	// logs use distinct stores; no previous instance's owners are trusted.
	t.coldParent = name + ".cache"
	if err = t.openColdStore(); err != nil {
		t.log.Close()
		return nil, err
	}
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	go t.syncLoop()
	return t, nil
}

// legacyWatermark returns the counter after every id a JSON-format log issued.
func legacyWatermark(data []byte) uint64 {
	var next uint64
	for line := range bytes.Lines(data) {
		var rec struct {
			Op string `json:"op"`
			ID uint64 `json:"id"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		switch rec.Op {
		case "alloc":
			next = max(next, rec.ID+1)
		case "next":
			next = max(next, rec.ID)
		}
	}
	return next
}

// replay applies records up to a torn tail: a record cut short by a crash,
// or the zeros a filesystem may leave after one.
func (t *nodeTable) replay(b []byte) error {
	r := logReader{b: b}
	for len(r.b) > 0 {
		if err := t.apply(&r); err != nil {
			if errors.Is(err, errTorn) || bytes.Count(r.rest, []byte{0}) == len(r.rest) {
				return nil
			}
			return err
		}
	}
	return nil
}

// errTorn reports a record that ends with the file.
var errTorn = errors.New("torn node log record")

// logReader decodes records, remembering where the current one began.
type logReader struct {
	b    []byte
	rest []byte // from the start of the record being decoded
	err  error
}

// uvarint decodes a field, recording errTorn once the bytes run out.
func (r *logReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	switch {
	case n == 0:
		r.err = errTorn
	case n < 0:
		r.err = errNodeLog
	default:
		r.b = r.b[n:]
	}
	return v
}

// name decodes a length-prefixed basename.
func (r *logReader) name() string {
	n := r.uvarint()
	if r.err != nil {
		return ""
	}
	if n > uint64(len(r.b)) {
		r.err = errTorn
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	if s == "" || s == "." || s == ".." || strings.Contains(s, "/") {
		r.err = errNodeLog
	}
	return s
}

// apply decodes and replays one record.
func (t *nodeTable) apply(r *logReader) error {
	r.rest = r.b
	op := r.b[0]
	r.b = r.b[1:]
	switch op {
	case opCounter:
		id, parent, name := r.uvarint(), r.uvarint(), r.name()
		if r.err != nil {
			return r.err
		}
		if id >= derivedBit {
			return errNodeLog
		}
		return t.place(id, id, parent, name)
	case opPin:
		parent, name, key := r.uvarint(), r.name(), r.uvarint()
		if r.err != nil {
			return r.err
		}
		if parent >= maxDerivingKey {
			return errNodeLog
		}
		return t.place(derivedID(parent, nameHash(name)), key, parent, name)
	case opPinAt:
		id, parent, name, key := r.uvarint(), r.uvarint(), r.name(), r.uvarint()
		if r.err != nil {
			return r.err
		}
		if id&derivedBit == 0 || id >= idLimit {
			return errNodeLog
		}
		return t.place(id, key, parent, name)
	case opMove:
		key, parent, name := r.uvarint(), r.uvarint(), r.name()
		if r.err != nil {
			return r.err
		}
		n, d := t.byKey[key], t.byKey[parent]
		if n == nil || d == nil || n == t.root {
			return errNodeLog
		}
		for c := d; c != nil; c = c.parent {
			if c == n {
				return errNodeLog
			}
		}
		if dst := d.kids[name]; dst != n {
			t.drop(dst)
		}
		delete(n.parent.kids, n.name)
		n.name, n.parent = name, d
		t.link(d, n)
		return nil
	case opRemove:
		key := r.uvarint()
		if r.err != nil {
			return r.err
		}
		t.drop(t.byKey[key])
		return nil
	case opNext:
		next := r.uvarint()
		if r.err != nil {
			return r.err
		}
		t.next = max(t.next, next)
		return nil
	}
	return errNodeLog
}

// place replays a logged node under the directory keyed parent.
func (t *nodeTable) place(id, key, parent uint64, name string) error {
	d := t.byKey[parent]
	if d == nil || id < 2 || key < 2 || t.byID[id] != nil || t.byKey[key] != nil || d.kids[name] != nil {
		return errNodeLog
	}
	k := &node{id: id, key: key, name: name, parent: d}
	t.link(d, k)
	t.byKey[key] = k
	if key >= t.next {
		t.next = key + 1
	}
	return nil
}

// compact atomically replaces the log with the logged nodes, parents first,
// and the counter watermark.
func (t *nodeTable) compact(name string) error {
	b := append([]byte(nil), nodeLogMagic...)
	queue := []*node{t.root}
	for i := 0; i < len(queue); i++ {
		for _, k := range queue[i].kids {
			if k.key != 0 {
				b = appendNode(b, k)
				queue = append(queue, k)
			}
		}
	}
	b = binary.AppendUvarint(append(b, opNext), t.next)
	tmp, err := os.CreateTemp(filepath.Dir(name), ".nodes-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := durable.Sync(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), name); err != nil {
		return err
	}
	return syncNodeDirectory(filepath.Dir(name))
}

// appendNode encodes the record that places logged node k at its position.
func appendNode(b []byte, k *node) []byte {
	pk := k.parent.key
	switch {
	case k.id == k.key:
		b = binary.AppendUvarint(append(b, opCounter), k.id)
		b = binary.AppendUvarint(b, pk)
		return appendName(b, k.name)
	case pk < maxDerivingKey && k.id == derivedID(pk, nameHash(k.name)):
		b = binary.AppendUvarint(append(b, opPin), pk)
	default:
		b = binary.AppendUvarint(append(b, opPinAt), k.id)
		b = binary.AppendUvarint(b, pk)
	}
	b = appendName(b, k.name)
	return binary.AppendUvarint(b, k.key)
}

// appendName encodes a basename.
func appendName(b []byte, name string) []byte {
	return append(binary.AppendUvarint(b, uint64(len(name))), name...)
}

// recordNode queues the record placing logged node k.
func (t *nodeTable) recordNode(k *node) {
	if t.log != nil && t.logErr == nil {
		t.pending = appendNode(t.pending, k)
	}
}

// recordMove queues the move of logged node k to its current position.
func (t *nodeTable) recordMove(k *node) {
	if t.log != nil && t.logErr == nil {
		b := binary.AppendUvarint(append(t.pending, opMove), k.key)
		b = binary.AppendUvarint(b, k.parent.key)
		t.pending = appendName(b, k.name)
	}
}

// recordRemove queues the retirement of logged node k's subtree.
func (t *nodeTable) recordRemove(k *node) {
	if t.log != nil && t.logErr == nil {
		t.pending = binary.AppendUvarint(append(t.pending, opRemove), k.key)
	}
}

// flush appends the current operation's records in one write while the node
// mutex is held, so a crash tears at most the last operation.
func (t *nodeTable) flush() {
	if len(t.pending) == 0 {
		return
	}
	if t.log != nil && t.logErr == nil {
		if _, err := t.log.Write(t.pending); err != nil {
			t.logErr = err
		} else {
			t.dirty = true
		}
	}
	t.pending = t.pending[:0]
}

// syncLoop groups durable flushes without retaining records in userspace
// buffers. It flushes outside the node mutex: an fsync (FlushFileBuffers on
// Windows, often milliseconds) must not stall every lookup and allocation.
// Records appended meanwhile mark the log dirty again for the next flush.
func (t *nodeTable) syncLoop() {
	defer close(t.done)
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			t.mu.Lock()
			dirty := t.dirty
			t.dirty = false
			t.mu.Unlock()
			if !dirty {
				continue
			}
			if err := durable.Sync(t.log); err != nil {
				t.mu.Lock()
				if t.logErr == nil {
					t.logErr = err
				}
				t.mu.Unlock()
			}
		case <-t.stop:
			return
		}
	}
}

// close stops the flush worker and makes every appended operation durable.
func (t *nodeTable) close() error {
	t.closeOnce.Do(func() {
		if t.coldStop != nil {
			close(t.coldStop) // reclamation never delays the next daemon
		}
		if t.stop != nil {
			close(t.stop)
			<-t.done
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.log != nil {
			if err := durable.Sync(t.log); err != nil && t.logErr == nil {
				t.logErr = err
			}
			if err := t.log.Close(); err != nil && t.logErr == nil {
				t.logErr = err
			}
		}
		if t.coldParent == "" && t.coldRoot != "" {
			// Tables without a log own a private temporary store, not state.
			go os.RemoveAll(t.coldRoot)
		}
		// State-backed indexes stay in place until the next open detaches
		// them. Neither deletion nor its completion holds up close.
	})
	return errors.Join(t.logErr, t.cacheErr)
}
