package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestImportFollowsFolderMerge is the Maildir sync that migration 021 exists
// for. A legacy folder is imported, then merged with its parent into the
// \Sent folder and pruned; the legacy folder has gained a message by the time
// it is synced again. The sync must write into the merged folder, recognise
// the two messages it already holds, and add only the new one, instead of
// re-creating the pruned folders and storing all three there.
func TestImportFollowsFolderMerge(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mboxID := gcMustMailbox(t, ctx, db, "syncbox")
	cfg := writeAdminConfig(t, dsn)
	root := t.TempDir()
	writeMaildirMessage(t, root, "0001", dryRunMsg("one"))
	writeMaildirMessage(t, root, "0002", dryRunMsg("two"))
	importFolder := func(extra ...string) {
		t.Helper()
		args := append([]string{
			"-config", cfg, "-maildir", root, "-mailbox", "syncbox",
			"-folder", "Sent/2008to2015",
			"-checkpoint-file", filepath.Join(t.TempDir(), "sync.ckpt"),
		}, extra...)
		if code := runImport(args); code != EX_OK {
			t.Fatalf("import %v exit=%d, want EX_OK", extra, code)
		}
	}

	importFolder()
	if _, err := archive.MergeFolders(ctx, db, mboxID, archive.MergeOptions{
		From: "Sent", Subtree: true, To: "Sent Messages", DropDuplicates: true, Batch: "merge-sync",
	}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if _, err := db.PruneEmptyFolders(ctx, mboxID, false); err != nil {
		t.Fatalf("prune: %v", err)
	}

	writeMaildirMessage(t, root, "0003", dryRunMsg("three"))
	importFolder("-dry-run")
	if states := folderStates(t, ctx, db, mboxID); states["Sent Messages"].messages != 2 {
		t.Fatalf("dry-run sync changed Sent Messages: %+v", states)
	}
	importFolder()

	states := folderStates(t, ctx, db, mboxID)
	for _, gone := range []string{"Sent", "Sent/2008to2015"} {
		if _, ok := states[gone]; ok {
			t.Errorf("the sync re-created the merged folder %q", gone)
		}
	}
	if got := states["Sent Messages"].messages; got != 3 {
		t.Fatalf("Sent Messages holds %d message(s) after the sync; want the 2 merged plus 1 new (folders: %v)",
			got, folderStateNames(states))
	}
	var total int64
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
		 WHERE f.mailbox_id = $1`, mboxID,
	).Scan(&total); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if total != 3 {
		t.Fatalf("mailbox holds %d message(s); want 3, each once", total)
	}
}
