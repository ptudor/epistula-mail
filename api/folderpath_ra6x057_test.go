package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// escapeFolderSegment is the encoding epistula-mcp applies to a folder name before
// it becomes a URL path segment (RA6X-057), reproduced here so this test drives
// the real route the same way the connector does. Percent-escaping both "/" and
// "." leaves nothing for path cleaning to act on.
func escapeFolderSegment(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), ".", "%2E")
}

// TestFolderRouteReadsTheRequestedFolder is the RA6X-057 regression on the API
// side.
//
// Folder names are database strings and IMAP accepts names like `.`, `A/../B`
// and `A//B`. Laid into a URL as real path separators, Go's ServeMux cleans the
// path and 301s to the cleaned form, so the handler resolves a DIFFERENT folder
// and answers with real mail from it. The route must instead resolve exactly
// the requested name, with no redirect on the way.
func TestFolderRouteReadsTheRequestedFolder(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// One folder per pathological name, each holding a message whose subject
	// identifies it — so "the right folder" is checked against content, not
	// only against the echoed name.
	names := []string{
		".",
		"..",
		"A",
		"B",
		"A/../B",
		"A//B",
		"/Leading",
		"Trailing/",
		"%2e",
		"Ünïcødé/受信箱",
		"Archive/2026", // already present in the fixture
		"a/./b",
		"...",
	}

	var aliceID int64
	if err := f.pool.QueryRow(ctx, `SELECT id FROM mailboxes WHERE name = 'alice'`).Scan(&aliceID); err != nil {
		t.Fatalf("alice id: %v", err)
	}
	folderIDs := map[string]int64{}
	for _, name := range names {
		var id int64
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
			VALUES ($1, $2, 1, 100)
			ON CONFLICT (mailbox_id, name) DO UPDATE SET name = EXCLUDED.name
			RETURNING id`, aliceID, name).Scan(&id); err != nil {
			t.Fatalf("insert folder %q: %v", name, err)
		}
		folderIDs[name] = id
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				subject, from_addr, to_addrs, cc_addrs, headers, text_body, bodystructure, flags
			) VALUES ($1, 1, sha256($2::text::bytea), DATE '2026-05-01', 10,
			          TIMESTAMPTZ '2026-05-01 00:00:00Z', $2::text, 'sender@x.invalid',
			          '{}', '{}', '{}', 'body', '{}', '{}')`,
			id, "marker for "+name); err != nil {
			t.Fatalf("insert message for %q: %v", name, err)
		}
	}

	// A client that refuses to follow redirects: a 3xx here IS the defect, and
	// following it would hide the substitution behind a 200.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			path := "/v1/mailboxes/alice/folders/" + escapeFolderSegment(name) + "/messages"
			req, err := http.NewRequest(http.MethodGet, f.ts.URL+path, nil)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+f.aliceContentToken)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				t.Fatalf("routing normalized %q and redirected to %q",
					name, resp.Header.Get("Location"))
			}
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("GET %s = %d (body: %s)", path, resp.StatusCode, b)
			}

			var out struct {
				Mailbox  string `json:"mailbox"`
				Folder   string `json:"folder"`
				Messages []struct {
					Subject string `json:"subject"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Folder != name {
				t.Errorf("folder = %q, want %q", out.Folder, name)
			}
			if out.Mailbox != "alice" {
				t.Errorf("mailbox = %q, want alice", out.Mailbox)
			}
			// Content identity: the row returned must be the one stored in the
			// folder that was asked for.
			want := "marker for " + name
			if len(out.Messages) != 1 || out.Messages[0].Subject != want {
				t.Errorf("messages = %+v, want exactly the row of folder %q", out.Messages, name)
			}
		})
	}

	// A distinct folder that shares a cleaned path with another must not be
	// reachable through that other one: `A/../B` and `B` are two folders and
	// two different sets of messages.
	if folderIDs["A/../B"] == folderIDs["B"] {
		t.Fatal("fixture collapsed two distinct folder names")
	}

	// Scope isolation is unaffected: bob's token cannot read alice's folders,
	// pathological name or not.
	for _, name := range []string{"A/../B", "Archive/2026", "."} {
		path := "/v1/mailboxes/alice/folders/" + escapeFolderSegment(name) + "/messages"
		resp := f.do(http.MethodGet, path, f.bobToken, nil)
		func() {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("bob GET %s = %d, want 403 (body: %s)", path, resp.StatusCode, b)
			}
		}()
	}
}

// TestFolderWildcardIsDecodedExactlyOnce pins the other half of the fix
// specification: the handler must use the value the router already unescaped
// and must not unescape it again. A second pass would turn a folder literally
// named `%2e` into `.`, selecting a different folder.
func TestFolderWildcardIsDecodedExactlyOnce(t *testing.T) {
	f := newAPIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var aliceID int64
	if err := f.pool.QueryRow(ctx, `SELECT id FROM mailboxes WHERE name = 'alice'`).Scan(&aliceID); err != nil {
		t.Fatalf("alice id: %v", err)
	}
	// Two folders that a second decode pass would conflate: the literal three
	// characters "%2e", and the single character ".".
	for _, name := range []string{"%2e", ".", "%252e"} {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext) VALUES ($1, $2, 1, 100)`,
			aliceID, name); err != nil {
			t.Fatalf("insert folder %q: %v", name, err)
		}
	}

	for _, name := range []string{"%2e", ".", "%252e"} {
		var out struct {
			Folder string `json:"folder"`
		}
		path := "/v1/mailboxes/alice/folders/" + escapeFolderSegment(name) + "/messages"
		f.getJSON(path, f.aliceContentToken, http.StatusOK, &out)
		if out.Folder != name {
			t.Errorf("GET %s resolved folder %q, want %q", path, out.Folder, name)
		}
	}
}
