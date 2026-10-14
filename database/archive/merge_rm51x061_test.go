package archive_test

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestMergeRechecksSurvivingCopy(t *testing.T) {
	for _, skippedMove := range []bool{false, true} {
		name := "removed destination"
		if skippedMove {
			name = "skipped planned move"
		}
		t.Run(name, func(t *testing.T) {
			db, _ := pgtest.Open(t)
			ctx := testContext(t)
			f := newFixture(t, ctx, db)
			mb := f.mailbox("mergerace")
			from := f.folder(mb, "Source", "")
			to := f.folder(mb, "To", "")
			var removed, survivor int64
			if skippedMove {
				removed = f.message(from, msgOpt{content: "dup"})
				child := f.folder(mb, "Source/Child", "")
				survivor = f.message(child, msgOpt{content: "dup"})
			} else {
				removed = f.message(to, msgOpt{content: "dup"})
				survivor = f.message(from, msgOpt{content: "dup"})
			}
			tx, err := db.Pool().Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(ctx, `SELECT id FROM mailboxes WHERE id=$1 FOR UPDATE`, mb); err != nil {
				t.Fatal(err)
			}
			type result struct {
				stats archive.MergeStats
				err   error
			}
			done := make(chan result, 1)
			go func() {
				stats, err := archive.MergeFolders(ctx, db, mb, archive.MergeOptions{From: "Source", To: "To", Subtree: true, DropDuplicates: true, Batch: "race"})
				done <- result{stats, err}
			}()
			for {
				select {
				case r := <-done:
					t.Fatalf("merge completed before lock release: %+v", r)
				default:
				}
				var waiting bool
				if err := db.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM mailboxes WHERE id = $1 FOR UPDATE%')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE id=$1`, removed); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			r := <-done
			if r.err != nil || r.stats.Dropped != 0 {
				t.Fatalf("merge=%+v err=%v", r.stats, r.err)
			}
			if skippedMove {
				f.mustBeIn(survivor, "To")
				if r.stats.Moved != 1 {
					t.Fatal(r.stats)
				}
			} else {
				f.mustBeIn(survivor, "Source")
				if r.stats.Skipped != 1 {
					t.Fatal(r.stats)
				}
			}
		})
	}
}
