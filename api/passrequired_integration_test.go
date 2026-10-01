package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
)

// passQueueFixture is the archive fixture with the annotation pass queue
// (epistula-database migration 022) enabled and emptied, so each test marks
// exactly the messages it means to.
func passQueueFixture(t *testing.T) *archiveFixture {
	t.Helper()
	f := newArchiveFixture(t)
	f.srv.passQueueAvailable = true
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM annotation_pass_required`); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *archiveFixture) mark(ids ...int64) {
	f.t.Helper()
	for _, id := range ids {
		if _, err := f.pool.Exec(context.Background(),
			`INSERT INTO annotation_pass_required (message_id) VALUES ($1)`, id); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *archiveFixture) annotateAs(model string, ids ...int64) {
	f.t.Helper()
	for _, id := range ids {
		if _, err := f.pool.Exec(context.Background(),
			`INSERT INTO message_annotations (message_id, model, tags) VALUES ($1, $2, '{}')`, id, model); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *archiveFixture) prune(token, body string, want int) passPruneResult {
	f.t.Helper()
	resp := f.do(http.MethodPost, "/v1/pass-required/prune", token, strings.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != want {
		f.t.Fatalf("prune %s = %d, want %d", body, resp.StatusCode, want)
	}
	var res passPruneResult
	if want == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			f.t.Fatal(err)
		}
	}
	return res
}

func sortedIDs(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sameIDs(got, want []int64) bool {
	g, w := sortedIDs(got), sortedIDs(want)
	if len(g) != len(w) {
		return false
	}
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// TestPassRequiredQueue covers the queue end to end: pass_required selects the
// marked messages (alone and beside not_annotated_by, in scope), and prune
// clears exactly the finished ones -- annotated by the caller's model and,
// when asked, classified into an active category, which a mailbox with no
// categories does not need -- leaving the rest queued for the next round.
func TestPassRequiredQueue(t *testing.T) {
	f := passQueueFixture(t)
	ctx := context.Background()
	a := f.aliceMsgIDs
	f.mark(a[0], a[1], a[2], f.bobMsgID)

	if ids := f.exportIDs("pass_required=true", f.classifierToken); !sameIDs(ids, []int64{a[0], a[1], a[2], f.bobMsgID}) {
		t.Fatalf("pass_required export = %v; want the four marked messages", ids)
	}
	if ids := f.exportIDs("mailbox=alice&pass_required=true", f.classifierToken); !sameIDs(ids, a[:3]) {
		t.Fatalf("pass_required in alice = %v; want %v", ids, a[:3])
	}
	// A metadata token may ask the operational question on a folder listing.
	var page struct {
		Messages []messageItem `json:"messages"`
	}
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages?pass_required=true&limit=10", f.metaToken, http.StatusOK, &page)
	if len(page.Messages) != 3 {
		t.Fatalf("folder listing pass_required = %d rows, want 3", len(page.Messages))
	}

	// a[0]: annotated and classified. a[1]: annotated, not classified.
	// a[2]: untouched. bob: annotated, and bob's mailbox has no categories.
	f.annotateAs("m1", a[0], a[1], f.bobMsgID)
	f.annotateAs("other-model", a[2])
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO message_classifications (message_id, category, confidence, model) VALUES ($1, 'travel', 0.9, 'm1')`,
		a[0]); err != nil {
		t.Fatal(err)
	}
	if ids := f.exportIDs("pass_required=true&not_annotated_by=m1", f.classifierToken); !sameIDs(ids, []int64{a[2]}) {
		t.Fatalf("pass_required + not_annotated_by = %v; want only %d", ids, a[2])
	}

	res := f.prune(f.classifierToken, `{"model":"m1","require_classification":true}`, http.StatusOK)
	if res.Pruned != 2 || res.Remaining != 2 {
		t.Fatalf("prune = %+v; want 2 cleared (a[0], bob) and 2 remaining", res)
	}
	if ids := f.exportIDs("pass_required=true", f.classifierToken); !sameIDs(ids, a[1:3]) {
		t.Fatalf("after prune the queue holds %v; want %v", ids, a[1:3])
	}
	// Without the classification requirement, annotation alone finishes a[1].
	res = f.prune(f.classifierToken, `{"model":"m1","mailbox":"alice"}`, http.StatusOK)
	if res.Pruned != 1 || res.Remaining != 1 {
		t.Fatalf("prune without classification = %+v; want 1 cleared, 1 remaining", res)
	}
	// Another model's annotation finishes nothing for m1.
	if res = f.prune(f.classifierToken, `{"model":"m1"}`, http.StatusOK); res.Pruned != 0 || res.Remaining != 1 {
		t.Fatalf("idle prune = %+v; want nothing cleared, 1 remaining", res)
	}
}

// TestPassRequiredPruneAuthorization pins who may clear what: write_annotation
// is required, a scoped token clears only its own mailboxes and is refused a
// mailbox outside them, and bad input or a missing migration is refused.
func TestPassRequiredPruneAuthorization(t *testing.T) {
	f := passQueueFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := f.aliceMsgIDs
	f.mark(a[0], f.bobMsgID)
	f.annotateAs("m1", a[0], f.bobMsgID)

	f.prune(f.metaToken, `{"model":"m1"}`, http.StatusForbidden)
	bobAnnotator := f.mintToken(ctx, "bob-annotator", []string{"bob"}, []string{auth.PermissionWriteAnnotation})
	f.prune(bobAnnotator, `{"model":"m1","mailbox":"alice"}`, http.StatusForbidden)
	if res := f.prune(bobAnnotator, `{"model":"m1"}`, http.StatusOK); res.Pruned != 1 || res.Remaining != 0 {
		t.Fatalf("bob-scoped prune = %+v; want bob's marker only", res)
	}
	if ids := f.exportIDs("pass_required=true", f.classifierToken); !sameIDs(ids, []int64{a[0]}) {
		t.Fatalf("a bob-scoped prune touched alice: queue = %v", ids)
	}

	f.prune(f.classifierToken, `{"model":""}`, http.StatusUnprocessableEntity)
	f.prune(f.classifierToken, `{"model":"m1","mailbox":"nobody"}`, http.StatusNotFound)
	wantProblem(t, f.do(http.MethodGet, "/v1/export?pass_required=maybe", f.classifierToken, nil),
		http.StatusUnprocessableEntity)

	f.srv.passQueueAvailable = false
	f.prune(f.classifierToken, `{"model":"m1"}`, http.StatusServiceUnavailable)
	wantProblem(t, f.do(http.MethodGet, "/v1/export?pass_required=true", f.classifierToken, nil),
		http.StatusServiceUnavailable)
}
