package imapsess

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// searchFixture builds a session selected on a folder with N messages
// inserted, each carrying a small fixed shape so each SEARCH case can
// assert against a known result set.
func searchFixture(t *testing.T) *Session {
	t.Helper()
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var folderID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', $2, 1) RETURNING id`,
		sess.mailboxID, time.Now().Unix(),
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	sess.selectedFolderID = folderID
	sess.selectedFolderName = "INBOX"

	rows := []struct {
		uid          int64
		subject      string
		from         string
		internalDate time.Time
		size         int64
		flags        []string
		textBody     string
		headers      map[string][]string
	}{
		{1, "first message", "alice@x", date(2026, 5, 1), 100, []string{`\Seen`}, "hello world", map[string][]string{"X-Spam-Status": {"No"}}},
		{2, "important update", "bob@y", date(2026, 5, 10), 1024, []string{`\Flagged`}, "urgent please read", map[string][]string{"X-Spam-Status": {"Yes, score=9"}}},
		{3, "newsletter", "list@z", date(2026, 5, 15), 50_000, nil, "this newsletter is long", map[string][]string{}},
	}
	for _, r := range rows {
		hdrJSON, _ := json.Marshal(r.headers)
		bsJSON, _ := json.Marshal(map[string]any{"type": "text", "subtype": "plain", "size": r.size})
		flagsToInsert := r.flags
		if flagsToInsert == nil {
			flagsToInsert = []string{}
		}
		// Synthesize a unique-per-uid sha256 in Go so the INSERT doesn't
		// have to reuse $2 in two different type contexts.
		sha := fixtureSHA(r.uid)
		if _, err := sess.be.Pool.Exec(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				message_id, subject, from_addr, to_addrs, cc_addrs,
				sent_date, headers, text_body, bodystructure, flags
			) VALUES (
				$1, $2, $3, $4, $5, $6,
				$7, $8, $9, '{}'::text[], '{}'::text[],
				$10, $11, $12, $13, $14
			)`,
			folderID, r.uid, sha, r.internalDate, r.size, r.internalDate,
			"msg-"+r.subject, r.subject, r.from,
			r.internalDate, hdrJSON, r.textBody, bsJSON, flagsToInsert,
		); err != nil {
			t.Fatalf("insert msg %d: %v", r.uid, err)
		}
		// A raw blob on disk so COPY/MOVE (R-061 blob-exists check) succeed.
		seedRawBlobFile(t, sess, r.uid, r.internalDate)
	}
	// Bump uidnext past the inserted UIDs so subsequent APPENDs don't conflict.
	if _, err := sess.be.Pool.Exec(ctx,
		`UPDATE folders SET uidnext = 100 WHERE id = $1`, folderID,
	); err != nil {
		t.Fatalf("uidnext bump: %v", err)
	}
	return sess
}

func date(y, m, d int) time.Time {
	return time.Date(y, time.Month(m), d, 12, 0, 0, 0, time.UTC)
}

func uidSetSlice(t *testing.T, s imap.NumSet) []imap.UID {
	t.Helper()
	us, ok := s.(imap.UIDSet)
	if !ok {
		t.Fatalf("got %T, want UIDSet", s)
	}
	nums, _ := us.Nums()
	return nums
}

func TestSearchAllReturnsEverything(t *testing.T) {
	sess := searchFixture(t)
	data, err := sess.Search(imapserver.NumKindUID, nil, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 3 {
		t.Errorf("count = %d, want 3", len(got))
	}
	if data.Count != 3 {
		t.Errorf("data.Count = %d, want 3", data.Count)
	}
}

func TestSearchBySubject(t *testing.T) {
	sess := searchFixture(t)
	criteria := &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "important"}},
	}
	// Subject is in headers JSONB? Our fixture only puts X-Spam-Status
	// in the headers map — Subject lives in the convenience column. So
	// for this fixture the Subject-via-Header path returns 0; that's
	// the right behavior given the schema.
	data, err := sess.Search(imapserver.NumKindUID, criteria, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 0 {
		t.Errorf("Header(Subject) result = %v, want 0 with the fixture", got)
	}
}

func TestSearchHeaderSpam(t *testing.T) {
	sess := searchFixture(t)
	criteria := &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "X-Spam-Status", Value: "Yes"}},
	}
	data, err := sess.Search(imapserver.NumKindUID, criteria, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("Header(X-Spam-Status=Yes) = %v, want [2]", got)
	}
}

func TestSearchFlagged(t *testing.T) {
	sess := searchFixture(t)
	criteria := &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}}
	data, err := sess.Search(imapserver.NumKindUID, criteria, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("Flagged = %v, want [2]", got)
	}
}

func TestSearchNotSeen(t *testing.T) {
	sess := searchFixture(t)
	criteria := &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}
	data, err := sess.Search(imapserver.NumKindUID, criteria, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Errorf("UNSEEN = %v, want [2 3]", got)
	}
}

func TestSearchLargerSmaller(t *testing.T) {
	sess := searchFixture(t)
	criteria := &imap.SearchCriteria{Larger: 500, Smaller: 10_000}
	data, err := sess.Search(imapserver.NumKindUID, criteria, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("500 < size < 10K = %v, want [2]", got)
	}
}

func TestSearchSince(t *testing.T) {
	sess := searchFixture(t)
	criteria := &imap.SearchCriteria{Since: date(2026, 5, 11)}
	data, err := sess.Search(imapserver.NumKindUID, criteria, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 1 || got[0] != 3 {
		t.Errorf("SINCE 2026-05-11 = %v, want [3]", got)
	}
}

func TestSearchBodyFTS(t *testing.T) {
	sess := searchFixture(t)
	criteria := &imap.SearchCriteria{Body: []string{"urgent"}}
	data, err := sess.Search(imapserver.NumKindUID, criteria, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("BODY urgent = %v, want [2]", got)
	}
}

func TestSearchOrCombines(t *testing.T) {
	sess := searchFixture(t)
	criteria := &imap.SearchCriteria{
		Or: [][2]imap.SearchCriteria{{
			{Flag: []imap.Flag{imap.FlagFlagged}}, // matches uid 2
			{Larger: 10_000},                      // matches uid 3
		}},
	}
	data, err := sess.Search(imapserver.NumKindUID, criteria, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Errorf("OR(Flagged, Larger 10K) = %v, want [2 3]", got)
	}
}

func TestSearchUIDRange(t *testing.T) {
	sess := searchFixture(t)
	criteria := &imap.SearchCriteria{
		UID: []imap.UIDSet{{{Start: 1, Stop: 2}}},
	}
	data, err := sess.Search(imapserver.NumKindUID, criteria, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("UID 1:2 = %v, want [1 2]", got)
	}
}

func TestSearchBareStarMatchesOnlyHighest(t *testing.T) {
	sess := searchFixture(t)
	// UID * — RFC 9051: "*" is exactly the highest UID, not the whole
	// folder.
	criteria := &imap.SearchCriteria{
		UID: []imap.UIDSet{{{Start: 0, Stop: 0}}},
	}
	data, err := sess.Search(imapserver.NumKindUID, criteria, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 1 || got[0] != 3 {
		t.Errorf("UID * = %v, want [3] (only the highest message)", got)
	}
}

func TestSearchStarColonNEqualsNColonStar(t *testing.T) {
	sess := searchFixture(t)
	// RFC 3501 §6.4.8: "*:2" and "2:*" are the same range (2..max).
	for _, set := range []imap.UIDSet{
		{{Start: 2, Stop: 0}}, // 2:*
		{{Start: 0, Stop: 2}}, // *:2
	} {
		data, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{UID: []imap.UIDSet{set}}, nil)
		if err != nil {
			t.Fatalf("Search %v: %v", set, err)
		}
		got := uidSetSlice(t, data.All)
		if len(got) != 2 || got[0] != 2 || got[1] != 3 {
			t.Errorf("set %v = %v, want [2 3]", set, got)
		}
	}
}

func TestSearchNColonStarBeyondMaxStillMatchesLast(t *testing.T) {
	sess := searchFixture(t)
	// "100:*" with max UID 3 spans min(100, 3)..3 — a non-empty folder
	// always matches its last message.
	data, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{
		UID: []imap.UIDSet{{{Start: 100, Stop: 0}}},
	}, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := uidSetSlice(t, data.All)
	if len(got) != 1 || got[0] != 3 {
		t.Errorf("UID 100:* = %v, want [3]", got)
	}
}

func TestSearchHeaderLikeWildcardsAreLiteral(t *testing.T) {
	sess := searchFixture(t)
	// "score=9" appears in message 2's X-Spam-Status. A SEARCH value
	// using LIKE metacharacters must match literally, not as wildcards:
	// "score=_" (underscore) would match "score=9" if unescaped.
	data, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "X-Spam-Status", Value: "score=_"}},
	}, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := uidSetSlice(t, data.All); len(got) != 0 {
		t.Errorf("HEADER value with literal underscore matched %v, want no matches (wildcard not escaped)", got)
	}

	// And "%" must not act as match-anything.
	data, err = sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "X-Spam-Status", Value: "100%"}},
	}, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := uidSetSlice(t, data.All); len(got) != 0 {
		t.Errorf("HEADER value with literal %% matched %v, want no matches", got)
	}

	// The real substring still matches.
	data, err = sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "X-Spam-Status", Value: "score=9"}},
	}, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := uidSetSlice(t, data.All); len(got) != 1 || got[0] != 2 {
		t.Errorf("HEADER score=9 = %v, want [2]", got)
	}
}
