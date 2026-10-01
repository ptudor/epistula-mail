package imapsess

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/ptudor/epistula-mail/database/blob"
)

func TestRemainingCopyRejectsCorruptionAtomically(t *testing.T) {
	for _, kind := range []blob.Kind{blob.KindRaw, blob.KindAttachment} {
		for _, damage := range []string{"missing", "truncated", "same-size"} {
			t.Run(string(kind)+"/"+damage, func(t *testing.T) {
				s := mutationFixture(t)
				seedForCopy(t, s, 1)
				ctx := context.Background()
				var sha string
				var date time.Time
				var size int64
				query := `SELECT encode(raw_sha256,'hex'),raw_blob_date,raw_size FROM messages WHERE folder_id=$1 AND uid=4`
				if kind == blob.KindAttachment {
					query = `SELECT encode(a.sha256,'hex'),a.blob_date,a.size_bytes FROM attachments a JOIN messages m ON m.id=a.message_id WHERE m.folder_id=$1 AND m.uid=4`
				}
				if err := s.be.Pool.QueryRow(ctx, query, s.selectedFolderID).Scan(&sha, &date, &size); err != nil {
					t.Fatal(err)
				}
				path, err := s.be.BlobStore.PathFor(kind, s.tenant, blob.BucketFromTime(date), sha)
				if err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "missing":
					err = os.Remove(path)
				case "truncated":
					err = os.Truncate(path, 1)
				case "same-size":
					err = os.WriteFile(path, make([]byte, size), 0640)
				}
				if err != nil {
					t.Fatal(err)
				}
				before := usedBytes(t, s)
				if _, err := s.Copy(imap.UIDSetNum(1, 4), "New/Child"); err == nil {
					t.Fatal("COPY accepted corrupt content")
				}
				if usedBytes(t, s) != before {
					t.Fatal("failed COPY changed quota")
				}
				var dest int
				if err := s.be.Pool.QueryRow(ctx, `SELECT count(*) FROM folders WHERE mailbox_id=$1 AND name LIKE 'New%'`, s.mailboxID).Scan(&dest); err != nil {
					t.Fatal(err)
				}
				if dest != 0 {
					t.Fatal("failed COPY created a partial hierarchy")
				}
				if err := s.Move(nil, imap.UIDSetNum(4), "New"); err == nil {
					t.Fatal("MOVE accepted corrupt content")
				}
				if !messageExists(t, s, s.selectedFolderID, 4) {
					t.Fatal("failed MOVE deleted its source")
				}
			})
		}
	}
}
