package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestMergePrintsIrreversibleDuplicateCount(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	mb := mustMailbox(t, ctx, db, "mergeoutput")
	store, _ := ingestFixture(t, mb, "mergeoutput")
	for _, item := range [][2]string{{"To", "dup"}, {"Source", "dup"}, {"Source", "unique"}} {
		p := ingestParamsFor(t, store, mb, "mergeoutput", "Subject: "+item[1]+"\r\n\r\n"+item[1]+"\r\n")
		p.FolderName = item[0]
		if _, err := db.Ingest(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old }()
	code := adminFolderMerge([]string{"-config", writeAdminConfig(t, dsn), "-mailbox", "mergeoutput", "-from", "Source", "-to", "To", "-drop-duplicates", "-yes"})
	os.Stdout = old
	if code != EX_OK {
		t.Fatalf("merge=%d", code)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	want := "1 duplicate row(s) were destroyed and are NOT journaled; archive-undo restores only the 1 moved messages"
	if !strings.Contains(string(out), want) {
		t.Fatalf("stdout=%s", out)
	}
	var batch string
	if err := db.Pool().QueryRow(ctx, `SELECT batch FROM archive_moves LIMIT 1`).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	stats, err := archive.Undo(ctx, db, mb, batch, true, 500)
	if err != nil || stats.Restored != 1 {
		t.Fatalf("dry-run undo=%+v err=%v", stats, err)
	}
}
