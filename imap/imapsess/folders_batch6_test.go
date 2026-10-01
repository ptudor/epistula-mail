package imapsess

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func hasFolder(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestCreateCanonicalizesInbox is the RA6X-009 regression for CREATE.
//
// CREATE validated the name but did not canonicalize it, while SELECT, APPEND
// and LIST all do — so `CREATE inbox` inserted a literal "inbox" row that no
// command could ever open again. The user saw a folder in LIST and got
// NONEXISTENT every time they clicked it.
func TestCreateCanonicalizesInbox(t *testing.T) {
	sess, _ := appendFixture(t)

	if err := sess.Create("inbox", nil); err != nil {
		t.Fatalf("CREATE inbox: %v", err)
	}
	names := folderNames(t, sess)
	if hasFolder(names, "inbox") {
		t.Errorf("CREATE stored the literal name %q: %v", "inbox", names)
	}
	if !hasFolder(names, "INBOX") {
		t.Fatalf("CREATE inbox did not create INBOX: %v", names)
	}
	// And the folder it made is the one SELECT reaches.
	if _, err := sess.Select("inbox", &imap.SelectOptions{ReadOnly: true}); err != nil {
		t.Errorf("SELECT inbox after CREATE inbox: %v", err)
	}
	// A second CREATE in either spelling is the same folder, so it clashes.
	if err := sess.Create("INBOX", nil); err == nil {
		t.Error("CREATE INBOX after CREATE inbox was allowed; they are one folder")
	}
}

// TestCopyDestinationGoesThroughTheSameGate is the RA6X-009 regression for the
// auto-created COPY/MOVE destination, which validated and canonicalized
// nothing: a name CREATE refuses walked into the store through a drag-and-drop,
// and `COPY 1 inbox` made a second literal "inbox" beside the real one.
func TestCopyDestinationGoesThroughTheSameGate(t *testing.T) {
	sess := mutationFixture(t)
	set := imap.UIDSetNum(1)

	// A name CREATE refuses is refused here too.
	for name, why := range map[string]string{
		"Bad\x00Name":            "NUL",
		"Bad\nName":              "newline",
		strings.Repeat("x", 256): "over the byte limit",
		strings.Repeat("é", 200): "over the byte limit in multibyte",
	} {
		if _, err := sess.Copy(set, name); err == nil {
			t.Errorf("COPY auto-created a folder whose name CREATE refuses (%s)", why)
		} else if err := sess.Create(name, nil); err == nil {
			t.Errorf("CREATE accepted %s, so this test proves nothing", why)
		}
	}

	// And the destination is canonicalized, so a COPY to "inbox" lands in the
	// real INBOX rather than making a shadow of it.
	if _, err := sess.Copy(set, "inbox"); err != nil {
		t.Fatalf("COPY to inbox: %v", err)
	}
	names := folderNames(t, sess)
	if hasFolder(names, "inbox") {
		t.Errorf("COPY created a literal %q beside INBOX: %v", "inbox", names)
	}
}

// TestCopyAutoCreatesAncestors pins the hierarchy half of RA6X-009: an
// auto-created `Archive/2026/Q1` must not leave its parents missing from LIST.
func TestCopyAutoCreatesAncestors(t *testing.T) {
	sess := mutationFixture(t)
	if _, err := sess.Copy(imap.UIDSetNum(1), "Archive/2026/Q1"); err != nil {
		t.Fatalf("COPY: %v", err)
	}
	names := folderNames(t, sess)
	for _, want := range []string{"Archive", "Archive/2026", "Archive/2026/Q1"} {
		if !hasFolder(names, want) {
			t.Errorf("auto-create left %q missing: %v", want, names)
		}
	}
}

// TestRenameRefusesItselfAndItsOwnSubtree is the RA6X-009 regression for
// RENAME. `RENAME A A/B` renamed the parent to `A/B` and then ran the
// descendant UPDATE, whose `name LIKE 'A/%'` predicate matched the
// just-renamed parent and processed it a second time.
func TestRenameRefusesItselfAndItsOwnSubtree(t *testing.T) {
	sess, _ := appendFixture(t)
	if err := sess.Create("Archive", nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if err := sess.Create("Archive/2026", nil); err != nil {
		t.Fatalf("CREATE child: %v", err)
	}

	if err := sess.Rename("Archive", "Archive", nil); err == nil {
		t.Error("RENAME onto itself was allowed")
	}
	if err := sess.Rename("Archive", "Archive/Sub", nil); err == nil {
		t.Error("RENAME into its own subtree was allowed")
	}
	// Nothing moved.
	names := folderNames(t, sess)
	for _, want := range []string{"Archive", "Archive/2026"} {
		if !hasFolder(names, want) {
			t.Errorf("a refused RENAME still changed the tree: %v", names)
		}
	}

	// A rename whose destination is illegal is refused before anything moves.
	if err := sess.Rename("Archive", "Bad\nName", nil); err == nil {
		t.Error("RENAME to a name with a control character was allowed")
	}
	// A rename that would push a DESCENDANT past the byte limit is refused
	// even though the destination itself fits: 252 bytes is legal, but the
	// child gains "/2026" on top of it and lands at 257.
	long := strings.Repeat("y", 252)
	if err := sess.Rename("Archive", long, nil); err == nil {
		t.Errorf("RENAME was allowed although it lengthens %q past the limit",
			"Archive/2026")
	}
	if names := folderNames(t, sess); !hasFolder(names, "Archive/2026") {
		t.Errorf("a refused RENAME still moved a descendant: %v", names)
	}
}

// TestRenameSlicesTheSuffixByCharacters is the RA6X-007 regression.
//
// The descendant re-prefix bound substring(name from N) with Go's len() of the
// old name — UTF-8 BYTES — while PostgreSQL counts CHARACTERS. For a parent
// named `é` the bound was 3 where the correct position is 2, so renaming `é`
// to `B` produced `BChild` instead of `B/Child`: the separator was eaten and
// two distinct folders silently merged into one name.
func TestRenameSlicesTheSuffixByCharacters(t *testing.T) {
	sess, _ := appendFixture(t)

	// A parent whose name is one 2-byte character, and a deeper multibyte one.
	for _, tc := range []struct {
		parent, child, to, want string
	}{
		{"é", "é/Child", "B", "B/Child"},
		{"Ünïcødé", "Ünïcødé/2026/Q1", "Plain", "Plain/2026/Q1"},
		{"日本語", "日本語/受信", "Mail", "Mail/受信"},
		{"ascii", "ascii/kid", "moved", "moved/kid"},
	} {
		t.Run(tc.parent, func(t *testing.T) {
			if err := sess.Create(tc.parent, nil); err != nil {
				t.Fatalf("CREATE %q: %v", tc.parent, err)
			}
			// Create the intermediate levels the child needs.
			parts := strings.Split(tc.child, "/")
			for i := 2; i <= len(parts); i++ {
				n := strings.Join(parts[:i], "/")
				if err := sess.Create(n, nil); err != nil {
					t.Fatalf("CREATE %q: %v", n, err)
				}
			}
			if err := sess.Rename(tc.parent, tc.to, nil); err != nil {
				t.Fatalf("RENAME %q -> %q: %v", tc.parent, tc.to, err)
			}
			names := folderNames(t, sess)
			if !hasFolder(names, tc.want) {
				t.Errorf("renaming %q to %q produced %v, want %q present",
					tc.parent, tc.to, names, tc.want)
			}
			// The mangled form the byte bound produced must not appear.
			mangled := tc.to + strings.TrimPrefix(tc.child, tc.parent+"/")
			if hasFolder(names, mangled) {
				t.Errorf("descendant lost its separator: %q is present in %v", mangled, names)
			}
		})
	}
}

// TestStoreCanClearEveryFlag is the RA6X-004 regression.
//
// `STORE 1 FLAGS ()` is a valid replacement set meaning "this message now has
// no flags", but an early `len(flags.Flags) == 0` return fired before the op
// was examined — so a client could never clear all flags in one operation,
// including \Deleted, and the server acknowledged the command as if it had.
func TestStoreCanClearEveryFlag(t *testing.T) {
	sess := mutationFixture(t)

	// uid 2 starts \Flagged, uid 3 starts \Deleted.
	if got := messageFlags(t, sess, 2); len(got) == 0 {
		t.Fatalf("fixture uid 2 has no flags to clear")
	}

	if err := sess.Store(nil, imap.UIDSetNum(2), &imap.StoreFlags{
		Op: imap.StoreFlagsSet, Flags: nil,
	}, nil); err != nil {
		t.Fatalf("STORE FLAGS (): %v", err)
	}
	if got := messageFlags(t, sess, 2); len(got) != 0 {
		t.Errorf("STORE FLAGS () left flags %v", got)
	}

	// Clearing \Deleted this way is the case that matters: the client believes
	// the message is no longer queued for expunge.
	if err := sess.Store(nil, imap.UIDSetNum(3), &imap.StoreFlags{
		Op: imap.StoreFlagsSet, Flags: []imap.Flag{},
	}, nil); err != nil {
		t.Fatalf("STORE FLAGS () on the deleted message: %v", err)
	}
	if got := messageFlags(t, sess, 3); len(got) != 0 {
		t.Errorf("STORE FLAGS () left \\Deleted in place: %v", got)
	}

	// An empty ADD or REMOVE genuinely is a no-op and must not touch the row.
	if err := sess.Store(nil, imap.UIDSetNum(1), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen},
	}, nil); err != nil {
		t.Fatalf("STORE +FLAGS: %v", err)
	}
	for _, op := range []imap.StoreFlagsOp{imap.StoreFlagsAdd, imap.StoreFlagsDel} {
		if err := sess.Store(nil, imap.UIDSetNum(1), &imap.StoreFlags{
			Op: op, Flags: nil,
		}, nil); err != nil {
			t.Fatalf("empty op %v: %v", op, err)
		}
		if got := messageFlags(t, sess, 1); len(got) != 1 || got[0] != `\Seen` {
			t.Errorf("empty op %v changed the flags: %v", op, got)
		}
	}
}

