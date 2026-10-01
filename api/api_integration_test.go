package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// apiFixture is a fully wired epistula-api over a fresh migrated database and
// a real blob store, served via httptest. Tokens cover the three
// permission shapes the design names: a *-scoped classifier
// (read_content + write_annotation), an alice-scoped analytics token
// (read_metadata only), and a bob-scoped reader.
type apiFixture struct {
	t       *testing.T
	srv     *server
	ts      *httptest.Server
	pool    *pgxpool.Pool
	rawByID map[int64][]byte

	classifierToken   string // scope *, read_content + write_annotation
	metaToken         string // scope alice, read_metadata
	aliceContentToken string // scope alice, read_content
	bobToken          string // scope bob, read_content

	aliceMsgIDs []int64 // ids of alice INBOX messages, ascending uid
	bobMsgID    int64
}

// testArgon are deliberately cheap parameters: the cache means each token
// verifies once, but cheap params keep even that fast.
var testArgon = auth.Params{Memory: 8 * 1024, Iterations: 1, Parallel: 1, KeyLen: 32, SaltLen: 16}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	db, _ := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("blob init: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Postgres.DSN = "unused-in-tests"
	cfg.Limits.DefaultPageSize = 3
	cfg.Limits.MaxPageSize = 10
	cfg.Limits.MaxExportStreams = 1
	cfg.Limits.ExportPageSize = 2
	cfg.Limits.AuthFailLimit = 3
	cfg.Limits.AuthFailDelay = "0s"

	srv := &server{
		cfg:                  cfg,
		pool:                 pool,
		store:                store,
		exportSem:            make(chan struct{}, cfg.Limits.MaxExportStreams),
		annotationsAvailable: true,
	}
	srv.auth = newAuthenticator(srv)

	f := &apiFixture{t: t, srv: srv, pool: pool, rawByID: map[int64][]byte{}}

	// Mailboxes + folders.
	var aliceID, bobID int64
	for name, dst := range map[string]*int64{"alice": &aliceID, "bob": &bobID} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO mailboxes (name, password_hash) VALUES ($1, 'x') RETURNING id`, name,
		).Scan(dst); err != nil {
			t.Fatalf("insert mailbox %s: %v", name, err)
		}
	}
	folder := func(mailboxID int64, name string) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
			 VALUES ($1, $2, 1, 100) RETURNING id`, mailboxID, name,
		).Scan(&id); err != nil {
			t.Fatalf("insert folder %s: %v", name, err)
		}
		return id
	}
	aliceInbox := folder(aliceID, "INBOX")
	folder(aliceID, "Archive/2026")
	bobInbox := folder(bobID, "INBOX")

	// Seven alice messages (pagination fixture) + one bob message.
	for uid := int64(1); uid <= 7; uid++ {
		id := f.insertMessage(ctx, store, "alice", aliceInbox, uid, fmt.Sprintf("alice message %d about gophers", uid),
			time.Date(2026, 5, int(uid), 12, 0, 0, 0, time.UTC), uid%2 == 0)
		f.aliceMsgIDs = append(f.aliceMsgIDs, id)
	}
	f.bobMsgID = f.insertMessage(ctx, store, "bob", bobInbox, 1, "bob secret payroll message",
		time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC), false)

	f.classifierToken = f.mintToken(ctx, "classifier", []string{"*"},
		[]string{auth.PermissionReadContent, auth.PermissionWriteAnnotation})
	f.metaToken = f.mintToken(ctx, "analytics", []string{"alice"},
		[]string{auth.PermissionReadMetadata})
	f.aliceContentToken = f.mintToken(ctx, "alice-reader", []string{"alice"},
		[]string{auth.PermissionReadContent})
	f.bobToken = f.mintToken(ctx, "bob-reader", []string{"bob"},
		[]string{auth.PermissionReadContent})

	f.ts = httptest.NewServer(srv.routes())
	t.Cleanup(f.ts.Close)
	return f
}

