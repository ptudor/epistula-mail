package imapsess

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestSearchNotAllMatchesNothing is the RA6X-011 regression.
//
// Empty criteria mean ALL, which the caller translates to TRUE — so their
// negation is FALSE. The NOT loop dropped an empty nested expression instead,
// making `NOT ALL` a no-op, and an expression with no other terms then became
// TRUE: `SEARCH NOT ALL` returned EVERY message rather than none. The OR arm
// already substituted TRUE for an empty operand, which is the same convention
// read the other way round — the two simply disagreed.
func TestSearchNotAllMatchesNothing(t *testing.T) {
	sess := mutationFixture(t)

	all := searchUIDs(t, sess, &imap.SearchCriteria{})
	if len(all) != 3 {
		t.Fatalf("SEARCH ALL returned %v, want all 3 fixture messages", all)
	}

	notAll := searchUIDs(t, sess, &imap.SearchCriteria{
		Not: []imap.SearchCriteria{{}},
	})
	if len(notAll) != 0 {
		t.Errorf("SEARCH NOT ALL returned %v, want nothing", notAll)
	}

	// Double negation is ALL again.
	notNotAll := searchUIDs(t, sess, &imap.SearchCriteria{
		Not: []imap.SearchCriteria{{Not: []imap.SearchCriteria{{}}}},
	})
	if len(notNotAll) != 3 {
		t.Errorf("SEARCH NOT NOT ALL returned %v, want all 3", notNotAll)
	}

	// OR ALL X is everything; OR (NOT ALL) X is just X.
	orAll := searchUIDs(t, sess, &imap.SearchCriteria{
		Or: [][2]imap.SearchCriteria{{
			{},
			{Flag: []imap.Flag{imap.FlagFlagged}},
		}},
	})
	if len(orAll) != 3 {
		t.Errorf("SEARCH OR ALL FLAGGED returned %v, want all 3", orAll)
	}

	orNotAll := searchUIDs(t, sess, &imap.SearchCriteria{
		Or: [][2]imap.SearchCriteria{{
			{Not: []imap.SearchCriteria{{}}},
			{Flag: []imap.Flag{imap.FlagFlagged}},
		}},
	})
	// The fixture flags uid 2.
	if len(orNotAll) != 1 || orNotAll[0] != 2 {
		t.Errorf("SEARCH OR (NOT ALL) FLAGGED returned %v, want just uid 2", orNotAll)
	}
}

// TestSearchBodyAndTextHaveTheRightScope is the RA6X-026 regression.
//
// BODY and TEXT both compiled to the same `fts` tsvector, which indexes subject
// plus extracted plain text. BODY therefore matched a word present only in the
// Subject (a false positive), TEXT could not find a string present only in a
// header (a false negative), and token matching missed substrings entirely.
func TestSearchBodyAndTextHaveTheRightScope(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// One message whose subject, body and an X-header each carry a distinct
	// marker, plus a body word to search a fragment of.
	if _, err := sess.be.Pool.Exec(ctx, `
		UPDATE messages
		   SET subject = 'SUBJECTONLYWORD quarterly',
		       text_body = 'the body mentions foobar and BODYONLYWORD',
		       headers = '{"X-Marker": ["HEADERONLYWORD"]}'::jsonb
		 WHERE folder_id = $1 AND uid = 1`, sess.selectedFolderID); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	body := func(term string) []imap.UID {
		return searchUIDs(t, sess, &imap.SearchCriteria{Body: []string{term}})
	}
	text := func(term string) []imap.UID {
		return searchUIDs(t, sess, &imap.SearchCriteria{Text: []string{term}})
	}

	// BODY searches the body only.
	if got := body("BODYONLYWORD"); !containsUID(got, 1) {
		t.Errorf("BODY missed a word in the body: %v", got)
	}
	if got := body("SUBJECTONLYWORD"); containsUID(got, 1) {
		t.Errorf("BODY matched a word present only in the Subject: %v", got)
	}
	if got := body("HEADERONLYWORD"); containsUID(got, 1) {
		t.Errorf("BODY matched a word present only in a header: %v", got)
	}

	// TEXT searches the whole message.
	for _, term := range []string{"BODYONLYWORD", "SUBJECTONLYWORD", "HEADERONLYWORD"} {
		if got := text(term); !containsUID(got, 1) {
			t.Errorf("TEXT missed %q, which is in the message: %v", term, got)
		}
	}

	// Substring semantics: a middle-of-word fragment matches.
	if got := body("oba"); !containsUID(got, 1) {
		t.Errorf("BODY %q missed a body containing 'foobar': %v", "oba", got)
	}
	// Case-insensitive.
	if got := body("bodyonlyword"); !containsUID(got, 1) {
		t.Errorf("BODY is case-sensitive: %v", got)
	}
	// A term that is genuinely absent still does not match.
	if got := body("NOTPRESENTANYWHERE"); containsUID(got, 1) {
		t.Errorf("BODY matched an absent term: %v", got)
	}
}

