package refs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

func TestReadPackedRefsFiltered(t *testing.T) {
	const a = "1111111111111111111111111111111111111111"
	const b = "2222222222222222222222222222222222222222"
	const c = "3333333333333333333333333333333333333333"
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "packed-refs")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	prefix := func(p string) func([]byte) bool {
		return func(n []byte) bool { return strings.HasPrefix(string(n), p) }
	}
	f := object.SHA1

	// Unsorted, duplicated; peeled lines attach only to their own ref.
	p := write(a + " refs/z/b\n^" + c + "\n" + b + " refs/x/y\n^" + c + "\n" + b + " refs/z/a\n" + c + " refs/z/a\n")
	got, err := readPackedRefs(p, f, prefix("refs/z/"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "refs/z/a" || got[0].ID.String() != b || !got[0].Peeled.IsZero() ||
		got[1].Name != "refs/z/a" || got[1].ID.String() != c || got[2].Name != "refs/z/b" || got[2].Peeled.String() != c {
		t.Fatalf("unexpected refs: %+v", got)
	}
	if got, err = readPackedRefs(p, f, prefix("refs/none/")); err != nil || len(got) != 0 {
		t.Fatalf("no match: %v %v", got, err)
	}

	// Corruption in filtered-out records is still reported.
	for _, body := range []string{
		"zz" + a[2:] + " refs/x/y\n",
		a + " refs/x/y\n^bad\n",
		"^" + c + "\n" + a + " refs/x/y\n",
	} {
		if _, err := readPackedRefs(write(body), f, prefix("refs/z/")); err == nil {
			t.Errorf("expected error for %q", body)
		}
	}
}
