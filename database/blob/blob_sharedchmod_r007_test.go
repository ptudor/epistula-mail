package blob

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSharedModeChmodsOnlyNewLevels is the R-007 EPERM regression: the
// shared-mode chmod walk must touch only the levels MkdirAll just created
// (which the calling process owns), never pre-existing ancestors — in the
// prescribed two-user deployment those belong to the other daemon, chmod(2)
// requires ownership, and an EPERM there broke every cross-daemon APPEND or
// delivery into an existing tenant tree. The test simulates the foreign
// owner by making chmod fail EPERM on every level that existed before the
// second write.
func TestSharedModeChmodsOnlyNewLevels(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetGroupWritable(true)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	bucket := BucketFromTime(time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC))
	writeOne := func(payload string) {
		t.Helper()
		w, err := s.NewWriter(KindRaw, "alice", bucket)
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if _, err := w.Write([]byte(payload)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, _, _, err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	// First write creates <root>/alice/{tmp,raw/<bucket>/aa/bb} as "the
	// other daemon" would have.
	writeOne("first blob payload")

	// Snapshot every directory that now exists.
	existing := make(map[string]bool)
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			existing[path] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}

	// Second write (different sha, same bucket): only newly-created shard
	// levels may be chmodded. Pre-existing levels fail EPERM, as they would
	// for a process that doesn't own them.
	var calls []string
	realChmod := osChmod
	osChmod = func(path string, mode os.FileMode) error {
		calls = append(calls, path)
		if existing[path] {
			return &os.PathError{Op: "chmod", Path: path, Err: os.ErrPermission}
		}
		return realChmod(path, mode)
	}
	defer func() { osChmod = realChmod }()

	writeOne("second blob payload — different sha")

	touchedExisting := 0
	for _, p := range calls {
		if existing[p] {
			touchedExisting++
			t.Errorf("chmod walk touched pre-existing level %s", p)
		}
	}
	if touchedExisting == 0 && len(calls) == 0 {
		// The second sha happened to land in the same aa/bb shard — nothing
		// new to create, and correctly nothing chmodded. Force a fresh
		// tenant chain to prove new levels do get the walk.
		writeOneTenant := func() {
			t.Helper()
			w, err := s.NewWriter(KindRaw, "bob", bucket)
			if err != nil {
				t.Fatalf("NewWriter(bob): %v", err)
			}
			if _, err := w.Write([]byte("bob payload")); err != nil {
				t.Fatalf("Write(bob): %v", err)
			}
			if _, _, _, err := w.Close(); err != nil {
				t.Fatalf("Close(bob): %v", err)
			}
		}
		writeOneTenant()
		if len(calls) == 0 {
			t.Fatal("no chmod calls recorded for a brand-new tenant chain")
		}
	}

	// Every newly-created level must carry the shared bits despite the
	// process umask.
	for _, p := range calls {
		if existing[p] {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if fi.Mode()&os.ModeSetgid == 0 || fi.Mode().Perm()&0o070 != 0o070 {
			t.Errorf("new level %s mode %v missing setgid/group bits", p, fi.Mode())
		}
	}
}

// TestSharedModeCreationRaceTolerated: if a cooperating daemon wins the
// creation race between this process's existence probe and its MkdirAll, the
// chmod on that level fails EPERM — tolerated when the winner's own walk
// already set the shared bits, and an error when the bits are genuinely
// missing (nothing would be able to write there).
func TestSharedModeCreationRaceTolerated(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetGroupWritable(true)

	dir := filepath.Join(root, "alice", "raw", "2026", "06", "08", "aa", "bb")

	// Winner already created + chmodded the level; loser's chmod EPERMs.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for p := dir; p != root; p = filepath.Dir(p) {
		if err := os.Chmod(p, os.ModeSetgid|0o770); err != nil {
			t.Fatal(err)
		}
	}
	realChmod := osChmod
	osChmod = func(path string, mode os.FileMode) error {
		return &os.PathError{Op: "chmod", Path: path, Err: os.ErrPermission}
	}
	defer func() { osChmod = realChmod }()

	if err := s.chmodShared(dir); err != nil {
		t.Errorf("chmodShared on a shared-bits dir with EPERM: %v, want tolerated", err)
	}

	// Same EPERM on a dir missing the shared bits must surface.
	bare := filepath.Join(root, "alice", "raw", "2026", "06", "08", "cc")
	if err := os.MkdirAll(bare, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := s.chmodShared(bare); err == nil {
		t.Error("chmodShared on a group-inaccessible dir with EPERM: want error, got nil")
	}
}
