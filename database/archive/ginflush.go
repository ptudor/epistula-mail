package archive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ptudor/epistula-mail/database/storage"
)

// ginFlusher merges the pending lists of the messages table's GIN indexes
// (the full-text vector and the headers) into the indexes proper, between the
// batches of a bulk move.
//
// A move rewrites a row in place, and a row that changes folder is a new
// tuple version with new entries in every index, GIN included. GIN buffers
// new entries in a pending list and merges it when the list outgrows
// gin_pending_list_limit, in whichever statement crosses that limit. Moving
// thousands of large messages fills the list quickly, and the statement that
// pays for the merge runs inside a batch's transaction, holding the mailbox
// lock that delivery also needs, until statement_timeout cancels it. Flushing between
// batches, outside any transaction, keeps each batch cheap and puts the merge
// where nothing waits on it.
//
// gin_clean_pending_list is restricted to the index owner. The admin CLI
// connects as the schema owner; any other role gets one warning and the
// batches run as before.
type ginFlusher struct {
	db       *storage.DB
	indexes  []string
	disabled bool
}

func newGINFlusher(ctx context.Context, db *storage.DB) (*ginFlusher, error) {
	f := &ginFlusher{db: db}
	rows, err := db.Pool().Query(ctx, `
		SELECT c.oid::regclass::text
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		  JOIN pg_am am ON am.oid = c.relam
		 WHERE i.indrelid = 'messages'::regclass AND am.amname = 'gin'
		 ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("list GIN indexes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan GIN index: %w", err)
		}
		f.indexes = append(f.indexes, name)
	}
	return f, rows.Err()
}

// flush merges every pending list. A missing privilege disables the flusher
// with a warning. Any other failure is returned: a bulk move should stop on a
// database error rather than carry on into the timeouts the flush prevents.
func (f *ginFlusher) flush(ctx context.Context) error {
	if f == nil || f.disabled {
		return nil
	}
	for _, idx := range f.indexes {
		if _, err := f.db.Pool().Exec(ctx, `SELECT gin_clean_pending_list($1::regclass)`, idx); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42501" {
				slog.Warn("cannot flush GIN pending lists (not the index owner); "+
					"a large batch may hit statement_timeout", "index", idx)
				f.disabled = true
				return nil
			}
			return fmt.Errorf("flush GIN pending list of %s: %w", idx, err)
		}
	}
	return nil
}
