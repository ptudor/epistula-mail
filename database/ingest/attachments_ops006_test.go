package ingest

import (
	"strings"
	"testing"
)

// TestEachAttachedPartYieldsOneRow is the OPS-006 regression. An attached text
// part that was neither text/plain nor text/html, such as a .csv or an .ics,
// got two identical attachment rows: the RA6X-049 branch recorded it as an
// attached text part, and the branch for every non-body part recorded it
// again. epistula-api and epistula-mcp then listed it twice.
//
// The matrix covers every media type class (body text and other text/*, with
// a non-text control) under every disposition, with and without a filename
// from either header, in every container shape. The rule it pins: body text
// gets a row only when explicitly attached (RA6X-049) and is searchable text
// either way; every other part gets exactly one row, whatever its
// disposition.
func TestEachAttachedPartYieldsOneRow(t *testing.T) {
	type mediaType struct {
		name, ct string
		// field is where the part's text is projected: "text" for text/plain,
		// "html" for text/html, "" for a part that is not body text.
		field string
		// mediatype is what Attachment.ContentType records.
		mediatype string
	}
	types := []mediaType{
		{"text-plain", "text/plain; charset=us-ascii", "text", "text/plain"},
		{"text-html", "text/html; charset=us-ascii", "html", "text/html"},
		{"text-csv", "text/csv", "", "text/csv"},
		{"text-calendar", "text/calendar; method=REQUEST", "", "text/calendar"},
		{"application-pdf", "application/pdf", "", "application/pdf"},
	}

	type disposition struct {
		name string
		cd   string // Content-Disposition, "" for none
		// ctName is a name parameter appended to Content-Type.
		ctName   string
		filename string
		// attached is RA6X-049's test: Content-Disposition: attachment, or
		// an inline disposition that names a file. A Content-Type name alone
		// is not one.
		attached bool
	}
	dispositions := []disposition{
		{"none", "", "", "", false},
		{"none-typename", "", "n.ext", "n.ext", false},
		{"inline", "inline", "", "", false},
		{"inline-filename", `inline; filename="f.ext"`, "", "f.ext", true},
		{"inline-typename", "inline", "n.ext", "n.ext", false},
		{"attachment", "attachment", "", "", true},
		{"attachment-filename", `attachment; filename="f.ext"`, "", "f.ext", true},
		{"attachment-typename", "attachment", "n.ext", "n.ext", true},
		{"attachment-both", `attachment; filename="f.ext"`, "n.ext", "f.ext", true},
	}

	const header = "From: s@ops006.invalid\r\nSubject: attachments\r\nMIME-Version: 1.0\r\n"
	// Each nesting level needs its own boundary: an enclosed multipart that
	// reused its container's would split the container instead.
	multipart := func(subtype, boundary, cover, child string) string {
		return "Content-Type: multipart/" + subtype + "; boundary=" + boundary + "\r\n\r\n" +
			"--" + boundary + "\r\n" + cover + "\r\n" +
			"--" + boundary + "\r\n" + child + "\r\n" +
			"--" + boundary + "--\r\n"
	}
	const plainCover = "Content-Type: text/plain\r\n\r\nsee attached\r\n"
	const htmlCover = "Content-Type: text/html\r\n\r\n<p>see attached</p>\r\n"
	const enclosed = "From: inner@ops006.invalid\r\nSubject: forwarded\r\n"
	containers := []struct {
		name string
		// wrap returns the whole message around the entity under test, which
		// is the header of one MIME entity followed by its body.
		wrap       func(entity string) string
		partNumber string
	}{
		{"top-level", func(e string) string { return header + e }, "1"},
		{"mixed", func(e string) string { return header + multipart("mixed", "outer", plainCover, e) }, "2"},
		{"alternative", func(e string) string { return header + multipart("alternative", "outer", plainCover, e) }, "2"},
		{"related", func(e string) string { return header + multipart("related", "outer", htmlCover, e) }, "2"},
		{"rfc822-in-mixed", func(e string) string {
			return header + multipart("mixed", "outer", plainCover, "Content-Type: message/rfc822\r\n\r\n"+enclosed+e)
		}, "2.1"},
		{"mixed-in-rfc822-in-mixed", func(e string) string {
			return header + multipart("mixed", "outer", plainCover,
				"Content-Type: message/rfc822\r\n\r\n"+enclosed+multipart("mixed", "inner", plainCover, e))
		}, "2.2"},
		{"top-level-rfc822", func(e string) string {
			return header + "Content-Type: message/rfc822\r\n\r\n" + enclosed + e
		}, "1.1"},
	}

	parsers := []struct {
		name string
		p    *Parser
	}{
		{"refusing", New(DefaultLimits())},
		{"salvaging", NewSalvaging(DefaultLimits())},
	}

	for _, mt := range types {
		for _, d := range dispositions {
			for _, c := range containers {
				for _, pr := range parsers {
					name := mt.name + "/" + d.name + "/" + c.name + "/" + pr.name
					t.Run(name, func(t *testing.T) {
						marker := "ops006-" + mt.name + "-" + d.name + "-" + c.name
						body := marker + "\r\n"
						ct := mt.ct
						if d.ctName != "" {
							ct += `; name="` + d.ctName + `"`
						}
						entity := "Content-Type: " + ct + "\r\n"
						if d.cd != "" {
							entity += "Content-Disposition: " + d.cd + "\r\n"
						}
						entity += "\r\n" + body

						msg := mustParseWith(t, pr.p, c.wrap(entity))

						want := 0
						if mt.field == "" || d.attached {
							want = 1
						}
						if len(msg.Attachments) != want {
							t.Fatalf("got %d attachment rows, want %d: %+v", len(msg.Attachments), want, msg.Attachments)
						}
						if want == 1 {
							a := msg.Attachments[0]
							if a.PartNumber != c.partNumber || a.ContentType != mt.mediatype ||
								a.Filename != d.filename || string(a.Data) != body || a.Size != int64(len(body)) {
								t.Errorf("attachment = {part %q, type %q, filename %q, data %q, size %d}, want {%q, %q, %q, %q, %d}",
									a.PartNumber, a.ContentType, a.Filename, a.Data, a.Size,
									c.partNumber, mt.mediatype, d.filename, body, len(body))
							}
						}

						inText := strings.Contains(msg.TextBody, marker)
						inHTML := strings.Contains(msg.HTMLBody, marker)
						switch mt.field {
						case "text":
							if !inText {
								t.Errorf("text/plain left the text projection: %q", msg.TextBody)
							}
						case "html":
							if !inHTML {
								t.Errorf("text/html left the HTML projection: %q", msg.HTMLBody)
							}
						default:
							if inText || inHTML {
								t.Errorf("a part that is not body text reached the projection: text %q, html %q", msg.TextBody, msg.HTMLBody)
							}
						}
					})
				}
			}
		}
	}
}

