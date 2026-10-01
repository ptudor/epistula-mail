package blob

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGroupWritableModes is the R-007 regression: in shared-group mode the
// blob store must create setgid, group-writable directories (so a cooperating
// daemon in the storage group can APPEND) and group-writable files, while
// CheckPermissions (which only rejects "other" bits) still passes.
func TestGroupWritableModes(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetGroupWritable(true)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Write a blob and inspect the on-disk modes.
	bucket := BucketFromTime(time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC))
	w, err := s.NewWriter(KindRaw, "alice", bucket)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write([]byte("hello world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sha, _, _, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The tenant tmp dir must be setgid + group-writable (drwxrws---).
	tmpDir := filepath.Join(root, "alice", "tmp")
	fi, err := os.Stat(tmpDir)
	if err != nil {
		t.Fatalf("stat tmp: %v", err)
	}
	if fi.Mode()&os.ModeSetgid == 0 {
		t.Errorf("tmp dir mode %v missing setgid", fi.Mode())
	}
	if fi.Mode().Perm()&0o070 != 0o070 {
		t.Errorf("tmp dir mode %v missing group rwx", fi.Mode())
	}
	if fi.Mode().Perm()&0o007 != 0 {
		t.Errorf("tmp dir mode %v has 'other' bits", fi.Mode())
	}

	// The shard (parent of the blob) must be group-writable so a peer daemon
	// can os.Link into it.
	finalPath, err := s.PathFor(KindRaw, "alice", bucket, sha)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	shard := filepath.Dir(finalPath)
	sfi, err := os.Stat(shard)
	if err != nil {
		t.Fatalf("stat shard: %v", err)
	}
	if sfi.Mode().Perm()&0o070 != 0o070 {
		t.Errorf("shard dir mode %v missing group rwx", sfi.Mode())
	}

	// The blob file must be group-writable (0660), no 'other' bits.
	ffi, err := os.Stat(finalPath)
	if err != nil {
		t.Fatalf("stat blob: %v", err)
	}
	if ffi.Mode().Perm() != 0o660 {
		t.Errorf("blob file mode %v, want 0660", ffi.Mode().Perm())
	}

	// CheckPermissions must still pass (it rejects only 'other' bits).
	if err := s.CheckPermissions(); err != nil {
		t.Errorf("CheckPermissions failed for shared-group tree: %v", err)
	}
}

// TestOwnerOnlyModesUnchanged confirms the default (non-shared) store keeps the
// original owner-only modes.
func TestOwnerOnlyModesUnchanged(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root) // default: owner-only
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bucket := BucketFromTime(time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC))
	w, err := s.NewWriter(KindRaw, "bob", bucket)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	_, _ = w.Write([]byte("x"))
	sha, _, _, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	finalPath, _ := s.PathFor(KindRaw, "bob", bucket, sha)
	ffi, err := os.Stat(finalPath)
	if err != nil {
		t.Fatalf("stat blob: %v", err)
	}
	if ffi.Mode().Perm() != 0o640 {
		t.Errorf("default blob file mode %v, want 0640", ffi.Mode().Perm())
	}
	tmpDir := filepath.Join(root, "bob", "tmp")
	fi, _ := os.Stat(tmpDir)
	if fi.Mode()&os.ModeSetgid != 0 {
		t.Errorf("default tmp dir %v should not be setgid", fi.Mode())
	}
}
