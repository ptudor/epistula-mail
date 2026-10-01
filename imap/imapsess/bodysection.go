// Body-section slicing for FETCH BODY[HEADER], BODY[TEXT], and
// BODY[HEADER.FIELDS (...)]. Per-part addressing (BODY[N], BODY[N.HEADER])
// is a separate file because it needs MIME tree walking that the
// header/text split below does not.
package imapsess

import (
	"github.com/ptudor/epistula-mail/database/ingest"
)

// splitHeaderBody returns (headerSection, bodySection) of an RFC 5322 message.
// headerSection INCLUDES the blank-line separator per IMAP semantics ("the
// [RFC 5322] header of the message" — the body begins immediately after).
//
// It delegates to ingest.SplitHeaderBody so the reader and the writer split at
// the same place (RA6X-006). This used to prefer the first CRLF CRLF anywhere
// in the message and only fall back to LF LF, while ingest was corrected to
// take whichever separator comes EARLIER. Postfix's pipe transport hands the
// LDA LF-terminated mail, so for a message whose body contains a CRLF blank
// line the two disagreed: ingest ended the headers at the real boundary while
// FETCH placed it deep in the body, turning body text into headers and
// truncating BODY[TEXT].
func splitHeaderBody(raw []byte) (header, body []byte) {
	return ingest.SplitHeaderBody(raw)
}

// filterHeaderFields is retained only for the tests that pin the previous
// canonicalising behaviour of BODY[HEADER.FIELDS]. The FETCH path uses
// selectRawFields, which copies the sender's own bytes (RA6X-027).
func filterHeaderFields(headerSection []byte, names []string, invert bool) []byte {
	return selectRawFields(headerSection, names, invert)
}