// searchUIDsInTZ runs the generated SEARCH SQL on ONE pinned connection whose
// TimeZone is tz. Pool.Exec would set the GUC on whichever connection it
// happened to acquire and hand it straight back, so the query under test would
// run somewhere else entirely — the point here is that the date reduction is
// independent of the session GUC, and only a pinned connection can show that.
func searchUIDsInTZ(t *testing.T, sess *Session, c *imap.SearchCriteria, tz string) []imap.UID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b := &searchSQL{}
	b.args = append(b.args, sess.selectedFolderID)
	where, err := b.buildCriteria(c)
	if err != nil {
		t.Fatalf("buildCriteria: %v", err)
	}
	if where == "" {
		where = "TRUE"
	}

	conn, err := sess.be.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET TimeZone TO "+quoteLiteral(tz)); err != nil {
		t.Fatalf("set timezone %s: %v", tz, err)
	}
	defer func() {
		if _, err := conn.Exec(context.Background(), "RESET TimeZone"); err != nil {
			t.Errorf("reset timezone: %v", err)
		}
	}()

	rows, err := conn.Query(ctx, searchQuery(false, where, ""), b.args...)
	if err != nil {
		t.Fatalf("search query: %v", err)
	}
	defer rows.Close()
	var uids []imap.UID
	for rows.Next() {
		var seq, uid int64
		if err := rows.Scan(&seq, &uid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		uids = append(uids, imap.UID(uid))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return uids
}

// quoteLiteral renders a timezone name as a SQL string literal. The names come
// from this file only, but doubling any quote keeps it correct for a caller
// that passes something else.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// hostileZones are the connection timezones each date search is run under: UTC,
// the furthest zone east of it, and one well west. A reduction that leaks the
// session GUC gives a different answer in at least one of them.
var hostileZones = []string{"UTC", "Pacific/Kiritimati", "America/Los_Angeles"}

// TestSearchSentDateFollowsTheSendersCalendar is the RA6X-048 regression.
//
// SENTSINCE/SENTBEFORE/SENTON are defined against the date the SENDER wrote,
// disregarding time and timezone (RFC 9051 §6.4.4). sent_date is a timestamptz
// whose original offset is gone, and `sent_date::date` additionally reduced it
// in the CONNECTION's timezone — so the same search over the same data returned
// different results on different connections, and a message the sender dated
// 4 September in +14:00 was filed under 3 September.
func TestSearchSentDateFollowsTheSendersCalendar(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The sender wrote 2026-09-04 00:30 +14:00 — 2026-09-03 10:30 UTC.
	if _, err := sess.be.Pool.Exec(ctx, `
		UPDATE messages
		   SET sent_date = '2026-09-04 00:30:00+14'::timestamptz,
		       sent_date_local = DATE '2026-09-04'
		 WHERE folder_id = $1 AND uid = 1`, sess.selectedFolderID); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	// The other two fixture messages are dated in May; keep them out of the way
	// of the September windows by leaving them alone.

	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }

	for _, tz := range hostileZones {
		// SENTON 4 September: the day the sender wrote.
		got := searchUIDsInTZ(t, sess, &imap.SearchCriteria{SentSince: day(4), SentBefore: day(5)}, tz)
		if !containsUID(got, 1) {
			t.Errorf("tz=%s: SENTON 4 Sep missed a message the sender dated 4 Sep +1400: %v", tz, got)
		}
		// SENTON 3 September: the UTC day, which is NOT what the sender wrote.
		got = searchUIDsInTZ(t, sess, &imap.SearchCriteria{SentSince: day(3), SentBefore: day(4)}, tz)
		if containsUID(got, 1) {
			t.Errorf("tz=%s: SENTON 3 Sep matched a message the sender dated 4 Sep: %v", tz, got)
		}
	}
}

// TestInternalDateSearchIsTimezoneStable pins the other half. INTERNALDATE is
// the server's own arrival time and is rendered to the client from its UTC
// value, so its date reduction has to use UTC too — otherwise SINCE/BEFORE
// disagree with the date the same client was just shown.
func TestInternalDateSearchIsTimezoneStable(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 01:00 UTC on 4 September — 15:00 on the 4th in Kiritimati, but 18:00 on
	// the 3rd in Los Angeles. Only a UTC reduction answers the same way twice.
	if _, err := sess.be.Pool.Exec(ctx, `
		UPDATE messages SET internal_date = '2026-09-04 01:00:00+00'::timestamptz
		 WHERE folder_id = $1 AND uid = 1`, sess.selectedFolderID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	since := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	for _, tz := range hostileZones {
		got := searchUIDsInTZ(t, sess, &imap.SearchCriteria{Since: since, Before: before}, tz)
		if !containsUID(got, 1) {
			t.Errorf("tz=%s: SINCE/BEFORE missed a message dated 2026-09-04T01:00Z: %v", tz, got)
		}
	}
}
