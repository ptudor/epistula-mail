package imapsess

import (
	"context"
	"github.com/emersion/go-imap/v2"
	"testing"
)

func TestSearchUnicodeInCLocale(t *testing.T) {
	s := mutationFixture(t)
	defer s.Close()
	var locale string
	if err := s.be.Pool.QueryRow(context.Background(), `SELECT datcollate FROM pg_database WHERE datname=current_database()`).Scan(&locale); err != nil {
		t.Fatal(err)
	}
	if locale != "C" {
		t.Skipf("requires C database; got %s", locale)
	}
	if _, err := s.be.Pool.Exec(context.Background(), `UPDATE messages SET text_body='Über',headers='{"X-Title":["Über"]}' WHERE uid=1`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*imap.SearchCriteria{{Body: []string{"über"}}, {Text: []string{"ÜBER"}}, {Header: []imap.SearchCriteriaHeaderField{{Key: "X-Title", Value: "über"}}}} {
		if got := searchUIDs(t, s, c); len(got) != 1 || got[0] != 1 {
			t.Fatal(got)
		}
	}
}
func TestMoveWithinSelectedFolderReassignsUID(t *testing.T) {
	s := mutationFixture(t)
	defer s.Close()
	if err := s.Move(nil, imap.UIDSetNum(1), "INBOX"); err != nil {
		t.Fatal(err)
	}
	if messageExists(t, s, s.selectedFolderID, 1) || !messageExists(t, s, s.selectedFolderID, 4) {
		t.Fatal("same-folder MOVE must replace the source UID")
	}
}
