package imapsess

import (
	"context"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestFreshAccountInbox(t *testing.T) {
	for _, command := range []string{"SELECT", "STATUS", "LIST", "APPEND"} {
		t.Run(command, func(t *testing.T) {
			s, _ := appendFixture(t)
			defer s.Close()
			switch command {
			case "SELECT":
				d, err := s.Select("inbox", nil)
				if err != nil || d.NumMessages != 0 || d.UIDValidity == 0 {
					t.Fatalf("data=%+v err=%v", d, err)
				}
			case "STATUS":
				d, err := s.Status("inbox", &imap.StatusOptions{UIDValidity: true})
				if err != nil || d.UIDValidity == 0 {
					t.Fatalf("data=%+v err=%v", d, err)
				}
			case "APPEND":
				if _, err := s.Append("inbox", newLiteral([]byte(appendRawMsg)), nil); err != nil {
					t.Fatal(err)
				}
			case "LIST":
				c := fetchWire(t, s)
				// Remove the INBOX which fetchWire's SELECT created, so LIST
				// independently exercises the missing-folder path.
				if _, err := s.be.Pool.Exec(context.Background(), `DELETE FROM folders WHERE mailbox_id=$1`, s.mailboxID); err != nil {
					t.Fatal(err)
				}
				c.send("u UNSELECT")
				if line := c.tagged("u"); !strings.HasPrefix(line, "u OK") {
					t.Fatal(line)
				}
				c.send(`l LIST "" inbox`)
				line := c.until(func(line string) bool { return strings.HasPrefix(line, "* LIST") })
				if !strings.Contains(line, "INBOX") {
					t.Fatal(line)
				}
				if line := c.tagged("l"); !strings.HasPrefix(line, "l OK") {
					t.Fatal(line)
				}
			}
		})
	}
}
