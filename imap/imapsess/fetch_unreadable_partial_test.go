package imapsess

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
)

// TestFetchAnswersTheRestPastAnUnreadableMessage is the junia 2026-11-03
// regression. One message's blob became unreadable, and every FETCH of a UID
// range containing it ended NO at that message. The messages after it in the
// range were never sent, so an iPhone, which syncs by range, kept a message
// that arrived meanwhile missing even after the blob became readable again.
// Now only the unreadable message goes unanswered: every other message in the
// range gets its response, and the command still ends NO [SERVERBUG].
func TestFetchAnswersTheRestPastAnUnreadableMessage(t *testing.T) {
	sess, store := appendFixture(t)
	defer sess.Close()
	mustCreateFolder(t, sess, "INBOX")
	for i, subject := range []string{"first", "second", "third"} {
		msg := strings.Replace(appendRawMsg, "Subject: APPENDed", "Subject: "+subject, 1)
		msg = strings.Replace(msg, "<append-1@x>", fmt.Sprintf("<append-%d@x>", i+1), 1)
		if _, err := sess.Append("INBOX", newLiteral([]byte(msg)), nil); err != nil {
			t.Fatal(err)
		}
	}

	// Make the middle message unreadable, as the stranded directory did.
	var sha string
	var date time.Time
	if err := sess.be.Pool.QueryRow(context.Background(),
		`SELECT encode(raw_sha256,'hex'), raw_blob_date FROM messages WHERE uid = 2`,
	).Scan(&sha, &date); err != nil {
		t.Fatal(err)
	}
	path, err := store.PathFor(blob.KindRaw, sess.tenant, blob.BucketFromTime(date), sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	c := fetchWire(t, sess)
	for i, items := range []string{
		"BODY.PEEK[]",
		"BODY.PEEK[HEADER.FIELDS (SUBJECT)]",
		"ENVELOPE",
	} {
		tag := fmt.Sprintf("f%d", i)
		c.send(tag + " UID FETCH 1:3 (" + items + ")")
		var answered []string
		line := c.until(func(l string) bool {
			if f := strings.Fields(l); len(f) >= 3 && f[0] == "*" && f[2] == "FETCH" {
				answered = append(answered, f[1])
			}
			return strings.HasPrefix(l, tag+" ")
		})
		if !strings.HasPrefix(line, tag+" NO [SERVERBUG]") {
			t.Errorf("%s: tagged response %q, want NO [SERVERBUG]", items, line)
		}
		if got := strings.Join(answered, ","); got != "1,3" {
			t.Errorf("%s: FETCH responses for sequence numbers %q, want 1,3", items, got)
		}
		c.send("n NOOP")
		if got := c.tagged("n"); !strings.HasPrefix(got, "n OK") {
			t.Fatalf("session unusable after the partial FETCH: %q", got)
		}
	}
}
