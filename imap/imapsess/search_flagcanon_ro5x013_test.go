package imapsess

import (
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// searchUIDs runs a UID SEARCH and returns the matched UIDs.
func searchUIDs(t *testing.T, sess *Session, c *imap.SearchCriteria) []imap.UID {
	t.Helper()
	data, err := sess.Search(imapserver.NumKindUID, c, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if data.All == nil {
		return nil
	}
	set, ok := data.All.(imap.UIDSet)
	if !ok {
		t.Fatalf("All = %T, want imap.UIDSet", data.All)
	}
	nums, _ := set.Nums()
	return nums
}

func containsUID(uids []imap.UID, want imap.UID) bool {
	for _, u := range uids {
		if u == want {
			return true
		}
	}
	return false
}

// TestStoreThenSearchAgreeOnFlagCase is the RO5X-013 end-to-end contract: a
// client that STOREs a non-canonical system flag must find the message again
// with a canonical SEARCH, and vice versa. The write seam canonicalizes
// (R-063) and the read seam canonicalizes the criterion (RO5X-013), so both
// directions agree.
func TestStoreThenSearchAgreeOnFlagCase(t *testing.T) {
	sess := mutationFixture(t)

	// A client sends the flag in a non-canonical case.
	if err := sess.Store(nil, imap.UIDSetNum(1),
		&imap.StoreFlags{Op: imap.StoreFlagsSet, Flags: []imap.Flag{imap.Flag(`\SEEN`)}},
		nil); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Every spelling of the criterion finds it.
	for _, spelling := range []string{`\Seen`, `\SEEN`, `\seen`} {
		got := searchUIDs(t, sess, &imap.SearchCriteria{Flag: []imap.Flag{imap.Flag(spelling)}})
		if !containsUID(got, 1) {
			t.Errorf("SEARCH %s did not find the message STOREd as \\SEEN (uids=%v)", spelling, got)
		}
	}

	// And the stored value really is canonical, so epistula-api's byte-exact
	// filter agrees too.
	if got := messageFlags(t, sess, 1); len(got) != 1 || got[0] != `\Seen` {
		t.Errorf("stored flags = %v, want [\\Seen]", got)
	}
}

// TestSearchCriterionCanonicalizationResidual documents what the chosen design
// does NOT cover, so the next reader does not re-derive it.
//
// Canonicalizing the criterion fixes "client sent an odd case". It cannot fix
// data already stored in an odd case — for that, migration 011 rewrites the
// rows, and every writer in the stack canonicalizes going forward. A row
// written directly by some future non-canonicalizing writer (a psql fix-up, an
// external administration tool) after the migration would still be missed.
//
// The alternative — comparing with lower(f) at query time — was deliberately
// rejected: it defeats any future index on flags and leaves the underlying
// data wrong.
func TestSearchCriterionCanonicalizationResidual(t *testing.T) {
	sess := mutationFixture(t)

	// Simulate a writer that bypasses the canonicalizing seam entirely.
	setFlags(t, sess, 1, []string{`\SEEN`})

	got := searchUIDs(t, sess, &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}})
	if containsUID(got, 1) {
		t.Log("note: a non-canonical stored row matched; the design does not " +
			"promise this, so this is a bonus rather than a regression")
	} else {
		t.Log("expected: a row written outside the canonicalizing seam and after " +
			"migration 011 is not matched — migration 011 is what fixes stored data")
	}
	// The assertion that matters: once the data is canonical, it matches.
	setFlags(t, sess, 1, []string{`\Seen`})
	if got := searchUIDs(t, sess, &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}); !containsUID(got, 1) {
		t.Errorf("canonical stored row was not matched by SEARCH SEEN (uids=%v)", got)
	}
}

// TestSearchKeywordFlagIsCanonicalizedFromAnyCase covers the criterion side:
// a client sending `KEYWORD \SEEN` must match a canonically-stored row.
func TestSearchKeywordFlagIsCanonicalizedFromAnyCase(t *testing.T) {
	sess := mutationFixture(t)
	setFlags(t, sess, 1, []string{`\Seen`})

	for _, spelling := range []string{`\SEEN`, `\seen`, `\SeEn`, `\Seen`} {
		got := searchUIDs(t, sess, &imap.SearchCriteria{Flag: []imap.Flag{imap.Flag(spelling)}})
		if !containsUID(got, 1) {
			t.Errorf("SEARCH KEYWORD %s did not match a row stored as \\Seen (uids=%v)", spelling, got)
		}
	}
}

// TestSearchNotFlagCanonicalizesCriterion covers the negated form: a client
// sending a non-canonical criterion to UNSEEN must not get the seen message
// back.
func TestSearchNotFlagCanonicalizesCriterion(t *testing.T) {
	sess := mutationFixture(t)
	setFlags(t, sess, 1, []string{`\Seen`})
	setFlags(t, sess, 2, []string{`\Seen`})
	setFlags(t, sess, 3, []string{})

	for _, spelling := range []string{`\Seen`, `\SEEN`, `\seen`} {
		got := searchUIDs(t, sess, &imap.SearchCriteria{NotFlag: []imap.Flag{imap.Flag(spelling)}})
		if containsUID(got, 1) || containsUID(got, 2) {
			t.Errorf("SEARCH NOT %s returned a seen message (uids=%v)", spelling, got)
		}
		if !containsUID(got, 3) {
			t.Errorf("SEARCH NOT %s missed the unseen message (uids=%v)", spelling, got)
		}
	}
}

// TestSearchKeywordsAreCaseInsensitive replaces the test that required the
// RA6X-028 defect.
//
// It previously asserted that `$junk` must NOT find a stored `$Junk`, on the
// understanding that keywords are case-sensitive. Flag names are
// case-insensitive — all of them (RFC 9051 §2.3.2, RFC 9007 §1.2) — so that
// assertion was pinning the bug: a client could not reliably find or clear a
// keyword it had written in a different case, and a message could end up
// carrying `$Junk` and `$junk` as two separate flags.
func TestSearchKeywordsAreCaseInsensitive(t *testing.T) {
	sess := mutationFixture(t)
	setFlags(t, sess, 1, []string{`$Junk`})

	for _, spelling := range []string{`$Junk`, `$junk`, `$JUNK`, `$jUnK`} {
		got := searchUIDs(t, sess, &imap.SearchCriteria{Flag: []imap.Flag{imap.Flag(spelling)}})
		if !containsUID(got, 1) {
			t.Errorf("SEARCH KEYWORD %s missed its row (uids=%v); flag names are case-insensitive",
				spelling, got)
		}
	}

	// A genuinely different keyword still does not match.
	got := searchUIDs(t, sess, &imap.SearchCriteria{Flag: []imap.Flag{imap.Flag(`$NotJunk`)}})
	if containsUID(got, 1) {
		t.Errorf("SEARCH KEYWORD $NotJunk matched a $Junk row (uids=%v)", got)
	}
}
