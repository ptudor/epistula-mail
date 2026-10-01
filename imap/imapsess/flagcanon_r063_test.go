package imapsess

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestFlagSliceCanonicalizesSystemFlags is the R-063 regression: client-cased
// EVERY flag is persisted in one canonical spelling so epistula-api's byte-exact
// flag= filter agrees with epistula-imap's case-insensitive comparison.
//
// This originally asserted that keywords pass through untouched, on the
// understanding that they are case-sensitive. Flag names are case-insensitive —
// all of them (RFC 9051 §2.3.2, RFC 9007 §1.2) — so keywords now fold too:
// well-known ones keep their conventional capitalisation, anything else is
// lowercased (RA6X-028).
func TestFlagSliceCanonicalizesSystemFlags(t *testing.T) {
	in := []imap.Flag{
		imap.Flag(`\SEEN`),
		imap.Flag(`\answered`),
		imap.Flag(`\FlAgGeD`),
		imap.Flag(`\DELETED`),
		imap.Flag(`\draft`),
		imap.Flag(`$JUNK`),     // well-known keyword — conventional spelling
		imap.Flag(`$Label1`),   // other keyword — lowercased identity
		imap.Flag(`NonSystem`), // other keyword — lowercased identity
	}
	got := flagSliceToStrings(in)
	want := []string{`\Seen`, `\Answered`, `\Flagged`, `\Deleted`, `\Draft`, `$Junk`, `$label1`, `nonsystem`}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("flag[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestFlagsFromOptionsCanonicalizesSystemFlags: APPEND's flag path must go
// through the same canonicalizing seam as STORE/COPY — `APPEND ... (\SEEN)`
// previously persisted `\SEEN` verbatim, the residual R-063 gap.
func TestFlagsFromOptionsCanonicalizesSystemFlags(t *testing.T) {
	got := flagsFromOptions(&imap.AppendOptions{Flags: []imap.Flag{
		imap.Flag(`\SEEN`), imap.Flag(`\deleted`), imap.Flag(`$Label1`), imap.Flag(`$junk`),
	}})
	want := []string{`\Seen`, `\Deleted`, `$label1`, `$Junk`}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("flag[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if flagsFromOptions(nil) != nil {
		t.Error("flagsFromOptions(nil) should stay nil")
	}
	if flagsFromOptions(&imap.AppendOptions{}) != nil {
		t.Error("flagsFromOptions(no flags) should stay nil")
	}
}
