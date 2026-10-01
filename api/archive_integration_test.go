package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
)

// archiveFixture adds archive sorting to the standard fixture: alice gets an
// \Archive folder and two active categories plus a retired one, and a
// classifier token that holds write_classification.
type archiveFixture struct {
	*apiFixture
	classifyToken string // scope *, read_content + write_classification
}

func newArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	f := newAPIFixture(t)
	f.srv.classificationsAvailable = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, stmt := range []string{
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext, special_use)
		 SELECT id, 'Archive', 2, 1, '\Archive' FROM mailboxes WHERE name = 'alice'`,
		`INSERT INTO archive_categories (mailbox_id, key, folder, description)
		 SELECT id, 'travel', 'Archive/Travel', 'Flights, hotels' FROM mailboxes WHERE name = 'alice'`,
		`INSERT INTO archive_categories (mailbox_id, key, folder, annual)
		 SELECT id, 'finance/banking/other', 'Archive/Finance/Banking/Other', true FROM mailboxes WHERE name = 'alice'`,
		`INSERT INTO archive_categories (mailbox_id, key, folder, retired_at)
		 SELECT id, 'old', 'Archive/Old', now() FROM mailboxes WHERE name = 'alice'`,
	} {
		if _, err := f.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	token := f.mintToken(ctx, "sorter", []string{"*"},
		[]string{auth.PermissionReadContent, auth.PermissionWriteClassification})
	return &archiveFixture{apiFixture: f, classifyToken: token}
}

func (f *archiveFixture) putClassification(id int64, token, body string) *http.Response {
	return f.do(http.MethodPut, fmt.Sprintf("/v1/messages/%d/classification", id), token, strings.NewReader(body))
}

func (f *archiveFixture) exportIDs(query, token string) []int64 {
	f.t.Helper()
	resp := f.do(http.MethodGet, "/v1/export?"+query, token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("export %s = %d (%s)", query, resp.StatusCode, b)
	}
	var ids []int64
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var m messageItem
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			f.t.Fatalf("bad NDJSON line: %v", err)
		}
		ids = append(ids, m.ID)
	}
	if err := sc.Err(); err != nil {
		f.t.Fatal(err)
	}
	return ids
}

func TestArchiveCategoriesEndpoint(t *testing.T) {
	f := newArchiveFixture(t)
	var got struct {
		Mailbox       string                `json:"mailbox"`
		ArchiveFolder string                `json:"archive_folder"`
		Categories    []archiveCategoryItem `json:"categories"`
	}
	// A metadata token may read the list: keys and folder names are the
	// operator's, not message content.
	f.getJSON("/v1/mailboxes/alice/archive-categories", f.metaToken, http.StatusOK, &got)
	if got.ArchiveFolder != "Archive" || len(got.Categories) != 2 ||
		got.Categories[0].Key != "finance/banking/other" || !got.Categories[0].Annual ||
		got.Categories[1].Description != "Flights, hotels" || got.Categories[1].Annual {
		t.Fatalf("categories = %+v", got)
	}
	// Out of scope.
	wantProblem(t, f.do(http.MethodGet, "/v1/mailboxes/alice/archive-categories", f.bobToken, nil), http.StatusForbidden)
	// A mailbox with nothing set up answers an empty list and no folder.
	var bob struct {
		ArchiveFolder *string               `json:"archive_folder"`
		Categories    []archiveCategoryItem `json:"categories"`
	}
	f.getJSON("/v1/mailboxes/bob/archive-categories", f.bobToken, http.StatusOK, &bob)
	if bob.ArchiveFolder != nil || len(bob.Categories) != 0 || bob.Categories == nil {
		t.Fatalf("bob = %+v; want an empty (non-null) list and no folder", bob)
	}
	f.srv.classificationsAvailable = false
	wantProblem(t, f.do(http.MethodGet, "/v1/mailboxes/alice/archive-categories", f.metaToken, nil), http.StatusServiceUnavailable)
}

func TestClassificationPut(t *testing.T) {
	f := newArchiveFixture(t)
	id := f.aliceMsgIDs[0]
	ok := `{"category":"travel","confidence":0.82,"model":"lmstudio:qwen"}`

	// write_annotation is not write_classification: the annotating token —
	// the kind the interactive epistula-mcp connector may hold — cannot file mail.
	wantProblem(t, f.putClassification(id, f.classifierToken, ok), http.StatusForbidden)
	// Out of scope.
	bobOnly := f.mintToken(context.Background(), "bob-sorter", []string{"bob"},
		[]string{auth.PermissionWriteClassification})
	wantProblem(t, f.putClassification(id, bobOnly, ok), http.StatusForbidden)

	for body, status := range map[string]int{
		`{"category":"old","confidence":0.9,"model":"m"}`:          http.StatusUnprocessableEntity, // retired
		`{"category":"shopping","confidence":0.9,"model":"m"}`:     http.StatusUnprocessableEntity, // never approved
		`{"category":"INBOX","confidence":0.9,"model":"m"}`:        http.StatusUnprocessableEntity, // not a key
		`{"category":"travel","confidence":1.5,"model":"m"}`:       http.StatusUnprocessableEntity,
		`{"category":"travel","model":"m"}`:                        http.StatusUnprocessableEntity, // no confidence
		`{"category":"travel","confidence":0.5,"model":" "}`:       http.StatusUnprocessableEntity,
		`{"category":"travel","confidence":0.5,"model":"m","x":1}`: http.StatusBadRequest,
		`{"category":"travel","confidence":0.5,"model":"m"}{}`:     http.StatusBadRequest,
	} {
		wantProblem(t, f.putClassification(id, f.classifyToken, body), status)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM message_classifications`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused writes stored %d classification(s) (%v)", n, err)
	}

	resp := f.putClassification(id, f.classifyToken, ok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT = %d, want 204", resp.StatusCode)
	}
	// The latest decision wins.
	resp = f.putClassification(id, f.classifyToken, `{"category":"finance/banking/other","confidence":0.4,"model":"other"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second PUT = %d", resp.StatusCode)
	}
	var cat, model string
	var conf float32
	if err := f.pool.QueryRow(context.Background(),
		`SELECT category, confidence, model FROM message_classifications WHERE message_id = $1`, id,
	).Scan(&cat, &conf, &model); err != nil {
		t.Fatal(err)
	}
	if cat != "finance/banking/other" || conf != 0.4 || model != "other" {
		t.Fatalf("stored %s %v %s", cat, conf, model)
	}

	// A category approved for alice is not approved for bob.
	wantProblem(t, f.putClassification(f.bobMsgID, f.classifyToken, ok), http.StatusUnprocessableEntity)
	wantProblem(t, f.putClassification(999999, f.classifyToken, ok), http.StatusNotFound)
	f.srv.classificationsAvailable = false
	wantProblem(t, f.putClassification(id, f.classifyToken, ok), http.StatusServiceUnavailable)
}

func TestNotClassifiedFilter(t *testing.T) {
	f := newArchiveFixture(t)
	ctx := context.Background()
	classify := func(id int64, cat string) {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO message_classifications (message_id, category, confidence, model) VALUES ($1, $2, 0.9, 'm')`,
			id, cat); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO archive_categories (mailbox_id, key, folder)
		 SELECT id, 'bobs-only', 'Archive/Bob' FROM mailboxes WHERE name = 'bob'`); err != nil {
		t.Fatal(err)
	}
	classify(f.aliceMsgIDs[0], "travel")
	classify(f.aliceMsgIDs[1], "old")       // retired: still undone work
	classify(f.aliceMsgIDs[2], "bobs-only") // active, but in another mailbox's list

	ids := f.exportIDs("mailbox=alice&not_classified=true", f.classifyToken)
	if len(ids) != 6 {
		t.Fatalf("not_classified export = %d rows, want 6 (7 less the one current classification)", len(ids))
	}
	for _, id := range ids {
		if id == f.aliceMsgIDs[0] {
			t.Fatal("a currently classified message was exported as undone")
		}
	}
	// A metadata token may ask the operational question on a folder listing.
	var page struct {
		Messages []messageItem `json:"messages"`
	}
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages?not_classified=true&limit=10", f.metaToken, http.StatusOK, &page)
	if len(page.Messages) != 6 {
		t.Fatalf("folder listing not_classified = %d rows, want 6", len(page.Messages))
	}
	wantProblem(t, f.do(http.MethodGet, "/v1/export?mailbox=alice&not_classified=maybe", f.classifyToken, nil),
		http.StatusUnprocessableEntity)
	if got := f.exportIDs("mailbox=alice&not_classified=false", f.classifyToken); len(got) != 7 {
		t.Fatalf("not_classified=false = %d rows, want all 7", len(got))
	}
}

func TestSampleFilterIsDeterministic(t *testing.T) {
	f := newArchiveFixture(t)
	f.seedBulkMessages(context.Background(), 400, 10)
	all := f.exportIDs("mailbox=alice", f.classifyToken)
	first := f.exportIDs("mailbox=alice&sample_ppm=250000", f.classifyToken)
	second := f.exportIDs("mailbox=alice&sample_ppm=250000", f.classifyToken)
	if len(first) == 0 || len(first) >= len(all) {
		t.Fatalf("a 25%% sample of %d returned %d", len(all), len(first))
	}
	// Roughly a quarter: 407 rows, allow a wide statistical margin.
	if len(first) < 60 || len(first) > 150 {
		t.Errorf("a 25%% sample of %d returned %d", len(all), len(first))
	}
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Fatal("the same sample_ppm returned a different sample")
	}
	if got := f.exportIDs("mailbox=alice&sample_ppm=1000000", f.classifyToken); len(got) != len(all) {
		t.Fatalf("sample_ppm=1000000 returned %d of %d", len(got), len(all))
	}
	for _, bad := range []string{"0", "1000001", "x"} {
		wantProblem(t, f.do(http.MethodGet, "/v1/export?mailbox=alice&sample_ppm="+bad, f.classifyToken, nil),
			http.StatusUnprocessableEntity)
	}
}

// TestNotClassifiedFilterPlansAsAntiJoin pins the filter's plan shape. Once a
// mailbox is almost fully classified, the worker's not_classified export has to
// look past nearly every message to find the few left. As a per-row SubPlan that
// cost 17 s on 267K messages, past the statement_timeout on every pass, so the
// last messages were never classified. Postgres pulls a NOT EXISTS up into an
// anti-join only when it is correlated through WHERE alone, which is a property
// of the SQL rather than of table statistics, so a small fixture shows it.
func TestNotClassifiedFilterPlansAsAntiJoin(t *testing.T) {
	f := newArchiveFixture(t)
	ctx := context.Background()
	var folderID int64
	if err := f.pool.QueryRow(ctx,
		`SELECT f.id FROM folders f JOIN mailboxes mb ON mb.id = f.mailbox_id
		  WHERE mb.name = 'alice' AND f.name = 'INBOX'`).Scan(&folderID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mailboxCol, from string
		args                   []any
	}{
		{"search and export", "f.mailbox_id",
			"messages m JOIN folders f ON f.id = m.folder_id", nil},
		{"folder listing", "(SELECT lf.mailbox_id FROM folders lf WHERE lf.id = $1)",
			"messages m", []any{folderID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var conds []string
			args := append([]any{}, tc.args...)
			if tc.args != nil {
				conds = append(conds, "m.folder_id = $1")
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/v1/export?not_classified=true", nil)
			if !f.srv.addArchiveFilters(w, r, tc.mailboxCol, &conds, &args) {
				t.Fatalf("addArchiveFilters refused: %d %s", w.Code, w.Body)
			}
			rows, err := f.pool.Query(ctx, "EXPLAIN SELECT m.id FROM "+tc.from+
				" WHERE "+strings.Join(conds, " AND ")+" ORDER BY m.internal_date DESC, m.id DESC LIMIT 500", args...)
			if err != nil {
				t.Fatal(err)
			}
			var plan []string
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, line)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if text := strings.Join(plan, "\n"); !strings.Contains(text, "Anti Join") {
				t.Fatalf("not_classified is not planned as an anti-join:\n%s", text)
			}
		})
	}
}
