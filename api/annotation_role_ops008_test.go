package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// documentedGrants returns the statements of the SQL block under
// "## Postgres role" in deploy/README.md, for role in database. The block is
// read from the file rather than copied here, so the test fails when the
// documented grants stop being enough. CREATE ROLE is left to the caller: the
// block's password is a placeholder.
func documentedGrants(t *testing.T, role, database string) []string {
	t.Helper()
	b, err := os.ReadFile("deploy/README.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "## Postgres role")
	if i < 0 {
		t.Fatal("deploy/README.md has no Postgres role section")
	}
	doc = doc[i:]
	start := strings.Index(doc, "```sql\n")
	if start < 0 {
		t.Fatal("the Postgres role section has no sql block")
	}
	doc = doc[start+len("```sql\n"):]
	end := strings.Index(doc, "```")
	if end < 0 {
		t.Fatal("unterminated sql block")
	}
	roleRe := regexp.MustCompile(`\bepistula_api\b`)
	var out []string
	for _, line := range strings.Split(doc[:end], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") || strings.HasPrefix(line, "CREATE ROLE") {
			continue
		}
		line = strings.Replace(line, "DATABASE epistula_database", "DATABASE "+pgx.Identifier{database}.Sanitize(), 1)
		out = append(out, roleRe.ReplaceAllString(line, role))
	}
	if len(out) == 0 {
		t.Fatal("the sql block holds no grants")
	}
	return out
}

// deployedRole is a login role holding exactly the documented epistula_api grants.
type deployedRole struct {
	name string
}

// newDeployedRole names a role and registers its removal after the test
// database is dropped. Call it BEFORE newAPIFixture: cleanups run
// last-registered first, and a role cannot be dropped while a database still
// holds its grants.
func newDeployedRole(t *testing.T) *deployedRole {
	t.Helper()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	r := &deployedRole{name: "ops008_api_" + hex.EncodeToString(suffix[:])}
	bootstrap := os.Getenv("MAIL_DATABASE_TEST_PG")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, bootstrap)
		if err != nil {
			t.Errorf("drop role %s: %v", r.name, err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{r.name}.Sanitize()); err != nil {
			t.Errorf("drop role %s: %v", r.name, err)
		}
	})
	return r
}

// connect creates the role in the fixture's database cluster, applies the
// documented grants, and returns a pool that logs in as it.
func (r *deployedRole) connect(t *testing.T, f *apiFixture) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, "CREATE ROLE "+pgx.Identifier{r.name}.Sanitize()+" LOGIN"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	var database string
	if err := f.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range documentedGrants(t, r.name, database) {
		if _, err := f.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("documented grant %q: %v", stmt, err)
		}
	}
	cfg := f.pool.Config().Copy()
	cfg.ConnConfig.User = r.name
	cfg.ConnConfig.Password = ""
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", r.name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestAnnotationWriteWithDocumentedGrants is the OPS-008 regression. The
// annotation write locked its message with SELECT ... FOR KEY SHARE, which
// needs UPDATE privilege on messages. A documented role with SELECT only
// would fail with "permission denied for table messages" and a 500, while
// tests running as the schema owner would pass.
// The server here runs as a role holding exactly the documented grants.
func TestAnnotationWriteWithDocumentedGrants(t *testing.T) {
	role := newDeployedRole(t)
	f := newAPIFixture(t)
	f.srv.pool = role.connect(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := f.aliceMsgIDs[0]
	path := "/v1/messages/" + strconv.FormatInt(id, 10) + "/annotation"
	for i, category := range []string{"other", "receipt"} {
		body := `{"model":"ops008","tags":["t"],"category":"` + category + `","summary":"s"}`
		resp := f.do(http.MethodPut, path, f.classifierToken, strings.NewReader(body))
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %d as the documented role = %d, want 204", i+1, resp.StatusCode)
		}
		var stored string
		if err := f.pool.QueryRow(ctx,
			`SELECT category FROM message_annotations WHERE message_id = $1 AND model = 'ops008'`, id,
		).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != category {
			t.Fatalf("PUT %d stored category %q, want %q", i+1, stored, category)
		}
	}

	// The role still cannot lock, let alone change, a message itself: the
	// function is the only lock it can take.
	_, err := f.srv.pool.Exec(ctx, `SELECT id FROM messages WHERE id = $1 FOR KEY SHARE`, id)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("direct FOR KEY SHARE as the documented role = %v, want permission denied", err)
	}
}

