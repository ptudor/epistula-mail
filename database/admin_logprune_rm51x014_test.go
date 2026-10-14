package main

import (
	"context"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"testing"
)

func TestLogPruneRetention(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx := context.Background()
	if _, err := db.Pool().Exec(ctx, `INSERT INTO delivery_log(envelope_to,bytes,outcome,received_at) SELECT 'old',1,'duplicate',now()-interval '100 days' FROM generate_series(1,2001); INSERT INTO delivery_log(envelope_to,bytes,outcome) VALUES ('new',1,'delivered')`); err != nil {
		t.Fatal(err)
	}
	config := writeAdminConfig(t, dsn)
	for _, mode := range []string{"-dry-run", "-yes"} {
		if code := adminLogPrune([]string{"-config", config, "-older-than", "2160h", mode}); code != EX_OK {
			t.Fatal(code)
		}
		var count int
		if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM delivery_log`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 2002
		if mode == "-yes" {
			want = 1
		}
		if count != want {
			t.Fatalf("%s count=%d want=%d", mode, count, want)
		}
	}
	var outcome string
	if err := db.Pool().QueryRow(ctx, `SELECT outcome FROM delivery_log WHERE envelope_to='new'`).Scan(&outcome); err != nil || outcome != "delivered" {
		t.Fatal(outcome, err)
	}
}
