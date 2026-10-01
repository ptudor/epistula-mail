package ingest

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// OPS-004: the part numbers ingest records below message/rfc822, in
// attachment rows and defect labels, follow RFC 9051 §6.4.5. A message's parts
// are its multipart body's children, or else its body alone as part 1. The
// parts of the message a message/rfc822 part encloses are numbered under that
// part's number. The IMAP reader resolves the same numbers
// (imapsess/rfc822chain_ops004_test.go checks the two against each other).
//
// The top-level body used to be walked as "", as if it were a message/rfc822
// part numbered "". A message whose own Content-Type is message/rfc822 then
// had its enclosed message's parts numbered 1, 2, and so on instead of 1.1,
// 1.2. Forwards nested inside that message were each one level short too.

// ops004Direct makes inner the whole body of a message: a forward with no
// multipart around it.
func ops004Direct(inner string) string {
	return "From: direct@ops004.invalid\r\nContent-Type: message/rfc822\r\n\r\n" + inner
}

// ops004Wrapped forwards inner the way mail clients do, as the second part of a
// multipart/mixed. Each wrapper needs its own boundary.
func ops004Wrapped(boundary, inner string) string {
	return "From: wrapped@ops004.invalid\r\nContent-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n" +
		"--" + boundary + "\r\nContent-Type: text/plain\r\n\r\nsee below\r\n" +
		"--" + boundary + "\r\nContent-Type: message/rfc822\r\n\r\n" + inner + "\r\n--" + boundary + "--\r\n"
}

// ops004Attached is a message whose second part is an attachment.
var ops004Attached = "From: inner@ops004.invalid\r\nContent-Type: multipart/mixed; boundary=att\r\n\r\n" +
	"--att\r\nContent-Type: text/plain\r\n\r\ntext\r\n" +
	"--att\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n" +
	"Content-Disposition: attachment; filename=\"a.bin\"\r\n\r\n" +
	base64.StdEncoding.EncodeToString([]byte("attachment bytes")) + "\r\n--att--\r\n"

func TestEnclosedAttachmentPartNumbers(t *testing.T) {
	singlePartAttachment := "From: inner@ops004.invalid\r\nContent-Type: application/octet-stream\r\n\r\nbytes"
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"top-level single part", singlePartAttachment, "1"},
		{"top-level multipart", ops004Attached, "2"},
		{"direct", ops004Direct(ops004Attached), "1.2"},
		{"direct, single-part enclosed", ops004Direct(singlePartAttachment), "1.1"},
		{"wrapped", ops004Wrapped("w0", ops004Attached), "2.2"},
		{"wrapped, single-part enclosed", ops004Wrapped("w0", singlePartAttachment), "2.1"},
		{"direct+direct", ops004Direct(ops004Direct(ops004Attached)), "1.1.2"},
		{"wrapped+direct", ops004Wrapped("w0", ops004Direct(ops004Attached)), "2.1.2"},
		{"direct+wrapped", ops004Direct(ops004Wrapped("w1", ops004Attached)), "1.2.2"},
		{"wrapped+wrapped", ops004Wrapped("w0", ops004Wrapped("w1", ops004Attached)), "2.2.2"},
		{"direct+direct+direct", ops004Direct(ops004Direct(ops004Direct(ops004Attached))), "1.1.1.2"},
		{"wrapped+direct+wrapped", ops004Wrapped("w0", ops004Direct(ops004Wrapped("w2", ops004Attached))), "2.1.2.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := mustParse(t, []byte(tc.raw))
			if len(msg.Attachments) != 1 {
				t.Fatalf("attachments = %+v, want one", msg.Attachments)
			}
			if got := msg.Attachments[0].PartNumber; got != tc.want {
				t.Errorf("attachment part number = %q, want %q", got, tc.want)
			}
			if paths := structurePaths(msg.BodyStructure); !paths[tc.want] {
				t.Errorf("part %s is not in the structure %v", tc.want, paths)
			}
		})
	}
}

// TestEnclosedDefectLabels: defect labels use the same numbers. A defect of
// the enclosed message's multipart body belongs to the message/rfc822 part
// whose number that body shares, not to the top-level message.
func TestEnclosedDefectLabels(t *testing.T) {
	unclosed := "From: inner@ops004.invalid\r\nContent-Type: multipart/mixed; boundary=u\r\n\r\n" +
		"--u\r\nContent-Type: text/plain\r\n\r\nfirst\r\n" +
		"--u\r\nContent-Type: application/pdf\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"JVBERi0xLjQK!!!\r\n"
	// Each defect is "<label>: <kind>[: <detail>]"; the detail of an
	// undecodable body is the decoder's own error text, so only the label and
	// the kind are compared. A node's own defects come before its children's.
	for _, tc := range []struct {
		name, raw string
		want      []string
	}{
		{"direct", ops004Direct(unclosed), []string{
			"part 1: " + DefectMissingCloseDelimiter,
			"part 1.2: " + DefectUndecodableBody + ": base64: ",
		}},
		{"direct+direct", ops004Direct(ops004Direct(unclosed)), []string{
			"part 1.1: " + DefectMissingCloseDelimiter,
			"part 1.1.2: " + DefectUndecodableBody + ": base64: ",
		}},
		{"wrapped+direct", ops004Wrapped("w0", ops004Direct(unclosed)), []string{
			"part 2.1: " + DefectMissingCloseDelimiter,
			"part 2.1.2: " + DefectUndecodableBody + ": base64: ",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := mustParse(t, []byte(tc.raw))
			if len(msg.Defects) != len(tc.want) {
				t.Fatalf("Defects = %q, want %d starting %q", msg.Defects, len(tc.want), tc.want)
			}
			for i, want := range tc.want {
				if !strings.HasPrefix(msg.Defects[i], want) {
					t.Errorf("defect %d = %q, want it to start %q", i, msg.Defects[i], want)
				}
			}
			if len(msg.Attachments) != 0 {
				t.Errorf("an undecodable body produced attachments %+v", msg.Attachments)
			}
		})
	}
}

// TestBareMultipartEnclosedIsNumberedAsMultipart: a Content-Type of
// "multipart" with no subtype is walked as a multipart, so an enclosed
// message of that type shares its number with the message/rfc822 part, as
// BODYSTRUCTURE shows it. The numbering used to test for a "multipart/" prefix
// while the walk tested the main type, so the attachment row said 1.1.2 for a
// part the structure places at 1.2.
func TestBareMultipartEnclosedIsNumberedAsMultipart(t *testing.T) {
	bare := "From: inner@ops004.invalid\r\nContent-Type: multipart; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\ntext\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\n\r\nbytes\r\n--b--\r\n"
	msg := mustParse(t, []byte(ops004Direct(bare)))
	if len(msg.Attachments) != 1 || msg.Attachments[0].PartNumber != "1.2" {
		t.Fatalf("attachments = %+v, want one at 1.2", msg.Attachments)
	}
	if paths := structurePaths(msg.BodyStructure); !paths["1.2"] || paths["1.1.2"] {
		t.Errorf("structure paths = %v", paths)
	}
	if got := fmt.Sprint(IsMultipart("multipart"), IsMultipart("multipart/mixed"), IsMultipart("message/rfc822")); got != "true true false" {
		t.Errorf("IsMultipart = %s", got)
	}
}
