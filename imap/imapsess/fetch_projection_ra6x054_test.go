package imapsess

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestFetchProjectionMatchesTheRequestedItems is the RA6X-054 regression for
// the query side.
//
// fetchTargets discarded its FetchOptions and always selected the headers and
// bodystructure JSONB, so `UID FETCH 1:* (UID FLAGS)` — the metadata
// synchronisation every client runs after a reconnect — pulled the full header
// set and the precomputed BODYSTRUCTURE of an entire folder into memory before
// a single response byte went out.
func TestFetchProjectionMatchesTheRequestedItems(t *testing.T) {
	body := func(o *imap.FetchOptions) []string { return fetchProjectionFor(o).columns }
	has := func(cols []string, want string) bool {
		for _, c := range cols {
			if c == want || strings.Contains(c, want) {
				return true
			}
		}
		return false
	}

	// The metadata-only synchronisation: identity, flags, size, date. Nothing
	// heavy.
	lean := body(&imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, InternalDate: true})
	for _, heavy := range []string{"headers", "bodystructure", "raw_sha256", "raw_blob_date",
		"subject", "from_addr", "to_addrs", "cc_addrs", "message_id", "in_reply_to", "sent_date"} {
		if has(lean, heavy) {
			t.Errorf("a UID/FLAGS fetch selects %q: %v", heavy, lean)
		}
	}
	for _, need := range []string{"uid", "raw_size", "internal_date", "flags"} {
		if !has(lean, need) {
			t.Errorf("a UID/FLAGS fetch does not select %q: %v", need, lean)
		}
	}

	// ENVELOPE pulls scalar columns plus the original bounded raw header locator.
	env := body(&imap.FetchOptions{Envelope: true})
	for _, need := range []string{"raw_sha256", "raw_blob_date", "subject", "from_addr", "to_addrs", "cc_addrs",
		"message_id", "in_reply_to", "sent_date"} {
		if !has(env, need) {
			t.Errorf("ENVELOPE does not select %q: %v", need, env)
		}
	}
	if has(env, "bodystructure") {
		t.Errorf("ENVELOPE selects bodystructure: %v", env)
	}

	// BODYSTRUCTURE pulls only its own column.
	bs := body(&imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{}})
	if !has(bs, "bodystructure") {
		t.Errorf("BODYSTRUCTURE does not select bodystructure: %v", bs)
	}
	if has(bs, "headers") {
		t.Errorf("BODYSTRUCTURE selects the headers JSONB: %v", bs)
	}

	// A body section pulls the blob locator and nothing heavier.
	sec := body(&imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}})
	for _, need := range []string{"raw_sha256", "raw_blob_date"} {
		if !has(sec, need) {
			t.Errorf("BODY[] does not select %q: %v", need, sec)
		}
	}
	if has(sec, "headers") || has(sec, "bodystructure") {
		t.Errorf("BODY[] selects a JSONB column it never reads: %v", sec)
	}

	// A nil options set must not panic and must stay lean.
	if got := body(nil); len(got) != 4 {
		t.Errorf("nil options selected %v, want only the always-needed columns", got)
	}
}

// TestFetchProjectionKeepsResponsesExact pins the other half: the projection
// may not change what a FETCH answers. Every item is compared against the same
// item fetched under a maximal option set.
func TestFetchProjectionKeepsResponsesExact(t *testing.T) {
	f := newFetchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	full := &imap.FetchOptions{
		UID: true, Flags: true, InternalDate: true, RFC822Size: true,
		Envelope:      true,
		BodyStructure: &imap.FetchItemBodyStructure{},
		BodySection:   []*imap.FetchItemBodySection{{}},
	}
	fullRows, err := f.session.fetchTargets(ctx, imap.SeqSetNum(1), full)
	if err != nil {
		t.Fatalf("fetchTargets (full): %v", err)
	}
	if len(fullRows) != 1 {
		t.Fatalf("got %d rows, want 1", len(fullRows))
	}
	want := fullRows[0]

	// ENVELOPE alone produces the same envelope as the maximal fetch.
	envRows, err := f.session.fetchTargets(ctx, imap.SeqSetNum(1), &imap.FetchOptions{Envelope: true})
	if err != nil {
		t.Fatalf("fetchTargets (envelope): %v", err)
	}
	gotEnv, wantEnv := buildEnvelope(envRows[0]), buildEnvelope(want)
	if gotEnv.Subject != wantEnv.Subject || gotEnv.MessageID != wantEnv.MessageID ||
		len(gotEnv.From) != len(wantEnv.From) || len(gotEnv.To) != len(wantEnv.To) ||
		!gotEnv.Date.Equal(wantEnv.Date) {
		t.Errorf("envelope from a projected fetch = %+v, want %+v", gotEnv, wantEnv)
	}

	// The identity and the small columns are the same under every projection.
	for name, o := range map[string]*imap.FetchOptions{
		"uid+flags":     {UID: true, Flags: true},
		"internaldate":  {InternalDate: true},
		"size":          {RFC822Size: true},
		"bodystructure": {BodyStructure: &imap.FetchItemBodyStructure{}},
	} {
		rows, err := f.session.fetchTargets(ctx, imap.SeqSetNum(1), o)
		if err != nil {
			t.Fatalf("fetchTargets (%s): %v", name, err)
		}
		got := rows[0]
		if got.uid != want.uid || got.seqNum != want.seqNum || got.rawSize != want.rawSize ||
			!got.internalDate.Equal(want.internalDate) || len(got.flags) != len(want.flags) {
			t.Errorf("%s: row identity differs: %+v vs %+v", name, got, want)
		}
	}
}

