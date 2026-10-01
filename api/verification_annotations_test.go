package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestVerificationExportKeepsAllModelsUnderByteBudget(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.cfg.Limits.MaxPageBytes = 1
	if _, err := f.srv.pool.Exec(context.Background(), `INSERT INTO message_annotations(message_id,model,tags,summary) SELECT id, 'verify-' || g, '{}', repeat('summary',100) FROM messages CROSS JOIN generate_series(1,3) g`); err != nil {
		t.Fatal(err)
	}
	resp := f.do(http.MethodGet, "/v1/export", f.classifierToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("export status %d", resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	count := 0
	for {
		var row messageItem
		if err := dec.Decode(&row); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(row.Annotations) != 3 {
			t.Fatalf("message %d lost sidecars: got %d, want 3", row.ID, len(row.Annotations))
		}
		count++
	}
	if count != 8 {
		t.Fatalf("lost documents: got %d, want 8", count)
	}
}

func TestVerificationAnnotationUpdateWaitsForMove(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var id int64
	if err := f.srv.pool.QueryRow(ctx, `SELECT min(id) FROM messages`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	put := annotationPut{Model: "verify-lock", Tags: []string{}}
	if err := f.srv.upsertAnnotation(ctx, id, put); err != nil {
		t.Fatal(err)
	}
	tx, err := f.srv.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT id FROM messages WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.srv.upsertAnnotation(ctx, id, put) }()
	// Observe the actual PostgreSQL lock wait, rather than guessing when the
	// goroutine has reached its query. Existing-row UPSERT used to skip this
	// parent lock because the FK value is unchanged.
	waitForLockedQuery(t, ctx, f.srv.pool, done, "%mail_lock_message_for_annotation%")
	if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("PUT after MOVE = %v, want message-gone", err)
	}
}