// TestPassQueueWithDocumentedGrants runs the annotation pass queue as a role
// holding exactly the documented grants, so a grant the queue needs and the
// README lacks fails here rather than as a deployed API error.
func TestPassQueueWithDocumentedGrants(t *testing.T) {
	role := newDeployedRole(t)
	f := newAPIFixture(t)
	f.srv.passQueueAvailable = true
	f.srv.classificationsAvailable = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := f.aliceMsgIDs[0]
	for _, stmt := range []string{
		`DELETE FROM annotation_pass_required`,
		`INSERT INTO annotation_pass_required (message_id) VALUES ($1)`,
		`INSERT INTO message_annotations (message_id, model, tags) VALUES ($1, 'm1', '{}')`,
	} {
		var err error
		if strings.Contains(stmt, "$1") {
			_, err = f.pool.Exec(ctx, stmt, id)
		} else {
			_, err = f.pool.Exec(ctx, stmt)
		}
		if err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	f.srv.pool = role.connect(t, f)

	resp := f.do(http.MethodGet, "/v1/export?pass_required=true", f.classifierToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pass_required export as the documented role = %d, want 200", resp.StatusCode)
	}
	resp = f.do(http.MethodPost, "/v1/pass-required/prune", f.classifierToken,
		strings.NewReader(`{"model":"m1","require_classification":true}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prune as the documented role = %d, want 200", resp.StatusCode)
	}
	var left int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM annotation_pass_required`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d marker(s) left after pruning a finished message as the documented role", left)
	}
}

// TestAnnotationLockSerializesWithMoveAsDocumentedRole repeats the MOVE race
// of TestVerificationAnnotationUpdateWaitsForMove as the documented role. The
// function's lock is a row lock, so it is held by the caller's transaction:
// the PUT waits for the MOVE and then finds its message gone.
func TestAnnotationLockSerializesWithMoveAsDocumentedRole(t *testing.T) {
	role := newDeployedRole(t)
	f := newAPIFixture(t)
	f.srv.pool = role.connect(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := f.aliceMsgIDs[0]
	put := annotationPut{Model: "ops008-lock", Tags: []string{}}
	if err := f.srv.upsertAnnotation(ctx, id, put); err != nil {
		t.Fatal(err)
	}
	move, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer move.Rollback(context.Background())
	if _, err := move.Exec(ctx, `SELECT id FROM messages WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.srv.upsertAnnotation(ctx, id, put) }()
	waitForLockedQuery(t, ctx, f.pool, done, "%mail_lock_message_for_annotation%")
	if _, err := move.Exec(ctx, `DELETE FROM messages WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := move.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("PUT after MOVE = %v, want message-gone", err)
	}
}

// waitForLockedQuery waits until a query matching pattern is waiting on a lock
// in PostgreSQL, failing if the call being watched returns first.
func waitForLockedQuery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, done <-chan error, pattern string) {
	t.Helper()
	for {
		select {
		case err := <-done:
			t.Fatalf("PUT returned before MOVE released its source: %v", err)
		default:
		}
		var blocked bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE $1)`,
			pattern,
		).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestClassificationWriteWithDocumentedGrants: the archive classifier's write
// works as a role holding exactly the documented grants, as the annotation
// write does — the same lock function, plus migration 020's two tables.
func TestClassificationWriteWithDocumentedGrants(t *testing.T) {
	role := newDeployedRole(t)
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO archive_categories (mailbox_id, key, folder)
		 SELECT id, 'travel', 'Archive/Travel' FROM mailboxes WHERE name = 'alice'`); err != nil {
		t.Fatal(err)
	}
	f.srv.pool = role.connect(t, f)
	f.srv.classificationsAvailable = true
	token := f.mintToken(ctx, "grants-sorter", []string{"*"}, []string{"write_classification"})

	id := f.aliceMsgIDs[0]
	path := "/v1/messages/" + strconv.FormatInt(id, 10) + "/classification"
	for i := 0; i < 2; i++ {
		resp := f.do(http.MethodPut, path, token, strings.NewReader(`{"category":"travel","confidence":0.9,"model":"grants"}`))
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %d as the documented role = %d, want 204", i+1, resp.StatusCode)
		}
	}
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM message_classifications WHERE message_id = $1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("stored %d classification(s) (%v), want 1", n, err)
	}
	var categories []archiveCategoryItem
	var out struct {
		Categories []archiveCategoryItem `json:"categories"`
	}
	f.getJSON("/v1/mailboxes/alice/archive-categories", token, http.StatusForbidden, nil)
	f.getJSON("/v1/mailboxes/alice/archive-categories", f.classifierToken, http.StatusOK, &out)
	categories = out.Categories
	if len(categories) != 1 {
		t.Fatalf("categories as the documented role = %+v", categories)
	}
}
