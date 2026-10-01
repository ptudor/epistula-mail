package blob

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerificationPeerShardSyncsDespiteBucketMarker(t *testing.T) {
	s, tenant, bucket := verifyStore(t)
	_, path := writeVerifyBlob(t, s, KindRaw, tenant, bucket, []byte("initial"))
	marker, _ := markerPathFor(s.root, filepath.Dir(path))
	// A second writer has created a different shard and paused before fsync.
	shard := filepath.Join(filepath.Dir(marker), "ee", "ff")
	if err := os.MkdirAll(shard, 0750); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	orig := fsyncDir
	fsyncDir = func(path string) error { seen[path] = true; return orig(path) }
	defer func() { fsyncDir = orig }()
	// Use the SAME Store, whose old bucket proof is also cached.
	if err := s.ensureDir(shard); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{s.root, filepath.Dir(marker), filepath.Dir(shard), shard} {
		if !seen[dir] {
			t.Errorf("acknowledged unsynced peer directory %s", dir)
		}
	}
}