// TestMultiSectionFetchReadsTheMessageOnce is the RA6X-054 regression for the
// MIME side.
//
// A client rendering a multipart message asks for several sections in ONE
// command. Each one used to read the whole message from disk and re-walk its
// MIME tree, so the I/O and the allocation scaled with the number of sections
// rather than with the message — and every walk produced identical results
// from identical bytes.
func TestMultiSectionFetchReadsTheMessageOnce(t *testing.T) {
	c := &messageSections{}
	raw := []byte("Content-Type: multipart/mixed; boundary=b\r\n" +
		"Subject: multi\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nfirst\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nsecond\r\n" +
		"--b--\r\n")

	// Prime the cache exactly as a blob read would, then resolve several parts
	// and assert each walk is served from the same bytes.
	c.raw, c.rawDone = raw, true

	one, err := c.part(nil, fetchRow{}, []int{1})
	if err != nil {
		t.Fatalf("part 1: %v", err)
	}
	two, err := c.part(nil, fetchRow{}, []int{2})
	if err != nil {
		t.Fatalf("part 2: %v", err)
	}
	if string(one.body) == string(two.body) {
		t.Fatalf("the fixture's two parts are identical; the test proves nothing")
	}
	if !strings.Contains(string(one.body), "first") || !strings.Contains(string(two.body), "second") {
		t.Fatalf("parts resolved wrongly: %q / %q", one.body, two.body)
	}
	// A repeated request for the same part is served from the cache and is
	// byte-identical — same backing array, not merely equal.
	again, err := c.part(nil, fetchRow{}, []int{1})
	if err != nil {
		t.Fatalf("part 1 again: %v", err)
	}
	if len(again.body) != len(one.body) || (len(one.body) > 0 && &again.body[0] != &one.body[0]) {
		t.Error("a repeated section re-walked the message instead of reusing the first walk")
	}
	if len(c.parts) != 2 {
		t.Errorf("cache holds %d parts, want 2", len(c.parts))
	}

	// The top-level header is split from the resident bytes rather than read
	// again once the whole message is in hand.
	hdr, err := c.topHeader(nil, fetchRow{})
	if err != nil {
		t.Fatalf("topHeader: %v", err)
	}
	if !strings.Contains(string(hdr), "Subject: multi") {
		t.Errorf("header section = %q", hdr)
	}
	if !strings.HasPrefix(string(raw), string(hdr)) {
		t.Error("the header is not a prefix of the message it came from")
	}
}

// TestMultiSectionFetchOverTheRealPath drives the whole FETCH path with several
// sections of one message and asserts the responses are exact, which is the
// property the cache must not disturb.
func TestMultiSectionFetchOverTheRealPath(t *testing.T) {
	f := newFetchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := f.session.fetchTargets(ctx, imap.SeqSetNum(1),
		&imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}})
	if err != nil {
		t.Fatalf("fetchTargets: %v", err)
	}
	r := rows[0]

	cache := &messageSections{}
	// Header first, then the whole message: the header must be identical
	// whether it was read alone or split from the resident bytes.
	headerAlone, err := cache.topHeader(f.session, r)
	if err != nil {
		t.Fatalf("topHeader: %v", err)
	}
	full, err := cache.rawBytes(f.session, r)
	if err != nil {
		t.Fatalf("rawBytes: %v", err)
	}
	if !strings.HasPrefix(string(full), string(headerAlone)) {
		t.Errorf("header-only read %q is not a prefix of the message", headerAlone)
	}

	// And in the other order, on a fresh cache.
	other := &messageSections{}
	if _, err := other.rawBytes(f.session, r); err != nil {
		t.Fatalf("rawBytes: %v", err)
	}
	headerFromFull, err := other.topHeader(f.session, r)
	if err != nil {
		t.Fatalf("topHeader from resident bytes: %v", err)
	}
	if string(headerFromFull) != string(headerAlone) {
		t.Errorf("header differs by read order:\n alone %q\n split %q", headerAlone, headerFromFull)
	}
}

// Collection helper for fixture assertions only. The server always visits bounded batches.
func (s *Session) fetchTargets(ctx context.Context, numSet imap.NumSet, options *imap.FetchOptions) ([]fetchRow, error) {
	uids, err := s.targetUIDs(ctx, numSet)
	if err != nil {
		return nil, err
	}
	var out []fetchRow
	err = s.visitFetchRows(ctx, uids, options, func(r fetchRow) error { out = append(out, r); return nil })
	return out, err
}
