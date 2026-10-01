package recipients

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestCatchallAllowlistGateNonWildcard is the R-026 regression: a non-wildcard
// domain with a catchall alias AND an allowlist must gate the catchall by the
// allowlist — an allowed localpart resolves to the catchall, a non-allowed one
// is rejected ErrNotAllowlisted (previously it bypassed the gate entirely).
func TestCatchallAllowlistGateNonWildcard(t *testing.T) {
	db, _ := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var domainID, mailboxID int64
	// is_wildcard = FALSE, but a catchall alias is present.
	if err := pool.QueryRow(ctx,
		`INSERT INTO domains (name, is_wildcard) VALUES ('example.invalid', false) RETURNING id`,
	).Scan(&domainID); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('jdoe', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}
	// Catchall alias (localpart='').
	if _, err := pool.Exec(ctx,
		`INSERT INTO aliases (domain_id, localpart, mailbox_id) VALUES ($1, '', $2)`,
		domainID, mailboxID,
	); err != nil {
		t.Fatalf("insert catchall alias: %v", err)
	}
	// One allow row: only "sales" is opted in.
	if _, err := pool.Exec(ctx,
		`INSERT INTO domain_acl (domain_id, localpart, kind) VALUES ($1, 'sales', 'allow')`,
		domainID,
	); err != nil {
		t.Fatalf("insert allow: %v", err)
	}

	r := New(pool)

	// Allowed localpart resolves to the catchall.
	m, err := r.Resolve(ctx, "sales@example.invalid")
	if err != nil {
		t.Fatalf("resolve sales@: %v", err)
	}
	if !m.IsCatchall || m.MailboxName != "jdoe" {
		t.Fatalf("sales@ resolved to %+v, want catchall→jdoe", m)
	}

	// Non-allowed localpart is now gated (was an open catchall before R-026).
	_, err = r.Resolve(ctx, "randomstranger@example.invalid")
	if !errors.Is(err, ErrNotAllowlisted) {
		t.Fatalf("resolve randomstranger@ = %v, want ErrNotAllowlisted", err)
	}
}

// TestStandardDomainAllowRowsInert confirms allow rows on a domain with
// neither wildcard nor catchall stay inert (an exact alias still resolves; a
// non-aliased address is ErrNoMatch, not ErrNotAllowlisted).
func TestStandardDomainAllowRowsInert(t *testing.T) {
	db, _ := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var domainID, mailboxID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO domains (name, is_wildcard) VALUES ('std.example', false) RETURNING id`,
	).Scan(&domainID); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('bob', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO aliases (domain_id, localpart, mailbox_id) VALUES ($1, 'bob', $2)`,
		domainID, mailboxID,
	); err != nil {
		t.Fatalf("insert alias: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO domain_acl (domain_id, localpart, kind) VALUES ($1, 'sales', 'allow')`,
		domainID,
	); err != nil {
		t.Fatalf("insert allow: %v", err)
	}

	r := New(pool)
	if m, err := r.Resolve(ctx, "bob@std.example"); err != nil || m.MailboxName != "bob" {
		t.Fatalf("resolve bob@ = (%+v, %v), want bob", m, err)
	}
	// Allow row is inert here: no catchall, so an un-aliased address is ErrNoMatch.
	if _, err := r.Resolve(ctx, "nobody@std.example"); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("resolve nobody@ = %v, want ErrNoMatch (allow rows inert)", err)
	}
}
