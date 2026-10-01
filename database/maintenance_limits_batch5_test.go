package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestReparseLimitBoundsRowsScanned is the RA6X-033 regression.
//
// The SQL always received the full batch size and p.Limit was checked only
// after the inner loop had processed the WHOLE batch, so `reparse-bs -limit 1`
// scanned — and, live, rewrote — up to the default 500 rows. An operator's
// small repair therefore modified hundreds of records beyond the boundary the
// help text promises, in dry-run as well as live.
func TestReparseLimitBoundsRowsScanned(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	parser := gcTestParser()

	var mailboxID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('limitbox', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	// More rows than one batch, each with a stale bodystructure so every row
	// is a rewrite candidate.
	const total = 12
	for i := 0; i < total; i++ {
		raw := []byte("From: s@x.invalid\r\nSubject: msg " + string(rune('a'+i)) + "\r\n\r\nbody\r\n")
		msg, err := parser.Parse(raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		when := time.Now().UTC()
		w, err := store.NewWriter(blob.KindRaw, "limitbox", blob.BucketFromTime(when))
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if _, err := w.Write(raw); err != nil {
			t.Fatalf("Write: %v", err)
		}
		sha, _, _, err := w.Close()
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := db.Ingest(ctx, storage.IngestParams{
			MailboxID: mailboxID, MailboxName: "limitbox", FolderName: "INBOX",
			EnvelopeTo: "limitbox@x.invalid", RawSHA256Hex: sha,
			RawSize: int64(len(raw)), RawBlobDate: when, Message: msg,
		}); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	// Make every stored structure stale so each row would be rewritten.
	if _, err := db.Pool().Exec(ctx, `UPDATE messages SET bodystructure = '{}'::jsonb`); err != nil {
		t.Fatalf("stale structures: %v", err)
	}

	for _, tc := range []struct{ limit, batch int64 }{
		{1, 500}, // limit far below the batch
		{4, 5},   // limit below batch size
		{6, 5},   // limit above batch size, spanning batches
	} {
		t.Run("", func(t *testing.T) {
			// Reset staleness so each sub-case starts from the same state.
			if _, err := db.Pool().Exec(ctx, `UPDATE messages SET bodystructure = '{}'::jsonb`); err != nil {
				t.Fatalf("reset: %v", err)
			}
			for _, dry := range []bool{true, false} {
				st, err := reparseAll(ctx, db, store, gcTestParser(), reparseParams{
					BatchSize: int(tc.batch),
					Limit:     tc.limit,
					DryRun:    dry,
				})
				if err != nil {
					t.Fatalf("reparseAll: %v", err)
				}
				if int64(st.Scanned) > tc.limit {
					t.Errorf("dry=%v: scanned %d rows under -limit %d (batch %d)",
						dry, st.Scanned, tc.limit, tc.batch)
				}
				if int64(st.Rewritten) > tc.limit {
					t.Errorf("dry=%v: rewrote %d rows under -limit %d", dry, st.Rewritten, tc.limit)
				}
			}
		})
	}
}

// TestDryRunDoesNotTouchTheStorageRoot is the RA6X-053 regression.
//
// The dry-run flag gated message and checkpoint writes only; Store.Init ran
// regardless, so a dry-run pointed at a mistyped storage_root CREATED it, and
// one pointed at a real root in shared-group mode could change its
// permissions.
func TestDryRunDoesNotTouchTheStorageRoot(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-mounted")

	cfg := DefaultConfig()
	cfg.Storage.Root = missing

	if _, code := openBlobStoreFor(cfg, true); code == EX_OK {
		t.Fatal("a dry-run accepted a storage root that does not exist")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("a dry-run created the storage root")
	}

	// An existing root's mode must be left exactly as it was.
	existing := filepath.Join(dir, "root")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	before, err := os.Stat(existing)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	cfg.Storage.Root = existing
	cfg.Storage.GroupWritable = true
	if _, code := openBlobStoreFor(cfg, true); code != EX_OK {
		t.Fatalf("a dry-run refused an existing root: exit %d", code)
	}
	after, err := os.Stat(existing)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if before.Mode() != after.Mode() {
		t.Fatalf("a dry-run changed the root's mode from %v to %v", before.Mode(), after.Mode())
	}

	// A live run still initializes a fresh store.
	fresh := filepath.Join(dir, "live")
	cfg.Storage.Root = fresh
	cfg.Storage.GroupWritable = false
	if _, code := openBlobStoreFor(cfg, false); code != EX_OK {
		t.Fatalf("a live run failed to initialize a fresh store: exit %d", code)
	}
	if fi, err := os.Stat(fresh); err != nil || !fi.IsDir() {
		t.Fatalf("a live run did not create the store: %v", err)
	}
}
