package imapsess

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/epistula-mail/database/archive"
	"github.com/ptudor/epistula-mail/database/storage"
)

func TestSorterHonoursConcurrentIMAPMoveAndExpunge(t *testing.T) {
	for _, expunge := range []bool{false, true} {
		name := "move"
		if expunge {
			name = "expunge"
		}
		t.Run(name, func(t *testing.T) {
			s := mutationFixture(t)
			defer s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := s.be.Pool.Exec(ctx, `UPDATE folders SET name='Archive',special_use='\Archive' WHERE id=$1`, s.selectedFolderID); err != nil {
				t.Fatal(err)
			}
			s.selectedFolderName = "Archive"
			if _, err := s.Select("Archive", nil); err != nil {
				t.Fatal(err)
			}
			db := storage.NewFromPool(s.be.Pool, storage.Config{StatementTimeout: 5 * time.Second})
			if _, err := db.ReplaceArchiveCategories(ctx, s.mailboxID, []storage.ArchiveCategory{{Key: "travel", Folder: "Archive/Travel"}}, false); err != nil {
				t.Fatal(err)
			}
			var id int64
			if err := s.be.Pool.QueryRow(ctx, `SELECT id FROM messages WHERE folder_id=$1 AND uid=1`, s.selectedFolderID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			if _, err := s.be.Pool.Exec(ctx, `INSERT INTO message_classifications(message_id,category,confidence,model) VALUES($1,'travel',1,'test')`, id); err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			gate := newPollGate(func(sql string) bool {
				return strings.Contains(sql, "FROM mailboxes") && strings.Contains(sql, "FOR UPDATE")
			}, func(ctx context.Context) context.Context {
				select {
				case <-release:
				case <-ctx.Done():
				}
				return ctx
			})
			cfg := s.be.Pool.Config()
			cfg.ConnConfig.Tracer = gate
			traced, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer traced.Close()
			gate.armed.Store(true)
			done := make(chan error, 1)
			go func() {
				stats, err := archive.SortOnce(ctx, storage.NewFromPool(traced, storage.Config{StatementTimeout: 5 * time.Second}), archive.SortOptions{BatchSize: 10})
				if err == nil && stats.Filed != 0 {
					err = fmt.Errorf("sorter filed %d stale messages", stats.Filed)
				}
				done <- err
			}()
			gate.waitHit(t)
			// The sorter has selected this message but has not acquired its write lock.
			if expunge {
				if err := s.Store(nil, imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}, Silent: true}, nil); err != nil {
					close(release)
					t.Fatal(err)
				}
				uids := imap.UIDSetNum(1)
				if err := s.Expunge(nil, &uids); err != nil {
					close(release)
					t.Fatal(err)
				}
			} else {
				if err := s.Create("Keep", nil); err != nil {
					close(release)
					t.Fatal(err)
				}
				if err := s.Move(nil, imap.UIDSetNum(1), "Keep"); err != nil {
					close(release)
					t.Fatal(err)
				}
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal("sort after IMAP mutation", err)
			}
			var n int
			if err := s.be.Pool.QueryRow(ctx, `SELECT count(*) FROM archive_moves WHERE message_id=$1`, id).Scan(&n); err != nil || n != 0 {
				t.Fatalf("stale filing journaled: %d %v", n, err)
			}
			if !expunge {
				var folder string
				if err := s.be.Pool.QueryRow(ctx, `SELECT f.name FROM messages m JOIN folders f ON f.id=m.folder_id WHERE m.raw_sha256=$1 AND f.mailbox_id=$2`, fixtureSHA(1), s.mailboxID).Scan(&folder); err != nil || folder != "Keep" {
					t.Fatalf("owner MOVE lost: %s %v", folder, err)
				}
			}
		})
	}
}
