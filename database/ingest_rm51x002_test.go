package main

import (
	"context"
	"testing"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestIngestYearZeroDateCommitsNull(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	id := mustMailbox(t, ctx, db, "yearzero")
	store, _ := ingestFixture(t, id, "yearzero")
	p := ingestParamsFor(t, store, id, "yearzero", "Date: Mon, 01 Jan 0000 00:00:00 +0000\r\n\r\nbody\r\n")
	if _, err := db.Ingest(ctx, p); err != nil {
		t.Fatal(err)
	}
	var nulls bool
	if err := db.Pool().QueryRow(ctx, `SELECT sent_date IS NULL AND sent_date_local IS NULL FROM messages`).Scan(&nulls); err != nil || !nulls {
		t.Fatalf("null dates=%v err=%v", nulls, err)
	}
}