// TestPermanentFlagsMatchWhatStorePersists is the RA6X-064 regression.
//
// SELECT and EXAMINE both advertised the five system flags and nothing else,
// while STORE persists arbitrary keywords durably. A conforming client reads
// PERMANENTFLAGS to decide what it may set, so it either disabled its tag
// controls or treated a keyword it did set as session-only — and EXAMINE
// advertised a writable set for a selection that permits no changes at all.
func TestPermanentFlagsMatchWhatStorePersists(t *testing.T) {
	sess := mutationFixture(t)

	// A durable keyword on a message in this folder. $Junk is well-known, so
	// its stored spelling is the conventional one (RA6X-028) and this test can
	// name it directly rather than restating the canonicalization rule.
	if err := sess.Store(nil, imap.UIDSetNum(1), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Flags: []imap.Flag{"$Junk"},
	}, nil); err != nil {
		t.Fatalf("STORE keyword: %v", err)
	}

	writable, err := sess.Select("INBOX", &imap.SelectOptions{})
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if !containsFlag(writable.PermanentFlags, imap.FlagWildcard) {
		t.Errorf("a writable SELECT does not advertise \\*, but STORE creates new keywords: %v",
			writable.PermanentFlags)
	}
	for _, f := range standardFlags() {
		if !containsFlag(writable.PermanentFlags, f) {
			t.Errorf("PERMANENTFLAGS is missing the system flag %q: %v", f, writable.PermanentFlags)
		}
	}
	// FLAGS reports the vocabulary actually in use, keyword included.
	if !containsFlag(writable.Flags, "$Junk") {
		t.Errorf("FLAGS omits a keyword the folder holds: %v", writable.Flags)
	}

	ro, err := sess.Select("INBOX", &imap.SelectOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("EXAMINE: %v", err)
	}
	if len(ro.PermanentFlags) != 0 {
		t.Errorf("EXAMINE advertises PERMANENTFLAGS %v; a read-only selection permits no permanent change",
			ro.PermanentFlags)
	}
}

func containsFlag(fs []imap.Flag, want imap.Flag) bool {
	for _, f := range fs {
		if f == want {
			return true
		}
	}
	return false
}