// insertMessage writes the raw bytes to the blob store and the row to PG.
// seen toggles the \Seen flag for filter tests.
func (f *apiFixture) insertMessage(ctx context.Context, store *blob.Store, tenant blob.Tenant, folderID, uid int64, body string, when time.Time, seen bool) int64 {
	f.t.Helper()
	raw := []byte("Subject: msg " + fmt.Sprint(uid) + "\r\n\r\n" + body + "\r\n")
	sum := sha256.Sum256(raw)

	w, err := store.NewWriter(blob.KindRaw, tenant, blob.BucketFromTime(when))
	if err != nil {
		f.t.Fatalf("blob writer: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		f.t.Fatalf("blob write: %v", err)
	}
	if _, _, _, err := w.Close(); err != nil {
		f.t.Fatalf("blob close: %v", err)
	}

	flags := []string{}
	if seen {
		flags = append(flags, `\Seen`)
	}
	var id int64
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO messages (
			folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
			subject, from_addr, to_addrs, cc_addrs, headers, text_body, bodystructure, flags
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 'sender@x.invalid', '{}', '{}', '{}', $8, '{}', $9)
		RETURNING id`,
		folderID, uid, sum[:], when, int64(len(raw)), when,
		"msg "+fmt.Sprint(uid), body, flags,
	).Scan(&id); err != nil {
		f.t.Fatalf("insert message uid %d: %v", uid, err)
	}
	f.rawByID[id] = raw
	return id
}

// seedBulkMessages inserts n alice INBOX messages with a text body of the
// given size, so an export produces more bytes than a socket will buffer.
func (f *apiFixture) seedBulkMessages(ctx context.Context, n, bodyBytes int) {
	f.t.Helper()
	var folderID int64
	if err := f.pool.QueryRow(ctx,
		`SELECT f.id FROM folders f JOIN mailboxes mb ON mb.id = f.mailbox_id
		  WHERE mb.name = 'alice' AND f.name = 'INBOX'`).Scan(&folderID); err != nil {
		f.t.Fatalf("alice INBOX: %v", err)
	}
	var nextUID int64
	if err := f.pool.QueryRow(ctx,
		`SELECT COALESCE(max(uid), 0) + 1 FROM messages WHERE folder_id = $1`, folderID).Scan(&nextUID); err != nil {
		f.t.Fatalf("next uid: %v", err)
	}
	body := strings.Repeat("x", bodyBytes)
	for i := 0; i < n; i++ {
		uid := nextUID + int64(i)
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				subject, from_addr, to_addrs, cc_addrs, headers, text_body, bodystructure, flags
			) VALUES ($1, $2, sha256($3::text::bytea), DATE '2026-05-01', $4,
			          TIMESTAMPTZ '2026-05-01 00:00:00Z' + make_interval(secs => $5::double precision),
			          $6, 'sender@x.invalid', '{}', '{}', '{}', $7::text, '{}', '{}')`,
			folderID, uid, fmt.Sprintf("bulk-%d", uid), int64(bodyBytes),
			float64(uid), fmt.Sprintf("bulk message %d", uid), body,
		); err != nil {
			f.t.Fatalf("insert bulk message %d: %v", uid, err)
		}
	}
}

func (f *apiFixture) mintToken(ctx context.Context, name string, scope, perms []string) string {
	f.t.Helper()
	secret, err := auth.GenerateAPITokenSecret()
	if err != nil {
		f.t.Fatalf("secret: %v", err)
	}
	hash, err := auth.HashPassword(secret, testArgon)
	if err != nil {
		f.t.Fatalf("hash: %v", err)
	}
	// Scope is stored as durable mailbox IDs (RA6X-012); the fixture still
	// takes names so callers read naturally, and resolves them here the same
	// way `admin api-token-add` does. A name with no mailbox row is a fixture
	// bug, not a silent empty scope.
	allMailboxes := len(scope) == 1 && scope[0] == "*"
	scopeIDs := []int64{}
	if !allMailboxes {
		if err := f.pool.QueryRow(ctx,
			`SELECT COALESCE(array_agg(id ORDER BY id), '{}')
			   FROM mailboxes WHERE name = ANY($1::text[])`, scope,
		).Scan(&scopeIDs); err != nil {
			f.t.Fatalf("resolve scope: %v", err)
		}
		if len(scopeIDs) != len(scope) {
			f.t.Fatalf("scope %v resolved to %d mailbox id(s); create the mailboxes first", scope, len(scopeIDs))
		}
	}

	var id int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO api_tokens (name, token_hash, scope_all_mailboxes, scope_mailbox_ids, permissions)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		name, hash, allMailboxes, scopeIDs, perms,
	).Scan(&id); err != nil {
		f.t.Fatalf("insert token: %v", err)
	}
	return auth.FormatAPIToken(id, secret)
}

