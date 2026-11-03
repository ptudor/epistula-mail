package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// strandedSetup prepares a store and the chain a writer killed between mkdir
// and chmod leaves behind: every level up to the date bucket carries
// wantDirMode, and the first shard level of payload's blob exists with
// strandedMode and nothing beneath it. On junia on 2026-11-03 that level was
// ptudor/raw/2026/11/03/69, at 2700 under Postfix's umask 077.
func strandedSetup(t *testing.T, shared bool, strandedMode os.FileMode, payload string) (s *Store, shardDir, leafDir string, bucket Bucket) {
	t.Helper()
	root := t.TempDir()
	s = NewStore(root)
	s.SetGroupWritable(shared)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bucket = BucketFromTime(time.Date(2026, 11, 3, 0, 37, 31, 0, time.UTC))
	sum := sha256.Sum256([]byte(payload))
	final, err := s.PathFor(KindRaw, "alice", bucket, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	leafDir = filepath.Dir(final)
	shardDir = filepath.Dir(leafDir)
	if err := os.MkdirAll(shardDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for p := filepath.Dir(shardDir); p != root; p = filepath.Dir(p) {
		if err := os.Chmod(p, s.dirMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(shardDir, strandedMode); err != nil {
		t.Fatal(err)
	}
	return s, shardDir, leafDir, bucket
}

func wantDirMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode() & (os.ModeSetgid | os.ModePerm); got != want {
		t.Errorf("%s mode %v, want %v", path, got, want)
	}
}

// TestRetryRepairsLevelStrandedByKilledWriter is the junia 2026-11-03
// regression. The first delivery attempt was killed after mkdir created the
// shard level and before the chmod walk. The retry found the level existing,
// created only the level below it, and chmodded only that one, so IMAP's group
// could not enter the level and every FETCH of the stored message failed. The
// retry must now repair the stranded level it writes beneath.
func TestRetryRepairsLevelStrandedByKilledWriter(t *testing.T) {
	const payload = "the retried delivery"
	s, shardDir, leafDir, bucket := strandedSetup(t, true, os.ModeSetgid|0o700, payload)

	writeBlob(t, s, "alice", bucket, []byte(payload))

	wantDirMode(t, shardDir, os.ModeSetgid|0o770)
	wantDirMode(t, leafDir, os.ModeSetgid|0o770)
}

// TestOwnerOnlyModeRepairsStrandedLevel: owner-only mode relies on the group
// r-x bits for the IMAP server's reads, so a level stranded at 0700 is
// repaired to the store's 0750.
func TestOwnerOnlyModeRepairsStrandedLevel(t *testing.T) {
	const payload = "owner-only retried delivery"
	s, shardDir, _, bucket := strandedSetup(t, false, 0o700, payload)

	writeBlob(t, s, "alice", bucket, []byte(payload))

	wantDirMode(t, shardDir, 0o750)
}

// TestRepairsStrandedTmpDir: staging is never synced, so the tmp dir gets its
// own check. A tmp dir stranded at 2700 would refuse the peer daemon's
// CreateTemp and with it every IMAP APPEND for the tenant.
func TestRepairsStrandedTmpDir(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetGroupWritable(true)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tmp := filepath.Join(root, "alice", "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "alice"), os.ModeSetgid|0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, os.ModeSetgid|0o700); err != nil {
		t.Fatal(err)
	}

	writeBlob(t, s, "alice", BucketFromTime(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)), []byte("via a stranded tmp"))

	wantDirMode(t, tmp, os.ModeSetgid|0o770)
}

// TestStrandedLevelOfPeerIsLeftAndLogged: a stranded level owned by the peer
// daemon cannot be chmodded (R-007), so it is left for that daemon and logged.
// The writer's own delivery still succeeds rather than deferring mail over a
// directory only the peer can fix.
func TestStrandedLevelOfPeerIsLeftAndLogged(t *testing.T) {
	const payload = "written under a peer's level"
	s, shardDir, _, bucket := strandedSetup(t, true, os.ModeSetgid|0o700, payload)
	var logBuf bytes.Buffer
	s.SetLogger(slog.New(slog.NewTextHandler(&logBuf, nil)))

	realEUID := currentEUID
	currentEUID = func() int { return realEUID() + 1 }
	defer func() { currentEUID = realEUID }()

	writeBlob(t, s, "alice", bucket, []byte(payload))

	wantDirMode(t, shardDir, os.ModeSetgid|0o700)
	if !strings.Contains(logBuf.String(), "belongs to another user") ||
		!strings.Contains(logBuf.String(), shardDir) {
		t.Errorf("no log line names the unrepairable level %s:\n%s", shardDir, logBuf.String())
	}
}

// TestRepairLeavesHealthyLevelsAlone: a level that already carries the shared
// bits is not chmodded, which keeps R-007's rule that a writer never touches
// the peer daemon's levels.
func TestRepairLeavesHealthyLevelsAlone(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetGroupWritable(true)
	if err := s.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bucket := BucketFromTime(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	writeBlob(t, s, "alice", bucket, []byte("first"))

	var calls []string
	realChmod := osChmod
	osChmod = func(path string, mode os.FileMode) error {
		calls = append(calls, path)
		return realChmod(path, mode)
	}
	defer func() { osChmod = realChmod }()

	writeBlob(t, s, "alice", bucket, []byte("first")) // same content: every level already exists
	if len(calls) != 0 {
		t.Errorf("chmod called on healthy existing levels: %v", calls)
	}
}

// TestSharedGroupUmaskKeepsGroupBits: under Postfix's umask 077, mkdir alone
// produced the stranded 2700 level. Under SharedGroupUmask the group bits
// survive mkdir itself, so no chmod is needed to make a level usable.
func TestSharedGroupUmaskKeepsGroupBits(t *testing.T) {
	prev := syscall.Umask(0o077)
	defer syscall.Umask(prev)

	if got := UseSharedGroupUmask(); got != 0o077 {
		t.Errorf("UseSharedGroupUmask returned previous umask %#o, want 0o077", got)
	}
	dir := filepath.Join(t.TempDir(), "69")
	if err := os.Mkdir(dir, os.ModeSetgid|0o770); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o770 {
		t.Errorf("mkdir under SharedGroupUmask gave %v, want group rwx and no other bits", fi.Mode())
	}
}
