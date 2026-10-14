package imapsess

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpaqueMultipartBodyStructureAndPart(t *testing.T) {
	var p persistedBS
	if err := json.Unmarshal([]byte(`{"type":"multipart","subtype":"mixed","size":10}`), &p); err != nil {
		t.Fatal(err)
	}
	wire := serializeBodyStructure(t, convertBS(p))
	if !strings.Contains(strings.ToLower(wire), `bodystructure ("text" "plain"`) {
		t.Fatal(wire)
	}
	for _, ct := range []string{"multipart/mixed", "multipart/mixed; boundary=b"} {
		raw := []byte("Content-Type: " + ct + "\r\n\r\nopaque\r\n")
		e, err := resolveRawPart(raw, []int{1})
		if err != nil || string(e.body) != "opaque\r\n" {
			t.Fatalf("%s: body=%q err=%v", ct, e.body, err)
		}
	}
}

func TestStoredMissingBoundaryFetch(t *testing.T) {
	sess, _ := appendFixture(t)
	defer sess.Close()
	mustCreateFolder(t, sess, "INBOX")
	raw := "Content-Type: multipart/mixed\r\n\r\nopaque\r\n"
	if _, err := sess.Append("INBOX", newLiteral([]byte(raw)), nil); err != nil {
		t.Fatal(err)
	}
	c := fetchWire(t, sess)
	c.send("f FETCH 1 (BODYSTRUCTURE BODY.PEEK[] BODY.PEEK[1])")
	var response strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		response.WriteString(line)
		if strings.HasPrefix(line, "f ") {
			if !strings.HasPrefix(line, "f OK") {
				t.Fatal(line)
			}
			break
		}
	}
	if got := response.String(); !strings.Contains(got, raw) || !strings.Contains(strings.ToLower(got), `bodystructure ("text" "plain"`) || !strings.Contains(got, "BODY[1] {8}\r\nopaque\r\n") {
		t.Fatal(got)
	}
}
