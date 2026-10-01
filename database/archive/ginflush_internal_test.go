package archive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestGINFlusherFindsMessageIndexes: the flusher targets exactly the GIN
// indexes on messages (the full-text vector and the headers) and can flush
// them as the schema owner.
func TestGINFlusherFindsMessageIndexes(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f, err := newGINFlusher(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.indexes, ","); got != "idx_messages_fts,idx_messages_headers" {
		t.Fatalf("GIN indexes = %q", got)
	}
	if err := f.flush(ctx); err != nil || f.disabled {
		t.Fatalf("flush as owner: err = %v, disabled = %v", err, f.disabled)
	}
}
