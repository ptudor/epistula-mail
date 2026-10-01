package blob

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSweepStaleTmp is the R-031 regression: gc mark's tmp sweep removes stale
// staging files (tmp/blob-* older than grace) while leaving young ones and
// non-staging files intact.
func TestSweepStaleTmp(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	if err := s.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	tenant, err := ParseTenant("alice")
	if err != nil {
		t.Fatalf("ParseTenant: %v", err)
	}
	tmpDir := s.tenantTmpDir(tenant)
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}

	stale := filepath.Join(tmpDir, "blob-stale")
	fresh := filepath.Join(tmpDir, "blob-fresh")
	keep := filepath.Join(tmpDir, "not-a-blob") // wrong prefix — never touched
	for _, p := range []string{stale, fresh, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o640); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{stale, keep} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	removed, err := s.SweepStaleTmp(time.Hour)
	if err != nil {
		t.Fatalf("SweepStaleTmp: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d, want 1 (only the stale blob-*)", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale blob-* was not removed (err=%v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh blob-* was wrongly removed: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("non-staging file was wrongly removed: %v", err)
	}
}
