package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/migrations"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestMisplacedBlobIsReportedNotReaped is the RA6X-062 regression.
//
// Walk reported a physical blob using only its date and basename digest,
// ignoring the two shard components, while PathFor reconstructs the canonical
// path from the digest. GC therefore marked and swept against a path the file
// is NOT at: the misplaced copy survives every pass while mark reports work
// against another location, and two such files for one digest produce
// duplicate keys in a single candidate upsert.
func TestMisplacedBlobIsReportedNotReaped(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('shardbox', 'x')`); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	raw := []byte(gcTestMessage)
	blobDate := time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(blobDate)

	// The canonical copy, plus one under deliberately wrong shard directories.
	sha, canonical := writeTestBlob(t, store, "shardbox", bucket, raw)
	backdateFile(t, canonical, 48*time.Hour)

	wrong := filepath.Join(store.Root(), "shardbox", "raw", string(bucket), "00", "11", sha+".eml")
	if err := os.MkdirAll(filepath.Dir(wrong), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(wrong, raw, 0o640); err != nil {
		t.Fatalf("write misplaced: %v", err)
	}
	backdateFile(t, wrong, 48*time.Hour)

	// Mark must not fail on the duplicate key, and must not treat the
	// misplaced file as a canonical blob.
	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatalf("gcMark: exit %d", code)
	}
	// Exactly one candidate: the canonical file. The misplaced one is not a
	// reference and not a candidate.
	if n := candidateCount(t, ctx, db); n != 1 {
		t.Fatalf("marked %d candidates, want 1 (the canonical copy only)", n)
	}

	makeEligible(t, ctx, db)
	if code := gcSweep(ctx, db, store, time.Hour); code != EX_OK {
		t.Fatalf("gcSweep: exit %d", code)
	}

	// The canonical orphan is reaped; the misplaced file is left for an
	// operator to resolve, never silently deleted at a reconstructed path.
	if fileExists(t, canonical) {
		t.Error("the canonical orphan was not reaped")
	}
	if !fileExists(t, wrong) {
		t.Error("a misplaced blob was deleted; that is an operator decision, not GC's")
	}
}

// TestMisplacedBlobIsSurfacedToTheCaller pins the diagnostic itself, so an
// operator learns the file exists rather than having it silently skipped.
func TestMisplacedBlobIsSurfacedToTheCaller(t *testing.T) {
	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	raw := []byte(gcTestMessage)
	bucket := blob.BucketFromTime(time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC))
	sha, _ := writeTestBlob(t, store, "shardbox", bucket, raw)

	wrong := filepath.Join(store.Root(), "shardbox", "raw", string(bucket), "zz", "zz", sha+".eml")
	if err := os.MkdirAll(filepath.Dir(wrong), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(wrong, raw, 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}

	var reported []string
	store.SetOnMisplacedBlob(func(kind blob.Kind, tenant blob.Tenant, shaHex, path string) {
		reported = append(reported, path)
	})
	var canonicalSeen int
	if err := store.Walk(blob.KindRaw, func(kind blob.Kind, tenant blob.Tenant, b blob.Bucket, shaHex, path string, info os.FileInfo) error {
		canonicalSeen++
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if canonicalSeen != 1 {
		t.Fatalf("Walk reported %d canonical blobs, want 1", canonicalSeen)
	}
	if len(reported) != 1 || reported[0] != wrong {
		t.Fatalf("misplaced blobs reported = %v, want [%s]", reported, wrong)
	}
}

// TestLegacyGCCandidatesArePurged is the RA6X-051 regression.
//
// Migration 007 gave existing gc_candidates rows tenant ” via a transient
// DEFAULT, assuming mark rebuilds the table — but mark UPSERTS tenant-qualified
// rows rather than truncating. blob.ParseTenant("") fails, so sweep SKIPS such
// a row rather than removing it, and every sweep reports the same invalid
// candidates forever, hiding an actual cleanup failure.
func TestLegacyGCCandidatesArePurged(t *testing.T) {
	db, _ := pgtest.OpenBare(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	all, err := migrations.All()
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}
	// Apply through 006, seed an eligible candidate, then let 007 give it the
	// empty tenant exactly as a real upgrade did.
	for _, m := range all {
		if m.Version > 6 {
			break
		}
		if _, err := db.Pool().Exec(ctx, m.SQL); err != nil {
			t.Fatalf("apply %d: %v", m.Version, err)
		}
	}
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO gc_candidates (sha256, kind, bucket, first_seen_at, generation_marked)
		VALUES (decode(repeat('ab',32),'hex'), 'raw', '2026/01/01', now() - interval '2 days', 1)`,
	); err != nil {
		t.Fatalf("seed legacy candidate: %v", err)
	}

	for _, m := range all {
		if m.Version <= 6 {
			continue
		}
		if _, err := db.Pool().Exec(ctx, m.SQL); err != nil {
			t.Fatalf("apply %d: %v", m.Version, err)
		}
	}

	var unscoped int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM gc_candidates WHERE tenant = ''`).Scan(&unscoped); err != nil {
		t.Fatalf("count: %v", err)
	}
	if unscoped != 0 {
		t.Fatalf("%d unscoped candidate(s) survived; sweep can never act on them", unscoped)
	}
}
