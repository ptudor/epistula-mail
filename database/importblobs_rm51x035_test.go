package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestImportBlobsMissingTenantFailureIsBounded(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	root := t.TempDir()
	store := blob.NewStore(root)
	real := gcMustMailbox(t, ctx, db, "real")
	for _, tenant := range []blob.Tenant{"ghost", "real"} {
		n := 1000
		if tenant == "real" {
			n = 1
		}
		for i := range n {
			raw := []byte(fmt.Sprintf("From: a@example.invalid\r\nSubject: %s-%d\r\n\r\nrecovered\r\n", tenant, i))
			sum := sha256.Sum256(raw)
			sha := hex.EncodeToString(sum[:])
			path, err := store.PathFor(blob.KindRaw, tenant, "2020/01/01", sha)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0640); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := writeRenameConfig(t, dsn, root)
	checkpoint := filepath.Join(root, "checkpoint")
	args := []string{"-config", cfg, "-checkpoint-file", checkpoint}
	if code := runImportBlobs(args); code != EX_TEMPFAIL {
		t.Fatalf("initial code=%d", code)
	}
	entries, err := os.ReadDir(checkpoint + ".failures.d")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 2 {
		t.Fatalf("failure files=%d want <=2", len(entries))
	}
	var count int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM messages m JOIN folders f ON f.id=m.folder_id WHERE f.mailbox_id=$1`, real).Scan(&count); err != nil || count != 1 {
		t.Fatalf("real rows=%d err=%v", count, err)
	}
	ghost := gcMustMailbox(t, ctx, db, "ghost")
	if code := runImportBlobs(append(args, "-retry-failures")); code != EX_OK {
		t.Fatalf("retry code=%d", code)
	}
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM messages m JOIN folders f ON f.id=m.folder_id WHERE f.mailbox_id=$1`, ghost).Scan(&count); err != nil || count != 1000 {
		t.Fatalf("ghost rows=%d err=%v", count, err)
	}
	entries, err = os.ReadDir(checkpoint + ".failures.d")
	if err != nil || len(entries) != 1 {
		t.Fatalf("queue files=%d err=%v", len(entries), err)
	}
}