// get performs an authenticated request and returns the response.
func (f *apiFixture) do(method, path, token string, body io.Reader) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.ts.URL+path, body)
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := f.ts.Client().Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (f *apiFixture) getJSON(path, token string, want int, v any) {
	f.t.Helper()
	resp := f.do(http.MethodGet, path, token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("GET %s = %d, want %d (body: %s)", path, resp.StatusCode, want, b)
	}
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			f.t.Fatalf("decode %s: %v", path, err)
		}
	}
}

func wantProblem(t *testing.T, resp *http.Response, status int) problem {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != status {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, status, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Status != status || p.Title == "" || p.Type == "" {
		t.Errorf("problem shape incomplete: %+v", p)
	}
	return p
}

// ---- authentication ----

func TestAPIMissingTokenIs401Problem(t *testing.T) {
	f := newAPIFixture(t)
	resp := f.do(http.MethodGet, "/v1/mailboxes", "", nil)
	wantProblem(t, resp, http.StatusUnauthorized)
}

func TestAPIBadTokenThrottlesPerIP(t *testing.T) {
	f := newAPIFixture(t)
	for i := 0; i < f.srv.cfg.Limits.AuthFailLimit; i++ {
		resp := f.do(http.MethodGet, "/v1/mailboxes", "mapi_999_wrongsecret", nil)
		wantProblem(t, resp, http.StatusUnauthorized)
	}
	resp := f.do(http.MethodGet, "/v1/mailboxes", "mapi_999_wrongsecret", nil)
	wantProblem(t, resp, http.StatusTooManyRequests)
}

func TestAPIRevokedTokenRejected(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`UPDATE api_tokens SET revoked_at = now() WHERE name = 'bob-reader'`); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp := f.do(http.MethodGet, "/v1/mailboxes", f.bobToken, nil)
	wantProblem(t, resp, http.StatusUnauthorized)
}

// ---- scope + permissions ----

func TestAPIMailboxListHonorsScope(t *testing.T) {
	f := newAPIFixture(t)
	var out struct {
		Mailboxes []mailboxItem `json:"mailboxes"`
	}
	f.getJSON("/v1/mailboxes", f.metaToken, http.StatusOK, &out)
	if len(out.Mailboxes) != 1 || out.Mailboxes[0].Name != "alice" {
		t.Errorf("scoped token sees %+v, want exactly [alice]", out.Mailboxes)
	}
	out.Mailboxes = nil
	f.getJSON("/v1/mailboxes", f.classifierToken, http.StatusOK, &out)
	if len(out.Mailboxes) != 2 {
		t.Errorf("* token sees %d mailboxes, want 2", len(out.Mailboxes))
	}
}

func TestAPIOutOfScopeIs403(t *testing.T) {
	f := newAPIFixture(t)
	resp := f.do(http.MethodGet, "/v1/mailboxes/bob/folders", f.metaToken, nil)
	p := wantProblem(t, resp, http.StatusForbidden)
	if !strings.Contains(p.Detail, "bob") {
		t.Errorf("detail %q should name the mailbox", p.Detail)
	}
	// Out-of-scope single message is also 403 (authenticated, not authorized).
	resp = f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d/text", f.bobMsgID), f.metaToken, nil)
	wantProblem(t, resp, http.StatusForbidden)
}

func TestAPIPermissionGates(t *testing.T) {
	f := newAPIFixture(t)
	// read_metadata cannot read content...
	resp := f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d", f.aliceMsgIDs[0]), f.metaToken, nil)
	wantProblem(t, resp, http.StatusForbidden)
	// ...nor request the text projection in a list...
	resp = f.do(http.MethodGet, "/v1/mailboxes/alice/folders/INBOX/messages?fields=text", f.metaToken, nil)
	wantProblem(t, resp, http.StatusForbidden)
	// ...nor write annotations.
	body := strings.NewReader(`{"model":"m","tags":["t"]}`)
	resp = f.do(http.MethodPut, fmt.Sprintf("/v1/messages/%d/annotation", f.aliceMsgIDs[0]), f.metaToken, body)
	wantProblem(t, resp, http.StatusForbidden)
	// read_content subsumes read_metadata: classifier lists folders fine.
	f.getJSON("/v1/mailboxes/alice/folders", f.classifierToken, http.StatusOK, nil)
}

// ---- listing + pagination ----

type listResp struct {
	Messages   []messageItem `json:"messages"`
	NextCursor string        `json:"next_cursor"`
}

