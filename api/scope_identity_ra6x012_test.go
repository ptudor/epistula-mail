package main

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
)

// TestDeletedMailboxNameReuseDoesNotRestoreAccess is the RA6X-012 regression,
// reproduced end to end: mint a token scoped to a mailbox, delete that
// mailbox, create a new one with the same name, and try the old credential
// against the new account's data.
//
// Before the fix, api_tokens.scope_mailboxes was a TEXT[] of NAMES with no
// foreign key, and mailbox deletion left it untouched — so the old token
// authorized read and write against a completely different account's mail,
// indefinitely, without relying on a warm cache.
func TestDeletedMailboxNameReuseDoesNotRestoreAccess(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A content token and a write token, both scoped to `bob`.
	readToken := f.bobToken
	writeToken := f.mintToken(ctx, "bob-writer", []string{"bob"},
		[]string{auth.PermissionReadContent, auth.PermissionWriteAnnotation})

	// Both work against the original account.
	if resp := f.do(http.MethodGet, "/v1/mailboxes/bob/folders", readToken, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("baseline read = %d, want 200", resp.StatusCode)
	}

	// Retire `bob` the way an operator does, then hand the name to somebody
	// else. mailboxes.id is a BIGSERIAL, so the replacement is a different id.
	oldID := mailboxIDByName(t, ctx, f, "bob")
	deleteMailbox(t, ctx, f, "bob")
	newID := createMailboxWithMessage(t, ctx, f, "bob")
	if newID == oldID {
		t.Fatalf("fixture invalid: recreated mailbox reused id %d", newID)
	}

	// The old credentials must not reach the new account by ANY route.
	for _, probe := range []struct {
		name   string
		method string
		path   string
		token  string
		body   string
	}{
		{"folder listing", http.MethodGet, "/v1/mailboxes/bob/folders", readToken, ""},
		{"folder messages", http.MethodGet, "/v1/mailboxes/bob/folders/INBOX/messages", readToken, ""},
		{"search", http.MethodGet, "/v1/search?q=replacement&mailbox=bob", readToken, ""},
		{"mailbox list", http.MethodGet, "/v1/mailboxes", readToken, ""},
		{"export", http.MethodGet, "/v1/export?mailbox=bob", readToken, ""},
	} {
		t.Run(probe.name, func(t *testing.T) {
			resp := f.do(probe.method, probe.path, probe.token, nil)
			defer resp.Body.Close()
			assertNoReplacementContent(t, resp, probe.name)
		})
	}

	// The write path too: annotating the replacement account's message with a
	// token that was scoped to the deleted account must be refused.
	t.Run("annotation write", func(t *testing.T) {
		msgID := replacementMessageID(t, ctx, f, newID)
		body := `{"model":"ra6x012","tags":[],"category":null,"summary":"should not land"}`
		resp := f.do(http.MethodPut,
			"/v1/messages/"+strconv.FormatInt(msgID, 10)+"/annotation", writeToken, strings.NewReader(body))
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
			t.Fatalf("a token scoped to the deleted account annotated the replacement account's mail (%d)",
				resp.StatusCode)
		}
		var n int64
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM message_annotations WHERE message_id = $1`, msgID,
		).Scan(&n); err != nil {
			t.Fatalf("count annotations: %v", err)
		}
		if n != 0 {
			t.Fatalf("%d annotation(s) were written to the replacement account", n)
		}
	})
}

// TestDeletedMailboxNameReuseWithAWarmCache is the same attack against a
// verification cache that already holds the token. This is the variant a
// name-keyed scope could not defend against at all: the cached entry carried
// the name, so the check passed before any query ran.
func TestDeletedMailboxNameReuseWithAWarmCache(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Warm the cache: this request performs the Argon2 verification and
	// stores the resolved token.
	if resp := f.do(http.MethodGet, "/v1/mailboxes/bob/folders", f.bobToken, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("cache warm-up = %d, want 200", resp.StatusCode)
	}

	deleteMailbox(t, ctx, f, "bob")
	createMailboxWithMessage(t, ctx, f, "bob")

	// The cache is still warm and still holds the token's scope. It holds
	// durable IDs, which are permanently dead, so it cannot authorize the
	// replacement account.
	resp := f.do(http.MethodGet, "/v1/mailboxes/bob/folders", f.bobToken, nil)
	defer resp.Body.Close()
	assertNoReplacementContent(t, resp, "warm-cache folder listing")
}

// TestMailboxRenameKeepsTokenScope pins the other half of durable identity: a
// rename must not detach a token from the account it was granted, and the
// vacated name must not carry the grant to whoever takes it next.
func TestMailboxRenameKeepsTokenScope(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := f.pool.Exec(ctx, `UPDATE mailboxes SET name = 'bob2' WHERE name = 'bob'`); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// The token follows the account under its new name.
	resp := f.do(http.MethodGet, "/v1/mailboxes/bob2/folders", f.bobToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renamed account = %d, want 200 (the token is scoped to the account, not the name)", resp.StatusCode)
	}

	// A new account taking the vacated name inherits nothing.
	createMailboxWithMessage(t, ctx, f, "bob")
	resp2 := f.do(http.MethodGet, "/v1/mailboxes/bob/folders", f.bobToken, nil)
	defer resp2.Body.Close()
	assertNoReplacementContent(t, resp2, "vacated-name folder listing")
}

// TestUnaffectedScopesSurviveADelete pins that retiring one mailbox does not
// disturb a token's other grants.
func TestUnaffectedScopesSurviveADelete(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	both := f.mintToken(ctx, "both", []string{"alice", "bob"},
		[]string{auth.PermissionReadContent})

	deleteMailbox(t, ctx, f, "bob")

	resp := f.do(http.MethodGet, "/v1/mailboxes/alice/folders", both, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alice scope = %d, want 200; deleting bob must not disturb it", resp.StatusCode)
	}
}

// assertNoReplacementContent fails unless the response denies access or
// returns nothing belonging to the replacement account. 403 and 404 are both
// acceptable denials; a 200 is only acceptable if it contains no trace of the
// replacement account's data.
func assertNoReplacementContent(t *testing.T, resp *http.Response, what string) {
	t.Helper()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: unexpected status %d", what, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	body := string(raw)
	for _, marker := range []string{"replacement", "REPLACEMENT-FOLDER"} {
		if strings.Contains(body, marker) {
			t.Fatalf("%s: the old token reached the replacement account's data: %s", what, body)
		}
	}
}

// --- fixture helpers -------------------------------------------------------

func mailboxIDByName(t *testing.T, ctx context.Context, f *apiFixture, name string) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(ctx, `SELECT id FROM mailboxes WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("lookup mailbox %s: %v", name, err)
	}
	return id
}

