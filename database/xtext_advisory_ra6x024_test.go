package main

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/recipients"
)

// invalidUTF8Fixtures are the shapes GO-2026-5970 / CVE-2026-56852 concerns:
// invalid UTF-8 reaching x/text's normalizer, where the pre-v0.39.0 code could
// spin forever. Each is byte sequences a remote sender fully controls — an
// envelope recipient and a charset-labelled MIME body.
var invalidUTF8Fixtures = map[string]string{
	"lone continuation byte":     "\x80",
	"truncated 2-byte sequence":  "\xc3",
	"truncated 3-byte sequence":  "\xe2\x82",
	"truncated 4-byte sequence":  "\xf0\x9f\x92",
	"overlong encoding":          "\xc0\xaf",
	"surrogate half":             "\xed\xa0\x80",
	"invalid byte after starter": "a\xffb",
	// A combining mark preceded by an invalid byte exercises the normalizer's
	// buffered-reorder path rather than its ASCII fast path.
	"invalid byte before combining marks": "\xff̧́",
	"long run of invalid bytes":           strings.Repeat("\xed\xa0\x80", 4096),
}

// TestNormalizationTerminatesOnInvalidUTF8 is the RA6X-024 regression: the
// normalizer must terminate on every invalid-UTF-8 input, since a synchronous
// normalization loop is not interrupted by a request context deadline. Two
// remote-controlled call sites reach it — recipients.SplitAddress (the
// envelope address the LDA is handed) and the ingest charset decoder — and
// the test also calls norm.NFC directly.
//
// The bound is deliberately generous: this asserts termination, not
// performance. An unfixed x/text does not merely run slowly here, it does not
// return.
func TestNormalizationTerminatesOnInvalidUTF8(t *testing.T) {
	for name, input := range invalidUTF8Fixtures {
		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)

				// 1. The normalizer itself, in both the string and byte forms
				//    the library exposes.
				_ = norm.NFC.String(input)
				_ = norm.NFC.Bytes([]byte(input))
				_ = norm.NFD.String(input)
				_ = norm.NFC.IsNormalString(input)

				// 2. The envelope-address path. The address is lowercased
				//    first, which may or may not remove the precondition
				//    depending on the bytes; either way it must return.
				_, _, _ = recipients.SplitAddress(input + "@example.invalid")
				_, _, _ = recipients.SplitAddress("user@" + input)

				// 3. The full ingest path for a body labelled with a charset
				//    whose decoder feeds a transform chain, and for a subject
				//    carrying the same bytes.
				parser := ingest.New(ingest.Limits{
					MaxMessageBytes:       10 << 20,
					MaxMimeDepth:          10,
					MaxMimeParts:          200,
					MaxHeaderBytes:        16 << 10,
					MaxHeaderSectionBytes: 256 << 10,
					MaxTransferExpansion:  10,
				})
				for _, cs := range []string{"iso-8859-1", "windows-1252", "utf-8", "shift_jis"} {
					raw := "From: s@example.invalid\r\n" +
						"Subject: " + input + "\r\n" +
						"Content-Type: text/plain; charset=" + cs + "\r\n\r\n" +
						input + "\r\n"
					_, _ = parser.Parse([]byte(raw))
				}
			}()

			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("normalization did not terminate on invalid UTF-8 " +
					"(GO-2026-5970); check the resolved golang.org/x/text version")
			}
		})
	}
}
