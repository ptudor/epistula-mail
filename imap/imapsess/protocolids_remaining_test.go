package imapsess

import (
	"context"
	"github.com/emersion/go-imap/v2"
	"math"
	"strconv"
	"testing"
)

func TestRemainingCopyUIDBounds(t *testing.T) {
	for _, next := range []int64{math.MaxUint32 - 1, math.MaxUint32, math.MaxUint32 + 1} {
		t.Run(strconv.FormatInt(next, 10), func(t *testing.T) {
			s := mutationFixture(t)
			ctx := context.Background()
			if err := s.Create("Archive", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := s.be.Pool.Exec(ctx, `UPDATE folders SET uidnext=$2 WHERE mailbox_id=$1 AND name='Archive'`, s.mailboxID, next); err != nil {
				t.Fatal(err)
			}
			before := usedBytes(t, s)
			got, err := s.Copy(imap.UIDSetNum(1), "Archive")
			if next == math.MaxUint32-1 {
				if err != nil {
					t.Fatal(err)
				}
				if !got.DestUIDs.Contains(imap.UID(next)) {
					t.Fatalf("COPYUID=%v", got)
				}
			} else {
				if err == nil {
					t.Fatalf("exhausted COPY succeeded: %+v", got)
				}
				if usedBytes(t, s) != before {
					t.Fatal("failed allocation changed quota")
				}
				var rows, after int64
				if err := s.be.Pool.QueryRow(ctx, `SELECT count(m.id),f.uidnext FROM folders f LEFT JOIN messages m ON m.folder_id=f.id WHERE f.mailbox_id=$1 AND f.name='Archive' GROUP BY f.uidnext`, s.mailboxID).Scan(&rows, &after); err != nil {
					t.Fatal(err)
				}
				if rows != 0 || after != next {
					t.Fatalf("failed allocation changed destination: rows=%d next=%d", rows, after)
				}
			}
		})
	}
}

func TestRemainingUIDValidityExhaustionRollsBackHierarchy(t *testing.T) {
	for _, rename := range []bool{false, true} {
		s := mutationFixture(t)
		ctx := context.Background()
		if err := s.Create("Old/Child", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.be.Pool.Exec(ctx, `SELECT setval('folder_uidvalidity_seq',4294967294,true)`); err != nil {
			t.Fatal(err)
		}
		var err error
		if rename {
			err = s.Rename("Old", "New", nil)
		} else {
			err = s.Create("New/Child", nil)
		}
		if err == nil {
			t.Fatal("partly exhausted hierarchy operation succeeded")
		}
		var old, newCount int
		if err := s.be.Pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE name LIKE 'Old%'),count(*) FILTER(WHERE name LIKE 'New%') FROM folders WHERE mailbox_id=$1`, s.mailboxID).Scan(&old, &newCount); err != nil {
			t.Fatal(err)
		}
		if old != 2 || newCount != 0 {
			t.Fatalf("partial hierarchy mutation: old=%d new=%d", old, newCount)
		}
	}
}

func TestRemainingInvalidStoredIDsNeverReachWire(t *testing.T) {
	s := mutationFixture(t)
	ctx := context.Background()
	if _, err := s.be.Pool.Exec(ctx, `UPDATE folders SET uidvalidity=4294967296 WHERE id=$1`, s.selectedFolderID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Select("INBOX", nil); err == nil {
		t.Fatal("SELECT accepted invalid UIDVALIDITY")
	}
	if _, err := s.Status("INBOX", &imap.StatusOptions{UIDValidity: true}); err == nil {
		t.Fatal("STATUS accepted invalid UIDVALIDITY")
	}
}
