package blob

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestCloseRetriesLinkOnPruneRace is the R-046 regression: if a concurrent gc
// prune rmdir's the shard parent between ensureDir and os.Link, Close must
// re-create the parent and retry the link once, succeeding — not bounce the
// delivery EX_TEMPFAIL. The first injected link removes the parent and returns
// ENOENT; the retry re-creates it and links for real.
func TestCloseRetriesLinkOnPruneRace(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	tenant, err := ParseTenant("racer")
	if err != nil {
		t.Fatalf("ParseTenant: %v", err)
	}
	bucket := Bucket("2003/06/15")

	orig := osLink
	calls := 0
	osLink = func(oldPath, newPath string) error {
		calls++
		if calls == 1 {
			// Simulate the prune: the freshly-created parent vanishes.
			_ = os.RemoveAll(filepath.Dir(newPath))
			return &os.LinkError{Op: "link", Old: oldPath, New: newPath, Err: syscall.ENOENT}
		}
		return orig(oldPath, newPath)
	}
	defer func() { osLink = orig }()

	w, err := s.NewWriter(KindRaw, tenant, bucket)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write([]byte("hello world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sha, _, deduped, err := w.Close()
	if err != nil {
		t.Fatalf("Close failed despite the retry: %v", err)
	}
	if deduped {
		t.Errorf("unexpected dedup on a fresh blob")
	}
	if calls < 2 {
		t.Fatalf("link retry did not fire (calls=%d, want >=2)", calls)
	}
	if ok, err := s.Exists(KindRaw, tenant, bucket, sha); err != nil || !ok {
		t.Errorf("blob absent after retried Close (exists=%v err=%v)", ok, err)
	}
}
