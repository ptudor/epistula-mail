package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestDeliveryLogForeignKeyIndexes(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	id := gcMustMailbox(t, ctx, db, "audited")
	folder, err := db.LookupOrCreateFolder(ctx, id, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO messages(folder_id,uid,raw_sha256,raw_size,internal_date)
	 SELECT $1,n,decode(repeat('ab',32),'hex'),1,now() FROM generate_series(1,200) n`, folder); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `INSERT INTO delivery_log(envelope_to,matched_mailbox_id,message_id,bytes,outcome)
	 SELECT 'audited@example.invalid',$1,m.id,1,'delivered' FROM messages m CROSS JOIN generate_series(1,25) n`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `ANALYZE delivery_log`); err != nil {
		t.Fatal(err)
	}
	var messageID int64
	if err := db.Pool().QueryRow(ctx, `SELECT min(id) FROM messages`).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Pool().Query(ctx, `EXPLAIN (ANALYZE) UPDATE delivery_log SET message_id=NULL WHERE message_id=$1`, messageID)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !strings.Contains(plan.String(), "idx_delivery_log_message") {
		t.Fatalf("index plan: %v\n%s", err, plan.String())
	}
	t.Log(plan.String())
	cfg := writeTimedAdminConfig(t, dsn, t.TempDir(), "2s")
	start := time.Now()
	if code := adminMailboxDelete([]string{"-config", cfg, "-name", "audited", "-yes"}); code != EX_OK {
		t.Fatalf("delete: %d", code)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("delete took %s", elapsed)
	}
	var total, detached int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE message_id IS NULL AND matched_mailbox_id IS NULL) FROM delivery_log`).Scan(&total, &detached); err != nil {
		t.Fatal(err)
	}
	if total != 5000 || detached != 5000 {
		t.Fatalf("audit rows: total=%d, detached=%d", total, detached)
	}
}
