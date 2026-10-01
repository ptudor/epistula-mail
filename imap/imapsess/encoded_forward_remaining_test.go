package imapsess

import (
	"bytes"
	"encoding/base64"
	"mime/quotedprintable"
	"strings"
	"testing"

	"github.com/ptudor/epistula-mail/database/ingest"
)

func TestRemainingEncodedForwardTraversal(t *testing.T) {
	for _, encoding := range []string{"base64", "quoted-printable"} {
		for _, multipart := range []bool{false, true} {
			t.Run(encoding+map[bool]string{true: "/multipart", false: "/singlepart"}[multipart], func(t *testing.T) {
				inner := "From: forwarded@example.invalid\r\nSubject: inner = value\r\n"
				if multipart {
					inner += "Content-Type: multipart/mixed; boundary=i\r\n\r\n--i\r\nContent-Type: text/plain\r\n\r\nfirst\r\n--i\r\nContent-Type: text/plain\r\n\r\nsecond = value\r\n--i--\r\n"
				} else {
					inner += "\r\nsecond = value"
				}
				var encoded string
				if encoding == "base64" {
					encoded = base64.StdEncoding.EncodeToString([]byte(inner))
				} else {
					var b bytes.Buffer
					w := quotedprintable.NewWriter(&b)
					_, _ = w.Write([]byte(inner))
					if err := w.Close(); err != nil {
						t.Fatal(err)
					}
					encoded = b.String()
				}
				raw := []byte("Content-Type: multipart/mixed; boundary=o\r\n\r\n--o\r\nContent-Type: message/rfc822\r\nContent-Transfer-Encoding: " + encoding + "\r\n\r\n" + encoded + "\r\n--o--\r\n")
				msg, err := ingest.New(ingest.DefaultLimits()).Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(msg.TextBody, "second = value") {
					t.Fatal("ingest did not accept and decode enclosed message")
				}
				container, err := resolveRawPart(raw, []int{1})
				if err != nil {
					t.Fatal(err)
				}
				if string(container.body) != encoded {
					t.Fatal("container bytes changed")
				}
				if rawHeaderValue(container.header, "content-transfer-encoding") != encoding {
					t.Fatal("container CTE changed")
				}
				enclosed, ok := encapsulatedOf(container)
				if !ok {
					t.Fatal("encoded enclosed message unavailable")
				}
				if !bytes.Contains(enclosed.header, []byte("Subject: inner = value\r\n")) {
					t.Fatalf("HEADER=%q", enclosed.header)
				}
				_, wantText := ingest.SplitHeaderBody([]byte(inner))
				if !bytes.Equal(enclosed.body, wantText) {
					t.Fatal("TEXT differs from decoded message")
				}
				path := []int{1, 1}
				if multipart {
					path[1] = 2
				}
				part, err := resolveRawPart(raw, path)
				if err != nil {
					t.Fatal(err)
				}
				if string(part.body) != "second = value" {
					t.Fatalf("part %v=%q", path, part.body)
				}
				if string(part.body[7:10]) != "= v" {
					t.Fatal("partial decoded section offsets changed")
				}
			})
		}
	}
}
