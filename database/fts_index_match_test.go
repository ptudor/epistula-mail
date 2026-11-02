package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestSearchPredicateUsesTheExpressionIndex pins what migration 027 relies
// on: a query written as mail_fts(m.subject, m.text_body) @@ tsquery is
// planned against idx_messages_search. The planner inlines the SQL function
// on both the index expression and the query before comparing them; if that
// ever stopped holding — a function body that is no longer inlinable, a
// reader that retypes the expression instead of calling the function — every
// search would silently become a sequential scan computing the vector per
// row. Sequential scans are disabled so a small fixture still shows which
// index the predicate CAN use.
func TestSearchPredicateUsesTheExpressionIndex(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var mailboxID, folderID int64
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('planbox', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext) VALUES ($1, 'INBOX', 1, 1) RETURNING id`,
		mailboxID,
	).Scan(&folderID); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		sha := make([]byte, 32)
		sha[0] = byte(i)
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO messages (folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
			                      subject, text_body, headers, bodystructure)
			VALUES ($1, $2, $3, now(), 10, now(), $4, 'payment due', '{}', '{}')`,
			folderID, i, sha, fmt.Sprintf("invoice %d", i),
		); err != nil {
			t.Fatal(err)
		}
	}

	tx, err := db.Pool().BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `
		EXPLAIN (COSTS OFF)
		SELECT m.id FROM messages m
		 WHERE mail_fts(m.subject, m.text_body) @@ plainto_tsquery('simple', $1)
		 ORDER BY m.internal_date DESC, m.id DESC LIMIT 100`, "invoice payment")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")
	if !strings.Contains(text, "idx_messages_search") {
		t.Fatalf("the search predicate does not use idx_messages_search:\n%s", text)
	}

	// And the predicate finds what the vector covers: subject (weight A) and
	// text_body (weight B) together.
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM messages m
		 WHERE m.folder_id = $1 AND mail_fts(m.subject, m.text_body) @@ plainto_tsquery('simple', 'invoice payment')`,
		folderID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 20 {
		t.Fatalf("matched %d of 20", n)
	}
}
