package recipients_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/recipients"
)

// Integration tests for the resolver. These exercise the deny → exact
// alias → wildcard-with-allowlist resolution order against a real PG.
// Skipped unless MAIL_DATABASE_TEST_PG is set.

func TestResolveDenyOverridesEverything(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	domainID := mustInsertDomain(t, ctx, db, "wild.invalid", true)
	mboxID := mustInsertMailbox(t, ctx, db, "operator")
	// Even an explicit alias mapping spam → operator must lose to a deny row.
	mustInsertAlias(t, ctx, db, domainID, "spam", mboxID)
	mustInsertACL(t, ctx, db, domainID, "spam", "deny")

	r := recipients.New(db.Pool())
	_, err := r.Resolve(ctx, "spam@wild.invalid")
	if !errors.Is(err, recipients.ErrDenied) {
		t.Fatalf("got err=%v, want ErrDenied (deny must win over an explicit alias)", err)
	}
}

func TestResolveExactAliasBeatsCatchall(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	domainID := mustInsertDomain(t, ctx, db, "wild.invalid", true)
	catchallBox := mustInsertMailbox(t, ctx, db, "catchall")
	exactBox := mustInsertMailbox(t, ctx, db, "alice")
	mustInsertAlias(t, ctx, db, domainID, "", catchallBox) // catchall
	mustInsertAlias(t, ctx, db, domainID, "alice", exactBox)

	r := recipients.New(db.Pool())
	got, err := r.Resolve(ctx, "alice@wild.invalid")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.MailboxID != exactBox {
		t.Errorf("MailboxID = %d, want %d (exact alias)", got.MailboxID, exactBox)
	}
	if got.IsCatchall {
		t.Error("IsCatchall = true, want false (matched via exact alias)")
	}
}

func TestResolveWildcardCatchallWithoutAllowlist(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	domainID := mustInsertDomain(t, ctx, db, "wild.invalid", true)
	catchallBox := mustInsertMailbox(t, ctx, db, "catchall")
	mustInsertAlias(t, ctx, db, domainID, "", catchallBox)

	r := recipients.New(db.Pool())
	got, err := r.Resolve(ctx, "anything-goes@wild.invalid")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.MailboxID != catchallBox {
		t.Errorf("MailboxID = %d, want catchall %d", got.MailboxID, catchallBox)
	}
	if !got.IsCatchall {
		t.Error("IsCatchall = false, want true")
	}
}

func TestResolveAllowlistGatesWildcardCatchall(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	domainID := mustInsertDomain(t, ctx, db, "wild.invalid", true)
	catchallBox := mustInsertMailbox(t, ctx, db, "catchall")
	mustInsertAlias(t, ctx, db, domainID, "", catchallBox)
	mustInsertACL(t, ctx, db, domainID, "allowed-name", "allow")

	r := recipients.New(db.Pool())

	// In the allowlist → routes via the catchall.
	got, err := r.Resolve(ctx, "allowed-name@wild.invalid")
	if err != nil {
		t.Fatalf("allowed Resolve: %v", err)
	}
	if got.MailboxID != catchallBox || !got.IsCatchall {
		t.Errorf("allowed: got mb=%d isCatchall=%v, want mb=%d isCatchall=true",
			got.MailboxID, got.IsCatchall, catchallBox)
	}

	// Not in the allowlist → rejected even though a catchall exists.
	_, err = r.Resolve(ctx, "random@wild.invalid")
	if !errors.Is(err, recipients.ErrNotAllowlisted) {
		t.Errorf("non-allowlisted: got err=%v, want ErrNotAllowlisted", err)
	}
}

func TestResolveDenyBeatsAllowOnSameLocalpart(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	domainID := mustInsertDomain(t, ctx, db, "wild.invalid", true)
	catchallBox := mustInsertMailbox(t, ctx, db, "catchall")
	mustInsertAlias(t, ctx, db, domainID, "", catchallBox)
	mustInsertACL(t, ctx, db, domainID, "ambivalent", "allow")
	mustInsertACL(t, ctx, db, domainID, "ambivalent", "deny")

	r := recipients.New(db.Pool())
	_, err := r.Resolve(ctx, "ambivalent@wild.invalid")
	if !errors.Is(err, recipients.ErrDenied) {
		t.Errorf("got err=%v, want ErrDenied (deny must win when both rows exist)", err)
	}
}

func TestResolveDenyOnStandardDomain(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Non-wildcard domain. Deny still wins.
	domainID := mustInsertDomain(t, ctx, db, "tight.invalid", false)
	mboxID := mustInsertMailbox(t, ctx, db, "alice")
	mustInsertAlias(t, ctx, db, domainID, "alice", mboxID)
	mustInsertACL(t, ctx, db, domainID, "alice", "deny")

	r := recipients.New(db.Pool())
	_, err := r.Resolve(ctx, "alice@tight.invalid")
	if !errors.Is(err, recipients.ErrDenied) {
		t.Errorf("got err=%v, want ErrDenied on a standard domain", err)
	}
}

func TestResolveUnknownDomain(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r := recipients.New(db.Pool())
	_, err := r.Resolve(ctx, "anyone@no-such.invalid")
	if !errors.Is(err, recipients.ErrUnknownDomain) {
		t.Errorf("got err=%v, want ErrUnknownDomain", err)
	}
}

func TestResolveNoMatchOnStandardDomain(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mustInsertDomain(t, ctx, db, "tight.invalid", false)

	r := recipients.New(db.Pool())
	_, err := r.Resolve(ctx, "nobody@tight.invalid")
	if !errors.Is(err, recipients.ErrNoMatch) {
		t.Errorf("got err=%v, want ErrNoMatch", err)
	}
}

// --- helpers ---------------------------------------------------------------

// poolHandle is the slice of *storage.DB that the helpers below call into.
// Inlined to avoid passing the whole storage.DB around just for one method.
type poolHandle interface {
	Pool() *pgxpool.Pool
}

func mustInsertDomain(t *testing.T, ctx context.Context, db poolHandle, name string, wildcard bool) int64 {
	t.Helper()
	var id int64
	err := db.Pool().QueryRow(ctx,
		`INSERT INTO domains (name, is_wildcard) VALUES ($1, $2) RETURNING id`,
		name, wildcard,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert domain %q: %v", name, err)
	}
	return id
}

func mustInsertMailbox(t *testing.T, ctx context.Context, db poolHandle, name string) int64 {
	t.Helper()
	var id int64
	// password_hash is NOT NULL; tests don't authenticate so any nonzero
	// string suffices.
	err := db.Pool().QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ($1, $2) RETURNING id`,
		name, "$argon2id$test-only-placeholder",
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert mailbox %q: %v", name, err)
	}
	return id
}

func mustInsertAlias(t *testing.T, ctx context.Context, db poolHandle, domainID int64, localpart string, mailboxID int64) {
	t.Helper()
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO aliases (domain_id, localpart, mailbox_id) VALUES ($1, $2, $3)`,
		domainID, localpart, mailboxID,
	); err != nil {
		t.Fatalf("insert alias (%d,%q,%d): %v", domainID, localpart, mailboxID, err)
	}
}

func mustInsertACL(t *testing.T, ctx context.Context, db poolHandle, domainID int64, localpart, kind string) {
	t.Helper()
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO domain_acl (domain_id, localpart, kind) VALUES ($1, $2, $3)`,
		domainID, localpart, kind,
	); err != nil {
		t.Fatalf("insert acl (%d,%q,%q): %v", domainID, localpart, kind, err)
	}
}