// deleteMailbox retires an account the way `admin mailbox-delete` does,
// including the token-scope cleanup that runs in the same transaction.
func deleteMailbox(t *testing.T, ctx context.Context, f *apiFixture, name string) {
	t.Helper()
	id := mailboxIDByName(t, ctx, f, name)
	if _, err := f.pool.Exec(ctx, `DELETE FROM mailboxes WHERE id = $1`, id); err != nil {
		t.Fatalf("delete mailbox %s: %v", name, err)
	}
	// The same single statement `admin mailbox-delete` runs: dropping the dead
	// scope entry and revoking a now-scopeless token have to happen in ONE row
	// update, because api_tokens_scope_nonempty is a row CHECK that would
	// reject an emptied live scope before any follow-up revoke could run.
	if _, err := f.pool.Exec(ctx,
		`UPDATE api_tokens
		    SET scope_mailbox_ids = array_remove(scope_mailbox_ids, $1),
		        revoked_at = CASE
		          WHEN revoked_at IS NULL
		           AND NOT scope_all_mailboxes
		           AND cardinality(array_remove(scope_mailbox_ids, $1)) = 0
		          THEN now()
		          ELSE revoked_at
		        END
		  WHERE $1 = ANY(scope_mailbox_ids)`, id); err != nil {
		t.Fatalf("rescope tokens: %v", err)
	}
}

// createMailboxWithMessage creates an account under the given name holding one
// clearly-marked message, so any leak into a response is unmistakable.
func createMailboxWithMessage(t *testing.T, ctx context.Context, f *apiFixture, name string) int64 {
	t.Helper()
	var mailboxID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ($1, 'x') RETURNING id`, name,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("create mailbox %s: %v", name, err)
	}
	var folderID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', 1, 100) RETURNING id`, mailboxID,
	).Scan(&folderID); err != nil {
		t.Fatalf("create folder: %v", err)
	}
	raw := []byte("Subject: REPLACEMENT-FOLDER\r\n\r\nreplacement account private mail\r\n")
	sum := sha256.Sum256(raw)
	when := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO messages (
			folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
			subject, from_addr, to_addrs, cc_addrs, headers, text_body, bodystructure, flags
		) VALUES ($1, 1, $2, $3::timestamptz, $4, $5::timestamptz, 'REPLACEMENT-FOLDER',
		          'x@y.invalid', '{}', '{}', '{}',
		          'replacement account private mail', '{}', '{}')`,
		folderID, sum[:], when, int64(len(raw)), when,
	); err != nil {
		t.Fatalf("insert replacement message: %v", err)
	}
	return mailboxID
}

func replacementMessageID(t *testing.T, ctx context.Context, f *apiFixture, mailboxID int64) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(ctx, `
		SELECT m.id FROM messages m
		  JOIN folders fo ON fo.id = m.folder_id
		 WHERE fo.mailbox_id = $1 ORDER BY m.id LIMIT 1`, mailboxID,
	).Scan(&id); err != nil {
		t.Fatalf("find replacement message: %v", err)
	}
	return id
}
