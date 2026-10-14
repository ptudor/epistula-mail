package main

import (
	"bytes"
	"context"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGCMarkSymlinkTenantAndStaleRepairs(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(store.Root(), "alice")); err != nil {
		t.Fatal(err)
	}
	_, path := writeTestBlob(t, store, "alice", "2003/06/15", []byte(gcTestMessage))
	backdateFile(t, path, 48*time.Hour)
	old := filepath.Join(filepath.Dir(path), ".repair-xyz")
	young := filepath.Join(filepath.Dir(path), ".repair-fresh")
	for _, p := range []string{old, young} {
		if err := os.WriteFile(p, []byte("orphan"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	backdateFile(t, old, 48*time.Hour)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	if code := gcMark(ctx, db, store, 24*time.Hour); code != EX_OK {
		t.Fatal(code, logs.String())
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale repair remains", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatal("young repair removed", err)
	}
	if !strings.Contains(logs.String(), "stale_repairs_removed=1") {
		t.Fatal(logs.String())
	}
	if n := candidateCount(t, ctx, db); n != 1 {
		t.Fatalf("symlink orphan candidates = %d", n)
	}
}
