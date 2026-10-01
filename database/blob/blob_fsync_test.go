package blob

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fsyncRecorder wraps fsyncDir to record which directories were fsynced,
// restoring the original on cleanup.
func fsyncRecorder(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var synced []string
	orig := fsyncDir
	fsyncDir = func(path string) error {
		mu.Lock()
		synced = append(synced, path)
		mu.Unlock()
		return orig(path)
	}
	t.Cleanup(func() { fsyncDir = orig })
	return &synced
}

func writeBlob(t *testing.T, s *Store, tenant Tenant, bucket Bucket, data []byte) string {
	t.Helper()
	w, err := s.NewWriter(KindRaw, tenant, bucket)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sha, _, _, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	return sha
}

// TestFsyncFreshTenantChain is R-028 part 2: a first-ever write to a fresh
// tenant fsyncs each newly-created ancestor level (so a crash can't drop the
// subtree). The blob's own parent shard is fsynced by Close.
func TestFsyncFreshTenantChain(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bucket := BucketFromTime(time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC))

	synced := fsyncRecorder(t)
	sha := writeBlob(t, s, "alice", bucket, []byte("hello"))

	finalPath, _ := s.PathFor(KindRaw, "alice", bucket, sha)
	shard := filepath.Dir(finalPath) // .../alice/raw/2026/06/08/aa/bb

	// The shard (Close's parent fsync) and each created ancestor up the chain
	// must have been fsynced. Walk from the tenant root's raw dir down.
	wantFsynced := []string{
		shard,
		filepath.Dir(shard),               // .../aa
		filepath.Dir(filepath.Dir(shard)), // .../08
		filepath.Join(root, "alice", "raw", "2026", "06"), // .../06
		filepath.Join(root, "alice", "raw", "2026"),       // .../2026
		filepath.Join(root, "alice", "raw"),               // .../raw
	}
	set := map[string]bool{}
	for _, p := range *synced {
		set[p] = true
	}
	for _, want := range wantFsynced {
		if !set[want] {
			t.Errorf("expected fsync of %q; fsynced set = %v", want, *synced)
		}
	}
}

// TestFsyncDedupParent is R-028 part 1: a dedup hit (writing identical bytes
// into the same tenant+bucket) still fsyncs the parent shard before Close
// returns success, so a PG row is never committed against a non-durable entry.
func TestFsyncDedupParent(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bucket := BucketFromTime(time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC))

	// First write creates the blob.
	sha := writeBlob(t, s, "bob", bucket, []byte("dup me"))
	finalPath, _ := s.PathFor(KindRaw, "bob", bucket, sha)
	shard := filepath.Dir(finalPath)

	// Second write of identical bytes dedups (os.Link EEXIST).
	synced := fsyncRecorder(t)
	w, err := s.NewWriter(KindRaw, "bob", bucket)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	_, _ = w.Write([]byte("dup me"))
	_, _, deduped, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !deduped {
		t.Fatal("second identical write should have deduped")
	}
	found := false
	for _, p := range *synced {
		if p == shard {
			found = true
		}
	}
	if !found {
		t.Errorf("dedup path did not fsync the parent shard %q; fsynced = %v", shard, *synced)
	}
}