func TestAPIFolderMessagesPaginatesWithoutLossOrDup(t *testing.T) {
	f := newAPIFixture(t)
	seen := map[int64]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/v1/mailboxes/alice/folders/INBOX/messages"
		if cursor != "" {
			path += "?cursor=" + cursor
		}
		var page listResp
		f.getJSON(path, f.metaToken, http.StatusOK, &page)
		pages++
		for _, m := range page.Messages {
			if seen[m.UID] {
				t.Fatalf("uid %d served twice", m.UID)
			}
			seen[m.UID] = true
			if m.TextBody != nil {
				t.Error("text_body must be omitted without fields=text")
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 7 {
		t.Errorf("walked %d messages, want 7", len(seen))
	}
	if pages != 3 {
		t.Errorf("page size 3 over 7 rows should take 3 pages, took %d", pages)
	}
}

func TestAPIFolderMessagesFilters(t *testing.T) {
	f := newAPIFixture(t)
	var out listResp
	// \Seen was set on even uids.
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages?flag=%5CSeen&limit=10", f.metaToken, http.StatusOK, &out)
	if len(out.Messages) != 3 {
		t.Errorf("flag=\\Seen matched %d, want 3", len(out.Messages))
	}
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages?not_flag=%5CSeen&limit=10", f.metaToken, http.StatusOK, &out)
	if len(out.Messages) != 4 {
		t.Errorf("not_flag=\\Seen matched %d, want 4", len(out.Messages))
	}
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages?since=2026-05-05&limit=10", f.metaToken, http.StatusOK, &out)
	if len(out.Messages) != 3 {
		t.Errorf("since=2026-05-05 matched %d, want 3 (uids 5,6,7)", len(out.Messages))
	}
}

func TestAPIFieldsTextInlinesBody(t *testing.T) {
	f := newAPIFixture(t)
	var out listResp
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages?fields=text&limit=1", f.classifierToken, http.StatusOK, &out)
	if len(out.Messages) != 1 || out.Messages[0].TextBody == nil {
		t.Fatalf("fields=text should inline text_body, got %+v", out.Messages)
	}
	if !strings.Contains(*out.Messages[0].TextBody, "gophers") {
		t.Errorf("text_body = %q, want the seeded body", *out.Messages[0].TextBody)
	}
}

func TestAPIBadCursorIs422(t *testing.T) {
	f := newAPIFixture(t)
	resp := f.do(http.MethodGet, "/v1/mailboxes/alice/folders/INBOX/messages?cursor=garbage!", f.metaToken, nil)
	wantProblem(t, resp, http.StatusUnprocessableEntity)
}

func TestAPIUnknownEndpointIs404Problem(t *testing.T) {
	f := newAPIFixture(t)
	resp := f.do(http.MethodGet, "/v1/nope", f.metaToken, nil)
	wantProblem(t, resp, http.StatusNotFound)
}

// ---- single message ----

func TestAPIMessageDocumentAndRawRoundTrip(t *testing.T) {
	f := newAPIFixture(t)
	id := f.aliceMsgIDs[0]

	var doc messageDoc
	f.getJSON(fmt.Sprintf("/v1/messages/%d", id), f.classifierToken, http.StatusOK, &doc)
	if doc.Mailbox != "alice" || doc.Folder != "INBOX" || doc.TextBody == nil {
		t.Errorf("document incomplete: %+v", doc)
	}

	resp := f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d/raw", id), f.classifierToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "message/rfc822" {
		t.Errorf("raw Content-Type = %q", ct)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !bytes.Equal(got, f.rawByID[id]) {
		t.Errorf("raw bytes differ from the ingested blob")
	}

	textResp := f.do(http.MethodGet, fmt.Sprintf("/v1/messages/%d/text", id), f.classifierToken, nil)
	defer textResp.Body.Close()
	text, _ := io.ReadAll(textResp.Body)
	if !strings.Contains(string(text), "gophers") {
		t.Errorf("text = %q, want the seeded body", text)
	}
}

// ---- search ----

func TestAPISearchScopedFTS(t *testing.T) {
	f := newAPIFixture(t)
	var out listResp
	f.getJSON("/v1/search?q=gophers&limit=10", f.classifierToken, http.StatusOK, &out)
	if len(out.Messages) != 7 {
		t.Errorf("search matched %d, want 7", len(out.Messages))
	}
	// bob's payroll message is invisible to the alice-scoped token
	// (read_content, since /v1/search is a body-content oracle — R-014).
	f.getJSON("/v1/search?q=payroll&limit=10", f.aliceContentToken, http.StatusOK, &out)
	if len(out.Messages) != 0 {
		t.Errorf("scoped search leaked %d out-of-scope messages", len(out.Messages))
	}
	f.getJSON("/v1/search?q=payroll&limit=10", f.bobToken, http.StatusOK, &out)
	if len(out.Messages) != 1 {
		t.Errorf("bob token should find its payroll message, got %d", len(out.Messages))
	}
}

// TestAPISearchRequiresReadContent is the R-014 gate: a read_metadata-only
// token must not use FTS as a body-content oracle.
func TestAPISearchRequiresReadContent(t *testing.T) {
	f := newAPIFixture(t)
	resp := f.do(http.MethodGet, "/v1/search?q=payroll&limit=10", f.metaToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("search with read_metadata token = %d, want 403", resp.StatusCode)
	}
}

// TestAPIFieldsAnnotationRequiresReadContent is the R-012 gate: fields=annotation
// inlines body-derived summaries, so a read_metadata-only token is refused.
func TestAPIFieldsAnnotationRequiresReadContent(t *testing.T) {
	f := newAPIFixture(t)
	resp := f.do(http.MethodGet,
		"/v1/mailboxes/alice/folders/INBOX/messages?fields=annotation&limit=10",
		f.metaToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("fields=annotation with read_metadata token = %d, want 403", resp.StatusCode)
	}
}

// ---- export ----

func TestAPIExportStreamsAllRowsAsNDJSON(t *testing.T) {
	f := newAPIFixture(t)
	resp := f.do(http.MethodGet, "/v1/export?mailbox=bob", f.bobToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("export Content-Type = %q", ct)
	}
	var rows []messageItem
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var m messageItem
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", sc.Text(), err)
		}
		rows = append(rows, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(rows) != 1 || rows[0].TextBody == nil {
		t.Fatalf("export rows = %+v, want bob's 1 message with text inline", rows)
	}

	// The export walks in date-paged batches; verify the multi-batch path
	// (7 alice rows at export_page_size=2) returns everything exactly once.
	resp2 := f.do(http.MethodGet, "/v1/export?mailbox=alice", f.classifierToken, nil)
	defer resp2.Body.Close()
	seen := map[int64]bool{}
	sc2 := bufio.NewScanner(resp2.Body)
	for sc2.Scan() {
		var m messageItem
		if err := json.Unmarshal(sc2.Bytes(), &m); err != nil {
			t.Fatalf("bad NDJSON line: %v", err)
		}
		if seen[m.ID] {
			t.Fatalf("message %d exported twice", m.ID)
		}
		seen[m.ID] = true
	}
	if len(seen) != 7 {
		t.Errorf("export returned %d rows, want 7", len(seen))
	}
}

func TestAPIExportStreamCap(t *testing.T) {
	f := newAPIFixture(t)
	// Hold the only export slot, then expect 429.
	f.srv.exportSem <- struct{}{}
	defer func() { <-f.srv.exportSem }()
	resp := f.do(http.MethodGet, "/v1/export?mailbox=alice", f.classifierToken, nil)
	wantProblem(t, resp, http.StatusTooManyRequests)
}

// ---- annotations ----

func TestAPIAnnotationLifecycle(t *testing.T) {
	f := newAPIFixture(t)
	id := f.aliceMsgIDs[0]
	put := func(body string) *http.Response {
		return f.do(http.MethodPut, fmt.Sprintf("/v1/messages/%d/annotation", id),
			f.classifierToken, strings.NewReader(body))
	}

	resp := put(`{"model":"haiku-4.5","tags":["receipts","travel"],"category":"Archive/Receipts","summary":"a receipt","tokens_in":1200,"tokens_out":80}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("annotation PUT = %d, want 204", resp.StatusCode)
	}

	// Idempotent replace on (message_id, model).
	resp = put(`{"model":"haiku-4.5","tags":["receipts"],"summary":"replaced"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("annotation re-PUT = %d, want 204", resp.StatusCode)
	}
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM message_annotations WHERE message_id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("annotation rows = %d, want 1 after replace", count)
	}

	// fields=annotation inlines it; tag filter matches it.
	var out listResp
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages?fields=annotation&tag=receipts&limit=10",
		f.classifierToken, http.StatusOK, &out)
	if len(out.Messages) != 1 || len(out.Messages[0].Annotations) != 1 {
		t.Fatalf("tag filter + fields=annotation got %+v", out.Messages)
	}
	if out.Messages[0].Annotations[0].Summary == nil || *out.Messages[0].Annotations[0].Summary != "replaced" {
		t.Errorf("annotation summary not replaced: %+v", out.Messages[0].Annotations[0])
	}

	// Validation: empty model is 422; unknown JSON field is 400.
	resp = put(`{"model":"","tags":[]}`)
	wantProblem(t, resp, http.StatusUnprocessableEntity)
	resp = put(`{"model":"m","bogus":true}`)
	wantProblem(t, resp, http.StatusBadRequest)
}

func TestAPIAnnotation503WhenMigrationAbsent(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.annotationsAvailable = false
	resp := f.do(http.MethodPut, fmt.Sprintf("/v1/messages/%d/annotation", f.aliceMsgIDs[0]),
		f.classifierToken, strings.NewReader(`{"model":"m"}`))
	wantProblem(t, resp, http.StatusServiceUnavailable)
}

// TestAPIAnnotationModelPriorityAndFilters covers the multi-model story: a
// message carries one annotation per model, the registry ranks them
// (non-retired by priority, then retired; primary flagged on the winner only),
// and the not_annotated_by / annotated_by filters let each GPU box pull just
// its own undone work.
func TestAPIAnnotationModelPriorityAndFilters(t *testing.T) {
	f := newAPIFixture(t)
	f.srv.modelPriorityAvailable = true
	ctx := context.Background()
	id := f.aliceMsgIDs[0]

	// primary-model wins; fast is a lower alternative; old-model is retired
	// (kept as an alternative, never primary) despite a high priority number.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO annotation_models (model, priority) VALUES ('primary-model', 100), ('fast', 10)`); err != nil {
		t.Fatalf("register models: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO annotation_models (model, priority, retired_at) VALUES ('old-model', 50, now())`); err != nil {
		t.Fatalf("register retired model: %v", err)
	}
	// id carries all three models; aliceMsgIDs[1] carries only fast.
	for _, m := range []string{"primary-model", "fast", "old-model"} {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO message_annotations (message_id, model, tags, summary) VALUES ($1, $2, '{x}', $3)`,
			id, m, m+" summary"); err != nil {
			t.Fatalf("annotate %s: %v", m, err)
		}
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO message_annotations (message_id, model, tags, summary) VALUES ($1, 'fast', '{x}', 'fast on other')`,
		f.aliceMsgIDs[1]); err != nil {
		t.Fatalf("annotate other: %v", err)
	}

	// The single-message endpoint returns annotations ranked.
	var doc messageDoc
	f.getJSON(fmt.Sprintf("/v1/messages/%d", id), f.classifierToken, http.StatusOK, &doc)
	if len(doc.Annotations) != 3 {
		t.Fatalf("annotations = %d, want 3", len(doc.Annotations))
	}
	if got := doc.Annotations[0]; got.Model != "primary-model" || !got.Primary || got.Priority == nil || *got.Priority != 100 {
		t.Errorf("annotations[0] = %+v, want primary-model primary=true priority=100", got)
	}
	if got := doc.Annotations[1]; got.Model != "fast" || got.Primary || got.Priority == nil || *got.Priority != 10 {
		t.Errorf("annotations[1] = %+v, want fast primary=false priority=10", got)
	}
	if got := doc.Annotations[2]; got.Model != "old-model" || got.Primary {
		t.Errorf("annotations[2] = %+v, want retired old-model never primary", got)
	}

	// not_annotated_by=primary-model excludes id and includes the other six.
	var out listResp
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages?not_annotated_by=primary-model&limit=10",
		f.metaToken, http.StatusOK, &out)
	if len(out.Messages) != 6 {
		t.Fatalf("not_annotated_by matched %d, want 6", len(out.Messages))
	}
	for _, m := range out.Messages {
		if m.ID == id {
			t.Errorf("message %d annotated by primary-model must be excluded", id)
		}
	}

	// annotated_by=fast matches the two messages fast annotated.
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages?annotated_by=fast&limit=10",
		f.metaToken, http.StatusOK, &out)
	if len(out.Messages) != 2 {
		t.Errorf("annotated_by=fast matched %d, want 2", len(out.Messages))
	}
}
