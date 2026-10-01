// Package imapflags is the one canonical spelling of IMAP flags for every
// component that reads or writes messages.flags.
//
// It lives in epistula-database — the module that owns the schema — because
// `epistula-imap` and `epistula-api` both need it and `epistula-api` cannot import
// `imapsess` (it depends on epistula-database, not on epistula-imap). Keeping one
// copy is the point: R-063 canonicalized flags at the IMAP *write* seam
// precisely so byte-exact comparisons downstream would agree, but the read
// paths were never brought in line, so a second copy of the map would be a
// second opportunity to drift (RO5X-013).
//
// FLAG NAMES ARE CASE-INSENSITIVE — all of them, not only the five system
// flags (RFC 9051 §2.3.2, RFC 9007 §1.2). This package previously treated
// keywords as case-sensitive, so `$Junk` and `$junk` became two separate flags:
// a client could not reliably clear or find a keyword written in another
// spelling, and a message could carry both at once (RA6X-028).
//
// Identity is therefore case-folded for every flag. What varies is only the
// DISPLAY spelling that gets stored, because clients show keywords verbatim:
// the five system flags and the well-known keywords below keep their
// conventional capitalisation, and anything else is stored lowercased. That
// gives every flag exactly one stored spelling, which is what lets every
// downstream comparison stay a plain byte-exact array test instead of a
// case-folding predicate in a dozen queries.
package imapflags

import "strings"

// The five RFC 9051 §2.3.2 system flags, in their canonical spelling.
const (
	Seen     = `\Seen`
	Answered = `\Answered`
	Flagged  = `\Flagged`
	Deleted  = `\Deleted`
	Draft    = `\Draft`
)

// canonical maps the lowercased spelling of each system flag to its canonical
// form.
var systemCanonical = map[string]string{
	strings.ToLower(Seen):     Seen,
	strings.ToLower(Answered): Answered,
	strings.ToLower(Flagged):  Flagged,
	strings.ToLower(Deleted):  Deleted,
	strings.ToLower(Draft):    Draft,
}

// canonical is systemCanonical plus every spelling-preserving keyword. Kept
// separate from systemCanonical so IsSystem still answers "is this one of the
// five", which is a different question from "does this flag have a
// conventional spelling".
var canonical = map[string]string{}

// wellKnownKeywords are the keyword flags with an established conventional
// spelling, from the IANA IMAP keyword registry and long-standing client
// practice. They keep that spelling so a MUA that renders keywords verbatim
// shows `$Junk`, not `$junk`.
//
// A keyword outside this set is stored lowercased. That is a display choice,
// not an identity one: matching is case-insensitive either way.
var wellKnownKeywords = []string{
	`$Forwarded`,
	`$Junk`,
	`$NotJunk`,
	`$Phishing`,
	`$MDNSent`,
	`$Submitted`,
	`$Important`,
	`$Answered`,
	`$Flagged`,
	`$Redirected`,
	`$Unsubscribed`,
	`$Ignored`,
	`$Muted`,
	// Not settable by a client, but it appears in stored data and in FLAGS
	// responses, so it gets its conventional spelling too.
	`\Recent`,
}

func init() {
	for lower, c := range systemCanonical {
		canonical[lower] = c
	}
	for _, k := range wellKnownKeywords {
		canonical[strings.ToLower(k)] = k
	}
}

// Canonical returns the one stored spelling of a flag.
//
// A system flag or a well-known keyword in any case maps to its conventional
// form; any other keyword is lowercased. Flag names are case-insensitive
// (RFC 9051 §2.3.2), so this is a display normalisation, and it is what makes
// one flag one row value — the property every byte-exact comparison in the
// schema depends on (RA6X-028).
//
// The empty string is returned unchanged; callers validate separately.
func Canonical(s string) string {
	if s == "" {
		return s
	}
	if c, ok := canonical[strings.ToLower(s)]; ok {
		return c
	}
	// A keyword outside the well-known set. Lowercased so `$Label1` and
	// `$label1` are one flag, which is what the protocol says they are.
	return strings.ToLower(s)
}

// EqualFlag reports whether two flag names denote the same flag.
//
// Useful where a value cannot be canonicalized first — comparing what a client
// sent against what is already stored, for instance.
func EqualFlag(a, b string) bool { return Canonical(a) == Canonical(b) }

// IsSystem reports whether s names one of the five settable system flags, in
// any case. A well-known keyword is not a system flag.
func IsSystem(s string) bool {
	_, ok := systemCanonical[strings.ToLower(s)]
	return ok
}

// maxKeywordBytes bounds a keyword flag. RFC 9051's atom grammar has no
// explicit length, but a flag far beyond this is a client bug or an attempt to
// push an oversize value into a TEXT[] column.
const maxKeywordBytes = 64

// Valid reports whether s is usable as a flag filter: either a system flag, or
// a plausible keyword — non-empty, no whitespace or control characters, and
// within maxKeywordBytes.
//
// Used by epistula-api to reject a `flag=`/`not_flag=` value with a 422 rather
// than serving a silently empty page, which is what a consumer passing `seen`
// or `Seen` used to get.
func Valid(s string) bool {
	if s == "" || len(s) > maxKeywordBytes {
		return false
	}
	if IsSystem(s) {
		return true
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// CanonicalSlice returns a copy of in with every element canonicalized.
func CanonicalSlice(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, Canonical(s))
	}
	return out
}
