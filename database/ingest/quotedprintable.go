package ingest

import "bytes"

// decodeQuotedPrintable decodes a quoted-printable body (OPS-003).
//
// It is mime/quotedprintable.Reader's algorithm with two differences, both
// in the direction mail clients take:
//
//   - No line-length limit. The stdlib reader reads each line into a
//     default-sized bufio.Reader, so every encoded line longer than 4096
//     bytes failed with "bufio: buffer full". That is a limitation of the
//     reader, not a property of the mail: RFC 2045 asks encoders for 76
//     characters, but bulk and HTML mail routinely sends one long line, and
//     every client decodes it.
//   - Nothing is refused. Where the stdlib reader returns an error (a control
//     byte, an '=' before a line break or at the end, an '=' not followed by
//     two hex digits there), the bytes pass through literally, as clients
//     show them.
//
// For every input the stdlib reader decodes without error, the output is
// byte for byte the same, which the tests pin against the stdlib itself, so
// no message that parsed before decodes differently now. The output is
// never longer than the input.
func decodeQuotedPrintable(body []byte) []byte {
	out := make([]byte, 0, len(body))
	for len(body) > 0 {
		line := body
		body = nil
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, body = line[:i+1], line[i+1:]
		}
		hasLF := bytes.HasSuffix(line, []byte("\n"))
		hasCRLF := bytes.HasSuffix(line, []byte("\r\n"))

		// RFC 2045 §6.7 (3): trailing whitespace was added in transport and
		// is deleted; a trailing '=' is a soft line break, which joins this
		// line to the next one.
		text := bytes.TrimRight(line, " \t\r\n")
		soft := bytes.HasSuffix(text, []byte("="))
		if soft {
			text = text[:len(text)-1]
		}

		for i := 0; i < len(text); i++ {
			c := text[i]
			if c == '=' && i+2 < len(text) && isHexDigit(text[i+1]) && isHexDigit(text[i+2]) {
				out = append(out, unhex(text[i+1])<<4|unhex(text[i+2]))
				i += 2
				continue
			}
			out = append(out, c)
		}

		switch {
		case soft:
		case hasCRLF:
			out = append(out, '\r', '\n')
		case hasLF:
			out = append(out, '\n')
		}
	}
	return out
}

// isHexDigit accepts both cases, as the stdlib reader does ("badly encoded"
// lower case is common).
func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'A' <= c && c <= 'F' || 'a' <= c && c <= 'f'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	default:
		return c - '0'
	}
}
