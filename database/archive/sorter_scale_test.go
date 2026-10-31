package archive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestSortCandidatesIndexOnlyScan guards the shape of the live sorter's
// candidate query against a large \Archive folder where nothing qualifies:
// the common state of a store the classifier has not caught up with. The
// folder scan must run index-only on idx_messages_folder_id_id (migration
// 026) rather than visiting the wide messages heap row by row, and a pass
// must still complete and file nothing.
func TestSortCandidatesIndexOnlyScan(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	var mb int64
	if err := db.Pool().QueryRow(ctx, `INSERT INTO mailboxes(name,password_hash) VALUES('sorterscale','x') RETURNING id`).Scan(&mb); err != nil {
		t.Fatal(err)
	}
	root, err := db.LookupOrCreateFolder(ctx, mb, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE folders SET special_use='\Archive' WHERE id=$1`, root); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO archive_categories(mailbox_id,key,folder) VALUES ($1,'bills','Archive/Bills')`, mb); err != nil {
		t.Fatal(err)
	}
	// 20k settled messages in \Archive: half unclassified, half classified
	// below the confidence floor, so the sorter has nothing to file.
	if _, err := db.Pool().Exec(ctx, `INSERT INTO messages(folder_id,uid,raw_sha256,raw_size,internal_date,created_at)
 SELECT $1,n,decode(md5(n::text)||md5(n::text),'hex'),1,now(),now()-interval '1 day' FROM generate_series(1,20000)n`, root); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO message_classifications(message_id,category,confidence,model)
 SELECT id,'bills',0.3,'test' FROM messages WHERE folder_id=$1 AND uid%2=0`, root); err != nil {
		t.Fatal(err)
	}
	// VACUUM sets the visibility map so an index-only scan needs no heap fetches.
	for _, stmt := range []string{`VACUUM ANALYZE messages`, `VACUUM ANALYZE message_classifications`, `ANALYZE archive_moves`} {
		if _, err := db.Pool().Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := db.Pool().Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sortCandidates,
		mb, root, 300.0, float32(0.6), []string{"bills"}, 500, int64(0), "Archive")
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
	if !strings.Contains(plan.String(), "Index Only Scan using idx_messages_folder_id_id") {
		t.Fatal("candidate scan did not run index-only on idx_messages_folder_id_id")
	}
	if strings.Contains(plan.String(), "Seq Scan on messages") {
		t.Fatal("candidate scan read the messages heap sequentially")
	}

	stats, err := SortOnce(ctx, db, SortOptions{SettleDelay: 5 * time.Minute, MinConfidence: 0.6, BatchSize: 500})
	if err != nil {
		t.Fatalf("SortOnce: %v", err)
	}
	if stats.Mailboxes != 1 || stats.Filed != 0 || stats.Skipped != 0 {
		t.Fatalf("stats=%+v, want one mailbox, nothing filed or skipped", stats)
	}
}
