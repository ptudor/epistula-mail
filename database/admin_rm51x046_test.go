package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/maintenance"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func writeTimedAdminConfig(t *testing.T, dsn, root, timeout string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	text := fmt.Sprintf("production=false\n[postgres]\ndsn=%q\nstatement_timeout=%q\n[storage]\nroot=%q\n", dsn, timeout, root)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMaintenanceVerificationPagesOutsideStatements(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	id := gcMustMailbox(t, ctx, db, "large")
	folder, err := db.LookupOrCreateFolder(ctx, id, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store := blob.NewStore(root)
	// Hard links keep this >5 GiB verification corpus at 1 MiB on disk.
	data := make([]byte, 1<<20)
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	var first string
	for i := 0; i < 5001; i++ {
		path, err := store.PathFor(blob.KindRaw, "large", blob.BucketFromTime(base.AddDate(0, 0, i)), sha)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = path
			err = os.WriteFile(path, data, 0600)
		} else {
			err = os.Link(first, path)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO messages(folder_id,uid,raw_sha256,raw_size,internal_date,raw_blob_date)
	 SELECT $1,n,$2,$3,now(),date '2000-01-01'+(n-1) FROM generate_series(1,5001) n`, folder, sum[:], len(data)); err != nil {
		t.Fatal(err)
	}
	cfg := writeTimedAdminConfig(t, dsn, root, "1s")
	mustQuiesce(t, cfg, "large")
	started := time.Now()
	if code := adminMailboxMaintenance([]string{"-config", cfg, "-name", "large", "-off"}); code != EX_OK {
		t.Fatalf("release: %d", code)
	}
	t.Logf("verified 5001 MiB in %s with a 1 s statement bound", time.Since(started))
	var active bool
	if err := db.Pool().QueryRow(ctx, `SELECT maintenance_at IS NOT NULL FROM mailboxes WHERE id=$1`, id).Scan(&active); err != nil || active {
		t.Fatalf("flag still active: %v, %v", active, err)
	}
	mustQuiesce(t, cfg, "large")
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if code := adminMailboxMaintenance([]string{"-config", cfg, "-name", "large", "-off"}); code != EX_TEMPFAIL {
		t.Fatalf("missing blob release: %d", code)
	}
	if err := db.Pool().QueryRow(ctx, `SELECT maintenance_at IS NOT NULL FROM mailboxes WHERE id=$1`, id).Scan(&active); err != nil || !active {
		t.Fatalf("flag lost: %v, %v", active, err)
	}
}

func TestMaintenanceReportsReadErrorsAndCorruption(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	id := gcMustMailbox(t, ctx, db, "verify")
	store, params := ingestFixture(t, id, "verify")
	if _, err := db.Ingest(ctx, params); err != nil {
		t.Fatal(err)
	}
	var sha string
	var date time.Time
	if err := db.Pool().QueryRow(ctx, `SELECT encode(raw_sha256,'hex'),raw_blob_date FROM messages`).Scan(&sha, &date); err != nil {
		t.Fatal(err)
	}
	path, err := store.PathFor(blob.KindRaw, "verify", blob.BucketFromTime(date), sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0600)
	_, _, err = verifyTenantBlobs(ctx, db, id, store.Root(), "verify", time.Second)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("read error: %v", err)
	}
	os.Chmod(path, 0600)
	if err := os.Truncate(path, 1); err != nil {
		t.Fatal(err)
	}
	missing, _, err := verifyTenantBlobs(ctx, db, id, store.Root(), "verify", time.Second)
	if err != nil || len(missing) != 1 || !strings.Contains(missing[0], "corrupt: wrong size") {
		t.Fatalf("corruption: %v, %v", missing, err)
	}
}

func TestMaintenanceNamesQuiescedMailbox(t *testing.T) {
	db, dsn := pgtest.Open(t)
	id := gcMustMailbox(t, context.Background(), db, "quiesced")
	root := t.TempDir()
	cfg := writeRenameConfig(t, dsn, root)
	mustQuiesce(t, cfg, "quiesced")
	_, err := maintenance.Acquire(context.Background(), root, db.Pool(), false, false)
	if !errors.Is(err, maintenance.ErrOffline) || !strings.Contains(err.Error(), "quiesced") {
		t.Fatalf("barrier error: %v", err)
	}
	if _, err := db.Pool().Exec(context.Background(), `UPDATE mailboxes SET disabled_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	saved := os.Stdout
	os.Stdout = output
	code := adminMailboxList([]string{"-config", cfg})
	os.Stdout = saved
	output.Seek(0, io.SeekStart)
	text, err := io.ReadAll(output)
	if err != nil || code != EX_OK || !strings.Contains(string(text), "maintenance") || strings.Contains(string(text), "disabled") {
		t.Fatalf("list: %d %q %v", code, text, err)
	}
}

func TestMailboxDeleteExceedsConfiguredStatementBudget(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	id := gcMustMailbox(t, ctx, db, "delete")
	folder, err := db.LookupOrCreateFolder(ctx, id, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO messages(folder_id,uid,raw_sha256,raw_size,internal_date)
	 SELECT $1,n,decode(repeat('ab',32),'hex'),1,now() FROM generate_series(1,2000) n`, folder); err != nil {
		t.Fatal(err)
	}
	cfg := writeTimedAdminConfig(t, dsn, t.TempDir(), "500ms")
	args := []string{"-config", cfg, "-name", "delete", "-yes"}
	if code := adminMailboxDelete(args); code != EX_OK {
		t.Fatalf("delete: %d", code)
	}
	var remaining int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("remaining: %d, %v", remaining, err)
	}
	if code := adminMailboxDelete(args); code != EX_USAGE {
		t.Fatalf("repeat delete: %d", code)
	}
}
