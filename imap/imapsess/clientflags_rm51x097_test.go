package imapsess

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestClientFlagValidationOnWire(t *testing.T) {
	s := mutationFixture(t)
	c := fetchWire(t, s)
	for i, tc := range []struct{ flag, want string }{
		{`\*`, "BAD"}, {`\Recent`, "BAD"}, {`\Bogus`, "BAD"}, {strings.Repeat("k", 65), "NO [LIMIT]"},
	} {
		tag := fmt.Sprintf("f%d", i)
		c.send(tag + " STORE 1 +FLAGS (" + tc.flag + ")")
		if got := c.tagged(tag); !strings.HasPrefix(got, tag+" "+tc.want) {
			t.Fatal(got)
		}
		_, err := s.Append("INBOX", newLiteral([]byte(appendRawMsg)), &imap.AppendOptions{Flags: []imap.Flag{imap.Flag(tc.flag)}})
		var ie *imap.Error
		if !errors.As(err, &ie) || string(ie.Type) != strings.Fields(tc.want)[0] {
			t.Fatalf("APPEND %q = %v", tc.flag, err)
		}
	}
	var flags []string
	for i := 0; i < 100; i++ {
		flags = append(flags, fmt.Sprintf("keyword%d", i))
	}
	c.send("full STORE 1 FLAGS (" + strings.Join(flags, " ") + ")")
	if got := c.tagged("full"); !strings.HasPrefix(got, "full OK") {
		t.Fatal(got)
	}
	c.send("extra STORE 1 +FLAGS (keyword100)")
	if got := c.tagged("extra"); !strings.HasPrefix(got, "extra NO [LIMIT]") {
		t.Fatal(got)
	}
	var count int
	if err := s.be.Pool.QueryRow(context.Background(), `SELECT cardinality(flags) FROM messages WHERE folder_id=$1 AND uid=1`, s.selectedFolderID).Scan(&count); err != nil || count != 100 {
		t.Fatalf("stored flags=%d, %v", count, err)
	}
	c.send("same STORE 1 +FLAGS (KEYWORD1)")
	if got := c.tagged("same"); !strings.HasPrefix(got, "same OK") {
		t.Fatal(got)
	}
}
