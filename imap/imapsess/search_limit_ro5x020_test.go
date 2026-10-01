package imapsess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// seedMessages inserts n minimal messages into the selected folder.
func seedMessages(t *testing.T, sess *Session, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Continue from the highest existing UID so repeated calls in one test
	// do not collide on (folder_id, uid).
	var nextUID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(uid), 0) + 1 FROM messages WHERE folder_id = $1`,
		sess.selectedFolderID,
	).Scan(&nextUID); err != nil {
		t.Fatalf("next uid: %v", err)
	}

	hdrJSON, _ := json.Marshal(map[string][]string{})
	bsJSON, _ := json.Marshal(map[string]any{"type": "text", "subtype": "plain", "size": 10})
	for i := 0; i < n; i++ {
		uid := nextUID + int64(i)
		sha := make([]byte, 32)
		sha[0] = byte(uid % 251)
		sha[1] = byte(i % 251)
		sha[2] = byte((i / 251) % 251)
		when := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		if _, err := sess.be.Pool.Exec(ctx, `
			INSERT INTO messages (
				folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
				message_id, subject, from_addr, to_addrs, cc_addrs,
				sent_date, headers, text_body, bodystructure, flags
			) VALUES ($1,$2,$3,$4,10,$5,$6,'s','x@y.invalid','{}','{}',$5,$7,'b',$8,'{}')`,
			sess.selectedFolderID, uid, sha, when, when,
			fmt.Sprintf("<m%d@x.invalid>", i), hdrJSON, bsJSON,
		); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}
}

// TestSearchRefusesOversizeResultSet is the RO5X-020 regression.
//
// SEARCH accumulated every match into slices and then serialized the whole
// NumSet, with no LIMIT and no cap. One `SEARCH ALL` on a large folder is
// enough — the per-session command-rate cap does not help.
func TestSearchRefusesOversizeResultSet(t *testing.T) {
	sess := mutationFixture(t) // 3 messages already
	seedMessages(t, sess, 100) // 103 total

	sess.be.MaxSearchResults = 50

	_, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{}, nil)
	if err == nil {
		t.Fatal("SEARCH ALL returned a result set larger than the cap without error")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("err = %T (%v), want *imap.Error", err, err)
	}
	if imapErr.Code != imap.ResponseCodeLimit {
		t.Errorf("Code = %q, want LIMIT", imapErr.Code)
	}
	if imapErr.Type != imap.StatusResponseTypeNo {
		t.Errorf("Type = %v, want NO", imapErr.Type)
	}
}

// TestSearchWithinCapReturnsEverything proves the cap does not truncate: a
// result set under the limit comes back complete.
func TestSearchWithinCapReturnsEverything(t *testing.T) {
	sess := mutationFixture(t)
	seedMessages(t, sess, 100) // 103 total

	sess.be.MaxSearchResults = 5000

	data, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{}, nil)
	if err != nil {
		t.Fatalf("SEARCH within the cap: %v", err)
	}
	nums, _ := data.All.(imap.UIDSet).Nums()
	if len(nums) != 103 {
		t.Errorf("SEARCH returned %d UIDs, want 103", len(nums))
	}
	if data.Count != 103 {
		t.Errorf("Count = %d, want 103", data.Count)
	}
}

// TestSearchExactlyAtCapSucceeds is the boundary: max results is allowed,
// max+1 is refused. Getting this wrong by one would refuse a legitimate
// result set.
func TestSearchExactlyAtCapSucceeds(t *testing.T) {
	sess := mutationFixture(t) // 3 messages
	seedMessages(t, sess, 47)  // 50 total

	sess.be.MaxSearchResults = 50
	data, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{}, nil)
	if err != nil {
		t.Fatalf("SEARCH with exactly max results was refused: %v", err)
	}
	if data.Count != 50 {
		t.Errorf("Count = %d, want 50", data.Count)
	}

	// One more ANNOUNCED message tips it over. Re-select to establish the
	// expanded client view; unannounced arrivals must not enter SEARCH.
	seedMessages(t, sess, 1)
	if _, err := sess.Select("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{}, nil); err == nil {
		t.Error("SEARCH with max+1 results should be refused")
	}
}

// TestSearchZeroCapDisablesLimit keeps the documented "0 disables" behaviour.
func TestSearchZeroCapDisablesLimit(t *testing.T) {
	sess := mutationFixture(t)
	seedMessages(t, sess, 100)

	sess.be.MaxSearchResults = 0
	data, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{}, nil)
	if err != nil {
		t.Fatalf("SEARCH with the limit disabled: %v", err)
	}
	if data.Count != 103 {
		t.Errorf("Count = %d, want 103", data.Count)
	}
}

// TestSearchCapAppliesToWindowedFormToo covers the other query shape: a
// sequence-number search takes the row_number() window path, which builds a
// different SQL string and must carry the LIMIT as well.
func TestSearchCapAppliesToWindowedFormToo(t *testing.T) {
	sess := mutationFixture(t)
	seedMessages(t, sess, 100)

	sess.be.MaxSearchResults = 50
	// NumKindSeq forces the windowed query form.
	if _, err := sess.Search(imapserver.NumKindSeq, &imap.SearchCriteria{}, nil); err == nil {
		t.Error("windowed SEARCH form ignored the result cap")
	}
}
