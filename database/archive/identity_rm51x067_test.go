package archive_test

import (
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

func TestJournalFolderIdentityAfterRename(t *testing.T) {
	for _, action := range []string{"undo", "reclassify", "replacement", "legacy"} {
		t.Run(action, func(t *testing.T) {
			db, _ := pgtest.Open(t)
			ctx := testContext(t)
			f := newFixture(t, ctx, db)
			mb := f.mailbox("rename")
			root := f.folder(mb, "Archive", `\Archive`)
			f.categories(mb, storage.ArchiveCategory{Key: "travel", Folder: "Archive/Travel"})
			id := f.message(root, msgOpt{})
			f.classify(id, "travel", 0.9)
			f.age(id, time.Hour)
			if s, err := archive.SortOnce(ctx, db, archive.SortOptions{SettleDelay: time.Minute}); err != nil || s.Filed != 1 {
				t.Fatalf("sort=%+v: %v", s, err)
			}
			var batch string
			var fromID, toID int64
			if err := db.Pool().QueryRow(ctx, `SELECT batch,from_folder_id,to_folder_id FROM archive_moves WHERE message_id=$1`, id).Scan(&batch, &fromID, &toID); err != nil {
				t.Fatal(err)
			}
			if fromID != root || toID == root {
				t.Fatalf("journal ids %d -> %d", fromID, toID)
			}
			if action == "legacy" {
				if _, err := db.Pool().Exec(ctx, `UPDATE archive_moves SET from_folder_id=NULL,to_folder_id=NULL`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.Pool().Exec(ctx, `UPDATE folders SET name=replace(name,'Archive','Saved') WHERE mailbox_id=$1`, mb); err != nil {
					t.Fatal(err)
				}
			}
			if action == "replacement" {
				replacement := f.folder(mb, "Archive/Travel", "")
				if _, err := db.Pool().Exec(ctx, `UPDATE messages SET folder_id=$1 WHERE id=$2`, replacement, id); err != nil {
					t.Fatal(err)
				}
			}
			if action == "reclassify" {
				s, err := archive.Reclassify(ctx, db, mb, archive.ReclassifyOptions{Key: "travel", Refile: true, Batch: "refile"})
				if err != nil || s.Refiled != 1 {
					t.Fatalf("reclassify=%+v: %v", s, err)
				}
			} else {
				s, err := archive.Undo(ctx, db, mb, batch, false, 500)
				if err != nil {
					t.Fatal(err)
				}
				if action == "replacement" {
					if s.Restored != 0 || s.Moved != 1 {
						t.Fatalf("undo=%+v", s)
					}
					f.mustBeIn(id, "Archive/Travel")
					return
				}
				if s.Restored != 1 {
					t.Fatalf("undo=%+v", s)
				}
			}
			if action == "legacy" {
				f.mustBeIn(id, "Archive")
			} else {
				f.mustBeIn(id, "Saved")
				if n := f.scalar(`SELECT count(*) FROM folders WHERE mailbox_id=$1 AND name='Archive'`, mb); n != 0 {
					t.Fatalf("stray old root: %d", n)
				}
			}
		})
	}
}
