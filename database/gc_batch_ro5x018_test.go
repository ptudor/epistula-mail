package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestBlobReferencedSingleAndBatchedAgree is the RO5X-018 equivalence test.
//
// The hard GC invariant is that mark and sweep run identical reference logic:
// sweep re-checks one blob at a time under the advisory lock while mark now
// batches. If the two forms ever disagreed, a blob could be marked but never
// swept, or swept while still live. Both are built from one shared
// `referencePredicate` fragment; this asserts they actually agree, including
// on the cases the design calls out — the same content in two tenants, and the
// same content on two dates.
func TestBlobReferencedSingleAndBatchedAgree(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Two mailboxes, so cross-tenant scoping is exercised.
	ids := map[string]int64{}
	for _, name := range []string{"alice", "bob"} {
		var id int64
		if err := db.Pool().QueryRow(ctx,
			`INSERT INTO mailboxes (name, password_hash) VALUES ($1, 'x') RETURNING id`, name,
		).Scan(&id); err != nil {
			t.Fatalf("mailbox %s: %v", name, err)
		}
		ids[name] = id
		var folderID int64
		if err := db.Pool().QueryRow(ctx,
			`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
			 VALUES ($1, 'INBOX', 1, 1) RETURNING id`, id,
		).Scan(&folderID); err != nil {
			t.Fatalf("folder %s: %v", name, err)
		}
		ids[name+".folder"] = folderID
	}

	day1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

	mkSHA := func(b byte) []byte {
		s := make([]byte, 32)
		for i := range s {
			s[i] = b
		}
		return s
	}
	shaShared := mkSHA(0x11)   // same content, both tenants
	shaTwoDates := mkSHA(0x22) // same content, two dates, alice only
	shaAliceOnly := mkSHA(0x33)
	shaAbsent := mkSHA(0x44) // referenced by nobody

	insertMsg := func(mailbox string, uid int64, sha []byte, d time.Time) int64 {
		t.Helper()
		var msgID int64
		if err := db.Pool().QueryRow(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				subject, from_addr, to_addrs, cc_addrs, headers, text_body,
				bodystructure, flags
			) VALUES ($1,$2,$3,$4,10,$5,'s','a@x.invalid','{}','{}','{}','b','{}','{}')
			RETURNING id`,
			ids[mailbox+".folder"], uid, sha, d, d,
		).Scan(&msgID); err != nil {
			t.Fatalf("insert message: %v", err)
		}
		return msgID
	}

	// alice: shared blob on day1, two-date blob on day1 and day2, alice-only on day1.
	aliceMsg := insertMsg("alice", 1, shaShared, day1)
	insertMsg("alice", 2, shaTwoDates, day1)
	insertMsg("alice", 3, shaTwoDates, day2)
	insertMsg("alice", 4, shaAliceOnly, day1)
	// bob: the SAME content as alice's shared blob, same date — a different
	// blob in a different tenant subtree.
	insertMsg("bob", 1, shaShared, day1)

	// An attachment for the attachment-kind half.
	attSHA := mkSHA(0x55)
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO attachments (
			message_id, part_number, filename, content_type,
			content_id, disposition, size_bytes, sha256, blob_date
		) VALUES ($1,'2','f.pdf','application/pdf',NULL,'attachment',9,$2,$3)`,
		aliceMsg, attSHA, day1,
	); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}

	type probe struct {
		kind    blob.Kind
		mailbox string
		sha     []byte
		date    time.Time
		label   string
	}
	probes := []probe{
		{blob.KindRaw, "alice", shaShared, day1, "shared content, alice, right date"},
		{blob.KindRaw, "bob", shaShared, day1, "shared content, bob, right date"},
		{blob.KindRaw, "bob", shaAliceOnly, day1, "alice-only content probed as bob"},
		{blob.KindRaw, "alice", shaTwoDates, day1, "two-date content, day 1"},
		{blob.KindRaw, "alice", shaTwoDates, day2, "two-date content, day 2"},
		{blob.KindRaw, "alice", shaShared, day2, "right content, wrong date"},
		{blob.KindRaw, "alice", shaAbsent, day1, "unreferenced content"},
		{blob.KindAttachment, "alice", attSHA, day1, "attachment, referenced"},
		{blob.KindAttachment, "bob", attSHA, day1, "attachment probed as the wrong tenant"},
		{blob.KindAttachment, "alice", attSHA, day2, "attachment, wrong date"},
		{blob.KindAttachment, "alice", shaShared, day1, "raw content probed as an attachment"},
	}

	for _, p := range probes {
		t.Run(p.label, func(t *testing.T) {
			single, err := blobReferenced(ctx, db.Pool(), p.kind, ids[p.mailbox], p.sha, p.date)
			if err != nil {
				t.Fatalf("blobReferenced: %v", err)
			}
			batched, err := blobsReferenced(ctx, db.Pool(), p.kind, ids[p.mailbox], []blobProbe{{
				sha:        p.sha,
				bucketDate: p.date,
				shaHex:     hex.EncodeToString(p.sha),
				bucket:     blob.BucketFromTime(p.date),
			}})
			if err != nil {
				t.Fatalf("blobsReferenced: %v", err)
			}
			got := batched[hex.EncodeToString(p.sha)+"|"+string(blob.BucketFromTime(p.date))]
			if single != got {
				t.Errorf("MARK AND SWEEP DISAGREE: single=%v batched=%v — "+
					"a blob could be marked but never swept, or swept while live", single, got)
			}
		})
	}

	// And in one batch together: the batched form must classify a mixed chunk
	// exactly as the single form classifies each element.
	t.Run("mixed chunk", func(t *testing.T) {
		var chunk []blobProbe
		var want []bool
		for _, p := range probes {
			if p.kind != blob.KindRaw || p.mailbox != "alice" {
				continue
			}
			chunk = append(chunk, blobProbe{
				sha: p.sha, bucketDate: p.date,
				shaHex: hex.EncodeToString(p.sha), bucket: blob.BucketFromTime(p.date),
			})
			single, err := blobReferenced(ctx, db.Pool(), p.kind, ids["alice"], p.sha, p.date)
			if err != nil {
				t.Fatalf("blobReferenced: %v", err)
			}
			want = append(want, single)
		}
		batched, err := blobsReferenced(ctx, db.Pool(), blob.KindRaw, ids["alice"], chunk)
		if err != nil {
			t.Fatalf("blobsReferenced: %v", err)
		}
		for i, c := range chunk {
			got := batched[c.shaHex+"|"+string(c.bucket)]
			if got != want[i] {
				t.Errorf("chunk element %d: batched=%v, single=%v", i, got, want[i])
			}
		}
	})
}

