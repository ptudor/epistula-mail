package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestRemainingContentIndexes(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	mb := gcMustMailbox(t, ctx, db, "indexed")
	folder, err := db.LookupOrCreateFolder(ctx, mb, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO messages(folder_id,uid,raw_sha256,raw_size,internal_date,flags)
 SELECT $1,n,decode(md5(n::text)||md5(n::text),'hex'),1,now(),CASE WHEN n=1 THEN ARRAY['\Deleted'] ELSE ARRAY[]::text[] END FROM generate_series(1,100000)n`, folder); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `ANALYZE messages`); err != nil {
		t.Fatal(err)
	}
	var id int64
	var sha []byte
	var date any
	if err := db.Pool().QueryRow(ctx, `SELECT id,raw_sha256,raw_blob_date FROM messages WHERE uid=1 AND folder_id=$1`, folder).Scan(&id, &sha, &date); err != nil {
		t.Fatal(err)
	}
	pred, err := referencePredicate(blob.KindRaw, "$1", "$2", "$3")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, query, index string
		args               []any
	}{
		{"GC", pred, "idx_messages_sha_date_folder", []any{sha, date, mb}},
		{"delivery", `SELECT raw_blob_date FROM messages WHERE folder_id=$1 AND raw_sha256=$2 ORDER BY id LIMIT 1`, "idx_messages_folder_sha", []any{folder, sha}},
		{"purge", `SELECT m.id,m.folder_id FROM messages m JOIN folders f ON f.id=m.folder_id
   WHERE f.mailbox_id=$1 AND (m.id=ANY($2) OR m.raw_sha256 IN (
    SELECT s.raw_sha256 FROM messages s JOIN folders sf ON sf.id=s.folder_id WHERE sf.mailbox_id=$1 AND s.id=ANY($2)))`, "messages_pkey", []any{mb, []int64{id}}},
		{"deleted", `SELECT id FROM messages WHERE folder_id=$1 AND '\Deleted'=ANY(flags)`, "idx_messages_deleted_folder", []any{folder}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, err := db.Pool().Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+c.query, c.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan strings.Builder
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				plan.WriteString(line + "\n")
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			t.Log(plan.String())
			if !strings.Contains(plan.String(), c.index) {
				t.Fatalf("missing %s", c.index)
			}
		})
	}
}
