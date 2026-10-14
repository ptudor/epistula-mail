package blob

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCloseRetriesPrunedAncestry(t *testing.T) {
	s, tenant, bucket := verifyStore(t)
	w, err := s.NewWriter(KindRaw, tenant, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("prune-race")); err != nil {
		t.Fatal(err)
	}
	orig := fsyncDir
	calls := 0
	fsyncDir = func(path string) error {
		calls++
		if calls == 1 {
			return &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
		}
		return orig(path)
	}
	defer func() { fsyncDir = orig }()
	if _, _, _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatal("did not retry")
	}
}

func TestExistingRootDoesNotSyncParent(t *testing.T) {
	s, tenant, bucket := verifyStore(t)
	orig := fsyncDir
	parent := filepath.Dir(s.root)
	fsyncDir = func(path string) error {
		if path == parent {
			return os.ErrPermission
		}
		return orig(path)
	}
	defer func() { fsyncDir = orig }()
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	writeBlob(t, s, tenant, bucket, []byte("read-protected parent"))
}

func TestNewRootSyncsCreatedParents(t *testing.T) {
	parent := t.TempDir()
	s := NewStore(filepath.Join(parent, "one", "two"))
	synced := fsyncRecorder(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{parent, filepath.Join(parent, "one"), s.root} {
		found := false
		for _, got := range *synced {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing fsync %s: %v", want, *synced)
		}
	}
}

func TestRecreatedPeerChainMustSyncAgain(t *testing.T) {
	s, tenant, bucket := verifyStore(t)
	_, path := writeVerifyBlob(t, s, KindRaw, tenant, bucket, []byte("pruned"))
	shard := filepath.Dir(path)
	// Model GC pruning the leaf and its parent, followed by a peer's MkdirAll.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shard); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Dir(shard)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shard, 0750); err != nil {
		t.Fatal(err)
	}
	orig := fsyncDir
	syncFailure := errors.New("peer chain has not been synced")
	fsyncDir = func(p string) error {
		if p == filepath.Dir(shard) {
			return syncFailure
		}
		return orig(p)
	}
	defer func() { fsyncDir = orig }()
	if err := s.ensureDir(shard); !errors.Is(err, syncFailure) {
		t.Fatalf("acknowledged recreated chain without syncing: %v", err)
	}
}
