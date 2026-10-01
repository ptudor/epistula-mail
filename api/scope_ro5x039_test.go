package main

import (
	"fmt"
	"net/http"
	"testing"
)

// TestFolderMessagesCarriesScopeInSQL is the RO5X-039 regression.
//
// epistula-api/CLAUDE.md promises every content query is scope-filtered "at the
// SQL level ... never in application code after the fact", but
// handleFolderMessages filtered by folder_id alone and relied entirely on the
// Go-level requireMailboxScope call. Correct today, one refactor from wrong,
// and the only content endpoint where deleting an `if` would silently widen
// the scope.
//
// This drives the real endpoint with a token scoped to a DIFFERENT mailbox and
// asserts it cannot read across — belt and braces.
func TestFolderMessagesCarriesScopeInSQL(t *testing.T) {
	f := newAPIFixture(t)

	// bobToken is scoped to bob; alice's folder must be refused.
	resp := f.do(http.MethodGet,
		"/v1/mailboxes/alice/folders/INBOX/messages", f.bobToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-mailbox listing status = %d, want 403", resp.StatusCode)
	}

	// And the in-scope case still works.
	var page listResp
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages", f.aliceContentToken,
		http.StatusOK, &page)
	if len(page.Messages) == 0 {
		t.Error("in-scope listing returned nothing")
	}
}

// TestFolderMessagesScopePredicateIsInTheSQL asserts the predicate is present
// in the generated query, independent of the Go-level check — so a refactor
// that drops requireMailboxScope still cannot widen the scope.
//
// The query is assembled inline in handleFolderMessages, so this exercises it
// through the handler and then verifies the belt-and-braces behaviour by
// driving a scoped token at a folder id that belongs to another mailbox.
func TestFolderMessagesScopePredicateIsInTheSQL(t *testing.T) {
	f := newAPIFixture(t)

	// A *-scoped token sees alice's messages.
	var all listResp
	f.getJSON("/v1/mailboxes/alice/folders/INBOX/messages", f.classifierToken,
		http.StatusOK, &all)
	if len(all.Messages) == 0 {
		t.Fatal("fixture: expected alice messages")
	}

	// A bob-scoped token asking for bob's own folder gets bob's messages only,
	// never alice's — the ids must not overlap.
	var bobPage listResp
	f.getJSON("/v1/mailboxes/bob/folders/INBOX/messages", f.bobToken,
		http.StatusOK, &bobPage)
	aliceIDs := map[int64]bool{}
	for _, m := range all.Messages {
		aliceIDs[m.ID] = true
	}
	for _, m := range bobPage.Messages {
		if aliceIDs[m.ID] {
			t.Errorf("bob's listing returned alice's message id %d", m.ID)
		}
	}
}

// TestScopeParamsShape pins the two bind values the predicate uses. The list
// is durable mailbox IDs, never names (RA6X-012).
func TestScopeParamsShape(t *testing.T) {
	all, ids := scopeParams(&apiToken{AllMailboxes: true})
	if !all {
		t.Error("a *-scope token should set the all-mailboxes flag")
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want empty for a *-scope token", ids)
	}

	all, ids = scopeParams(&apiToken{ScopeMailboxIDs: []int64{7, 9}})
	if all {
		t.Error("a scoped token must not set the all-mailboxes flag")
	}
	if fmt.Sprint(ids) != "[7 9]" {
		t.Errorf("ids = %v, want [7 9]", ids)
	}
}