// TestBlobsReferencedEmptyChunk covers the boundary the flush path relies on.
func TestBlobsReferencedEmptyChunk(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := blobsReferenced(ctx, db.Pool(), blob.KindRaw, 1, nil)
	if err != nil {
		t.Fatalf("blobsReferenced(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries for an empty chunk", len(got))
	}
}

// TestReferencePredicateRejectsUnknownKind keeps the shared fragment total.
func TestReferencePredicateRejectsUnknownKind(t *testing.T) {
	if _, err := referencePredicate(blob.Kind("nonsense"), "$1", "$2", "$3"); err == nil {
		t.Error("referencePredicate accepted an unknown kind")
	}
	for _, k := range []blob.Kind{blob.KindRaw, blob.KindAttachment} {
		sql, err := referencePredicate(k, "$1", "$2", "$3")
		if err != nil {
			t.Errorf("referencePredicate(%s): %v", k, err)
		}
		if sql == "" {
			t.Errorf("referencePredicate(%s) returned empty SQL", k)
		}
	}
}

// TestGCMarkBatchedRoundTrips is the RO5X-018 scale check: mark a store with
// many referenced blobs and many orphans, and assert exactly the orphans
// became candidates while the referenced ones did not — with the batched
// implementation doing it in far fewer round trips than one per blob.
func TestGCMarkBatchedRoundTrips(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	root := t.TempDir()
	store := blob.NewStore(root)
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}

	var mailboxID, folderID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("mailbox: %v", err)
	}
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, 1) RETURNING id`, mailboxID,
	).Scan(&folderID); err != nil {
		t.Fatalf("folder: %v", err)
	}

	tenant, err := blob.ParseTenant("alice")
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	day := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(day)

	const referencedN, orphanN = 300, 300
	writeBlob := func(seed int) string {
		content := []byte(fmt.Sprintf("blob-%d", seed))
		w, err := store.NewWriter(blob.KindRaw, tenant, bucket)
		if err != nil {
			t.Fatalf("writer: %v", err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("write: %v", err)
		}
		shaHex, _, _, err := w.Close()
		if err != nil {
			t.Fatalf("close: %v", err)
		}
		return shaHex
	}

	referenced := map[string]bool{}
	for i := 0; i < referencedN; i++ {
		shaHex := writeBlob(i)
		referenced[shaHex] = true
		raw, _ := hex.DecodeString(shaHex)
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				subject, from_addr, to_addrs, cc_addrs, headers, text_body,
				bodystructure, flags
			) VALUES ($1,$2,$3,$4,10,$5,'s','a@x.invalid','{}','{}','{}','b','{}','{}')`,
			folderID, int64(i+1), raw, day, day,
		); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}
	orphans := map[string]bool{}
	for i := 0; i < orphanN; i++ {
		shaHex := writeBlob(1_000_000 + i)
		orphans[shaHex] = true
	}

	// Age every blob past the grace window.
	past := time.Now().Add(-48 * time.Hour)
	if err := store.Walk(blob.KindRaw, func(_ blob.Kind, _ blob.Tenant, _ blob.Bucket, _, path string, _ os.FileInfo) error {
		return os.Chtimes(path, past, past)
	}); err != nil {
		t.Fatalf("age blobs: %v", err)
	}

	if rc := gcMark(ctx, db, store, 24*time.Hour); rc != EX_OK {
		t.Fatalf("gcMark returned %d", rc)
	}

	rows, err := db.Pool().Query(ctx,
		`SELECT encode(sha256, 'hex') FROM gc_candidates WHERE tenant = 'alice'`)
	if err != nil {
		t.Fatalf("read candidates: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[h] = true
	}

	for sha := range orphans {
		if !got[sha] {
			t.Errorf("orphan %s… was not marked as a candidate", sha[:16])
		}
	}
	for sha := range referenced {
		if got[sha] {
			t.Errorf("referenced blob %s… was wrongly marked as a candidate", sha[:16])
		}
	}
	if len(got) != orphanN {
		t.Errorf("candidates = %d, want exactly %d", len(got), orphanN)
	}
}