// TestSeveralAttachedTextPartsEachYieldOneRow puts the shapes that used to
// double side by side in one message, the way a calendar invitation or a
// report mail arrives, and checks the whole list: one row per part, in part
// order, with no part number repeated.
func TestSeveralAttachedTextPartsEachYieldOneRow(t *testing.T) {
	raw := "From: s@ops006.invalid\r\n" +
		"Subject: invitation\r\n" +
		"Content-Type: multipart/mixed; boundary=outer\r\n" +
		"\r\n" +
		"--outer\r\n" +
		"Content-Type: multipart/alternative; boundary=alt\r\n" +
		"\r\n" +
		"--alt\r\n" +
		"Content-Type: text/plain\r\n\r\nplain body\r\n" +
		"--alt\r\n" +
		"Content-Type: text/html\r\n\r\n<p>html body</p>\r\n" +
		"--alt\r\n" +
		"Content-Type: text/calendar; method=REQUEST\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n" +
		"--alt--\r\n" +
		"\r\n" +
		"--outer\r\n" +
		"Content-Type: text/calendar; method=REQUEST; name=\"invite.ics\"\r\n" +
		"Content-Disposition: attachment; filename=\"invite.ics\"\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n" +
		"--outer\r\n" +
		"Content-Type: text/csv\r\n" +
		"Content-Disposition: inline; filename=\"report.csv\"\r\n\r\na,b\r\n1,2\r\n" +
		"--outer\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Disposition: attachment; filename=\"notes.txt\"\r\n\r\nnotes\r\n" +
		"--outer--\r\n"

	msg := mustParseWith(t, New(DefaultLimits()), raw)
	var got []string
	for _, a := range msg.Attachments {
		got = append(got, a.PartNumber+" "+a.ContentType+" "+a.Filename)
	}
	want := []string{
		"1.3 text/calendar ",
		"2 text/calendar invite.ics",
		"3 text/csv report.csv",
		"4 text/plain notes.txt",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("attachments:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, s := range []string{"plain body", "notes"} {
		if !strings.Contains(msg.TextBody, s) {
			t.Errorf("text projection lost %q: %q", s, msg.TextBody)
		}
	}
	if !strings.Contains(msg.HTMLBody, "html body") {
		t.Errorf("HTML projection: %q", msg.HTMLBody)
	}
}
