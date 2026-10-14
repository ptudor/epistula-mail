package archive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestDeletedTrackerBoundedIndexedCandidates(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	var mb int64
	if err := db.Pool().QueryRow(ctx, `INSERT INTO mailboxes(name,password_hash) VALUES('tracker','x') RETURNING id`).Scan(&mb); err != nil {
		t.Fatal(err)
	}
	inbox, err := db.LookupOrCreateFolder(ctx, mb, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	root, err := db.LookupOrCreateFolder(ctx, mb, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE folders SET special_use='\Archive' WHERE id=$1`, root); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO messages(folder_id,uid,raw_sha256,raw_size,internal_date,flags)
 SELECT $1,n,decode(md5(n::text)||md5(n::text),'hex'),1,now(),CASE WHEN n<=100 THEN ARRAY['\Deleted'] ELSE ARRAY[]::text[] END FROM generate_series(1,100000)n`, inbox); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `ANALYZE messages`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Pool().Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+deletedInboxCandidates, 40)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Log(plan.String())
	if !strings.Contains(plan.String(), "idx_messages_deleted_folder") {
		t.Fatal("tracker did not use deleted partial index")
	}
	tracker := NewDeletedTracker()
	if n, err := tracker.ArchiveOnce(ctx, db, DeletedOptions{BatchSize: 2, SettleDelay: time.Hour}); err != nil || n != 0 {
		t.Fatalf("initial pass n=%d err=%v", n, err)
	}
	if len(tracker.firstSeen) != 2*maxBatchesPerMailbox {
		t.Fatalf("candidate count=%d", len(tracker.firstSeen))
	}
}
