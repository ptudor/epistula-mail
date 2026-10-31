// The User-Agent field the send tool adds to a message.
//
// epistula-api relays a submission byte for byte, so the only place a message
// can say which software composed it is here, before the bytes leave the
// connector: on this path the connector is the mail user agent. The field is
// added only when the message does not already name its composer (User-Agent
// or X-Mailer) and carries no DKIM-Signature, which may oversign an absent
// field to forbid exactly this kind of addition. S/MIME and OpenPGP
// signatures cover the MIME entity, not the top-level header, so a field
// added above them still verifies.
package main

import (
	"bytes"
	"strings"
)

// composerFields are the header fields whose presence leaves a message
// untouched: it already says what composed it, or a signature may cover the
// absence of such a field. Lower case, compared case-insensitively.
var composerFields = []string{"user-agent", "x-mailer", "dkim-signature"}

// addMailUserAgent returns raw with a User-Agent field naming ua prepended to
// its header block, in the message's own line-ending convention, or raw
// itself when the header already has a field in composerFields.
func addMailUserAgent(raw []byte, ua string) []byte {
	if hasHeaderField(raw, composerFields) {
		return raw
	}
	eol := "\n"
	if i := bytes.IndexByte(raw, '\n'); i > 0 && raw[i-1] == '\r' {
		eol = "\r\n"
	}
	out := make([]byte, 0, len("User-Agent: ")+len(ua)+len(eol)+len(raw))
	out = append(out, "User-Agent: "...)
	out = append(out, ua...)
	out = append(out, eol...)
	return append(out, raw...)
}

// hasHeaderField reports whether the message's header block carries a field
// with one of the given lower-case names. It reads the header only: from the
// first byte to the first empty line, where a line starting with space or
// tab continues the field above it. A body line that looks like a field is
// never consulted.
func hasHeaderField(raw []byte, names []string) bool {
	rest := raw
	for len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			return false
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(string(line[:colon])))
		for _, want := range names {
			if name == want {
				return true
			}
		}
	}
	return false
}
