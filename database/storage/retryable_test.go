package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsRetryableSQLSTATE is the R-005 ground-truth table: transient Postgres
// SQLSTATE classes must defer (EX_TEMPFAIL), constraint/data errors must not.
func TestIsRetryableSQLSTATE(t *testing.T) {
	cases := []struct {
		code string
		want bool
		note string
	}{
		{"57014", true, "query_canceled (statement_timeout under lock contention)"},
		{"40P01", true, "deadlock_detected"},
		{"40001", true, "serialization_failure"},
		{"53300", true, "too_many_connections"},
		{"53100", true, "disk_full"},
		{"57P01", true, "admin_shutdown (routine PG restart)"},
		{"57P03", true, "cannot_connect_now (starting up)"},
		{"08006", true, "connection_failure"},
		{"08003", true, "connection_does_not_exist"},
		{"23505", false, "unique_violation must NOT retry"},
		{"23503", false, "foreign_key_violation must NOT retry"},
		{"22001", false, "string_data_right_truncation must NOT retry"},
		{"42601", false, "syntax_error must NOT retry"},
	}
	for _, c := range cases {
		err := error(&pgconn.PgError{Code: c.code, Message: c.note})
		// Also test through a wrap, since callers wrap with fmt.Errorf.
		wrapped := fmt.Errorf("ingest: %w", err)
		if got := IsRetryable(err); got != c.want {
			t.Errorf("IsRetryable(PgError %s: %s) = %v, want %v", c.code, c.note, got, c.want)
		}
		if got := IsRetryable(wrapped); got != c.want {
			t.Errorf("IsRetryable(wrapped PgError %s) = %v, want %v", c.code, got, c.want)
		}
	}
}

// TestIsRetryableContextAndNil covers the non-PgError paths.
func TestIsRetryableContextAndNil(t *testing.T) {
	if IsRetryable(nil) {
		t.Error("IsRetryable(nil) = true, want false")
	}
	if !IsRetryable(context.DeadlineExceeded) {
		t.Error("IsRetryable(context.DeadlineExceeded) = false, want true")
	}
	if !IsRetryable(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)) {
		t.Error("IsRetryable(wrapped DeadlineExceeded) = false, want true")
	}
	if !IsRetryable(errors.New("dial tcp 10.0.0.1:5432: connect: connection refused")) {
		t.Error("IsRetryable(connection refused substring) = false, want true")
	}
	if IsRetryable(errors.New("some unrelated application error")) {
		t.Error("IsRetryable(unrelated error) = true, want false")
	}
}
