package source

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/store"
)

// convertAttrs are the gitattributes under which git may convert content between
// blob and work tree, in the order classify reads them.
var convertAttrs = []string{"text", "eol", "crlf", "filter", "ident", "working-tree-encoding"}

// conversion is what git may do to a path's bytes between its blob and the work tree.
type conversion int

const (
	convertNone conversion = iota // the blob is the work-tree bytes
	convertEOL                    // end-of-line normalization only, which changes the size iff it changes bytes
	convertAny                    // filter driver, ident or encoding: only reading the file tells
)

// classify decides a path's conversion from its convertAttrs values, as git's
// convert.c does; unknown values count as the conversion they might request.
func classify(values []string, c git.Conversion) conversion {
	text, eol, crlf, filter, ident, encoding := values[0], values[1], values[2], values[3], values[4], values[5]
	if c.Filters[filter] || ident == git.AttrSet || isAttrValue(encoding) {
		return convertAny
	}
	switch text {
	case git.AttrUnset:
		return convertNone
	case git.AttrUnspecified:
		switch crlf {
		case git.AttrUnset:
			return convertNone
		case git.AttrUnspecified:
			if eol == git.AttrUnspecified && !c.AutoCRLF {
				return convertNone
			}
		}
	}
	return convertEOL
}

// isAttrValue reports an attribute set to a value rather than a bare state.
func isAttrValue(v string) bool {
	return v != git.AttrUnspecified && v != git.AttrSet && v != git.AttrUnset
}

// verifier proves, for one repository, which stat-clean tracked files' index blob
// ids equal their work-tree bytes; every other candidate is hashed. Walk goroutine only.
type verifier struct {
	w         *walker
	repo      *git.Repo
	index     *git.Index // the captured index the repository's attributes fall back to
	path      string     // the repository's root-relative work tree
	started   bool       // conv and attrs were read
	conv      git.Conversion
	attrs     *git.Attributes // nil when the repository's conversion rules could not be read
	objects   *git.Objects    // opened with the first end-of-line candidate
	noObjects bool            // the object reader could not be opened
	warned    bool
}

// verifier returns the verifier of the repository whose work tree is root-relative path.
func (w *walker) verifier(path string, repo *git.Repo, index *git.Index) *verifier {
	if v := w.verifiers[path]; v != nil {
		return v
	}
	v := &verifier{w: w, repo: repo, index: index, path: path}
	w.verifiers[path] = v
	return v
}

// add takes the index blob oid as the content of the file at repository-relative
// path rel when git stores that file unconverted, else queues it for hashing.
func (v *verifier) add(ctx context.Context, rel, oid string, j hashJob) {
	if !v.started {
		v.started = true
		conv, err := v.repo.Conversion(ctx)
		if err == nil {
			v.conv = conv
			v.attrs, err = v.repo.Attributes(convertAttrs, git.AttributeOptions{Index: v.index})
		}
		if err != nil {
			v.attrs = nil
			v.warn(err)
		}
	}
	if v.attrs == nil {
		v.w.jobs <- j
		return
	}
	values, err := v.attrs.Check(rel)
	if err != nil {
		v.warn(err)
		v.w.jobs <- j
		return
	}
	switch classify(values, v.conv) {
	case convertNone:
		v.take(oid, j)
	case convertEOL:
		v.sized(oid, j)
	default:
		v.w.jobs <- j
	}
}

// sized takes an end-of-line candidate whose index blob has the file's size.
func (v *verifier) sized(oid string, j hashJob) {
	if v.objects == nil && !v.noObjects {
		var err error
		if v.objects, err = v.repo.Objects(); err != nil {
			v.noObjects = true
			v.warn(err)
		}
	}
	if v.objects == nil {
		v.w.jobs <- j
		return
	}
	size, err := v.objects.Size(oid)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// A blob missing locally (a partial clone) is not fetched just to compare sizes.
		v.w.jobs <- j
	case err != nil:
		v.warn(err)
		v.w.jobs <- j
	case size == j.info.Size():
		v.take(oid, j)
	default:
		v.w.jobs <- j
	}
}

// take records the index blob as the file's content: a work-tree content id the
// stat cache keeps, whatever attributes say later, while git keeps the blob.
func (v *verifier) take(oid string, j hashJob) {
	id := store.OID(oid)
	v.w.mu.Lock()
	defer v.w.mu.Unlock()
	v.w.res.Files[j.p] = store.File{Mode: j.mode, ID: id}
	v.w.cacheOut[j.p] = newStatEntry(j.info, j.key, j.mode, id)
	v.w.repoHeld = true
}

// warn reports the first failure to check this repository's content conversion.
func (v *verifier) warn(err error) {
	if v.warned {
		return
	}
	v.warned = true
	where := v.path
	if where == "" {
		where = "."
	}
	v.w.warn(fmt.Sprintf("%s: cannot check git content conversion, tracked files hashed: %v", where, err))
}

// closeVerifiers releases the verifiers' object readers once the walk is done.
func (w *walker) closeVerifiers() {
	for _, v := range w.verifiers {
		if v.objects != nil {
			v.objects.Close()
			v.objects = nil
		}
	}
}
