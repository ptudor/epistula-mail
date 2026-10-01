package ingest

import (
	"bytes"
	"fmt"
)

// VisitRawMultipart visits raw child spans without parsing or allocating their
// headers. The CRLF before each delimiter belongs to the delimiter. Preamble,
// epilogue, padding and bare LF are supported. Ingest and the IMAP reader both
// split with it, so a part is the same bytes to the writer and to BODY[n]
// (OPS-003). A body without a close delimiter ends its last part at EOF.
func VisitRawMultipart(body []byte, boundary string, visit func([]byte) error) error {
	_, err := visitRawMultipart(body, boundary, visit)
	return err
}

// visitRawMultipart is VisitRawMultipart that also reports whether the body
// had a close delimiter, which ingest records as a defect when it did not.
func visitRawMultipart(body []byte, boundary string, visit func([]byte) error) (closed bool, err error) {
	if boundary == "" {
		return false, fmt.Errorf("%w: multipart missing boundary", ErrMalformed)
	}
	delim := []byte("--" + boundary)

	// contentStart is where the current part's bytes begin; -1 while we are
	// still in the preamble.
	contentStart := -1

	i := 0
	for i <= len(body) {
		lineEnd := bytes.IndexByte(body[i:], '\n')
		var next int
		if lineEnd < 0 {
			lineEnd = len(body)
			next = len(body) + 1
		} else {
			lineEnd = i + lineEnd
			next = lineEnd + 1
		}
		line := bytes.TrimRight(body[i:lineEnd], "\r")

		if bytes.HasPrefix(line, delim) {
			rest := bytes.TrimRight(line[len(delim):], " \t")
			isClose := bytes.Equal(rest, []byte("--"))
			if isClose || len(rest) == 0 {
				if contentStart >= 0 {
					// The line break immediately before this delimiter is part
					// of the delimiter, not of the part.
					end := i
					if end > contentStart && body[end-1] == '\n' {
						end--
					}
					if end > contentStart && body[end-1] == '\r' {
						end--
					}
					if err := visit(body[contentStart:end]); err != nil {
						return false, err
					}
				}
				if isClose {
					return true, nil
				}
				contentStart = next
				i = next
				continue
			}
		}
		i = next
	}

	// No close delimiter: take whatever the last part had. Truncated multipart
	// mail is common enough that refusing it would lose readable content.
	if contentStart >= 0 && contentStart <= len(body) {
		if err := visit(body[contentStart:]); err != nil {
			return false, err
		}
	}
	return false, nil
}
