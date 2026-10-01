package imapsess

import (
	"bytes"
	"net/mail"
	"strings"

	"github.com/emersion/go-imap/v2"
)

// parseGroupedAddresses retains group delimiters discarded by net/mail. The
// standard parser validates the full list and parses each phrase/member; this
// scanner only locates delimiters outside comments, quotes, literals and routes.
func parseGroupedAddresses(s string) ([]imap.Address, error) {
	if _, err := mail.ParseAddressList(s); err != nil {
		return nil, err
	}
	var out []imap.Address
	appendMembers := func(raw string) error {
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		members, err := mail.ParseAddressList(raw)
		if err != nil {
			return err
		}
		for _, a := range members {
			out = append(out, toIMAPAddress(a))
		}
		return nil
	}
	start, comments, angles, brackets := 0, 0, 0, 0
	quoted, escaped, group := false, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && (quoted || comments > 0 || brackets > 0) {
			escaped = true
			continue
		}
		if comments > 0 {
			if c == '(' {
				comments++
			}
			if c == ')' {
				comments--
			}
			continue
		}
		if quoted {
			if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
			continue
		}
		if c == '(' {
			comments++
			continue
		}
		if c == '<' {
			angles++
			continue
		}
		if c == '>' {
			angles--
			continue
		}
		if c == '[' {
			brackets++
			continue
		}
		if c == ']' {
			brackets--
			continue
		}
		if angles > 0 || brackets > 0 {
			continue
		}
		// Encoded-word punctuation belongs to a phrase, never to the list.
		if strings.HasPrefix(s[i:], "=?") {
			if end := strings.Index(s[i+2:], "?="); end >= 0 {
				i += end + 3
				continue
			}
		}
		switch c {
		case ':':
			name, err := mail.ParseAddress(strings.TrimSpace(s[start:i]) + " <group@example.invalid>")
			if err != nil {
				return nil, err
			}
			out = append(out, imap.Address{Mailbox: name.Name})
			start, group = i+1, true
		case ';':
			if err := appendMembers(s[start:i]); err != nil {
				return nil, err
			}
			out = append(out, imap.Address{})
			start, group = i+1, false
		case ',':
			if !group {
				if err := appendMembers(s[start:i]); err != nil {
					return nil, err
				}
				start = i + 1
			}
		}
	}
	if err := appendMembers(s[start:]); err != nil {
		return nil, err
	}
	return out, nil
}

// Decode original address syntax only during address parsing. Decoding encoded
// words into a stored header first can turn a display-name comma into a list
// delimiter; original raw headers remain the authority for wire envelopes.
func buildRawEnvelope(r fetchRow, raw []byte) (*imap.Envelope, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	r.sentDate = nil
	if date, err := mail.ParseDate(m.Header.Get("Date")); err == nil {
		r.sentDate = &date
	}
	return buildEnvelopeHeaders(r, map[string][]string(m.Header)), nil
}
