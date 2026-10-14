package archive_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

func TestUndoneOrOwnerReversedFilingStaysInArchive(t *testing.T) {
	for _, method := range []string{"undo", "owner move"} {
		t.Run(method, func(t *testing.T) {
			db, _ := pgtest.Open(t)
			ctx := testContext(t)
			f := newFixture(t, ctx, db)
			mb := f.mailbox("sticky")
			root := f.folder(mb, "Archive", `\Archive`)
			f.categories(mb, storage.ArchiveCategory{Key: "travel", Folder: "Archive/Travel"})
			id := f.message(root, msgOpt{})
			f.classify(id, "travel", 0.9)
			f.age(id, time.Hour)
			opts := archive.SortOptions{SettleDelay: time.Minute, MinConfidence: 0.6}
			if stats, err := archive.SortOnce(ctx, db, opts); err != nil || stats.Filed != 1 {
				t.Fatalf("first sort=%+v err=%v", stats, err)
			}
			if method == "undo" {
				var batch string
				if err := db.Pool().QueryRow(ctx, `SELECT batch FROM archive_moves WHERE message_id=$1`, id).Scan(&batch); err != nil {
					t.Fatal(err)
				}
				if stats, err := archive.Undo(ctx, db, mb, batch, false, 500); err != nil || stats.Restored != 1 {
					t.Fatalf("undo=%+v err=%v", stats, err)
				}
			} else {
				folder, err := db.LookupFolder(ctx, mb, "Archive/Travel")
				if err != nil {
					t.Fatal(err)
				}
				if err := db.RunTx(ctx, func(tx pgx.Tx) error {
					_, err := storage.MoveMessages(ctx, tx, mb, []storage.MoveItem{{ID: id, FromFolderID: folder}}, "Archive")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			f.age(id, time.Hour)
			if stats, err := archive.SortOnce(ctx, db, opts); err != nil || stats.Filed != 0 {
				t.Fatalf("second sort=%+v err=%v", stats, err)
			}
			f.mustBeIn(id, "Archive")
			if n := f.scalar(`SELECT count(*) FROM archive_moves WHERE message_id=$1`, id); n != 1 {
				t.Fatalf("journal entries=%d", n)
			}
		})
	}
}
