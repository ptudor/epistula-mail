package imapsess

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// folderSpecialUse reads the persisted attribute for one folder.
func folderSpecialUse(t *testing.T, sess *Session, name string) *string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var su *string
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT special_use FROM folders WHERE mailbox_id = $1 AND name = $2`,
		sess.mailboxID, name,
	).Scan(&su); err != nil {
		t.Fatalf("read special_use for %q: %v", name, err)
	}
	return su
}

// TestCreatePersistsSpecialUse is the RO5X-007/RO5X-043 regression on the
// storage side: the attribute a client sets at CREATE must reach
// folders.special_use, which is the column LIST reports from.
func TestCreatePersistsSpecialUse(t *testing.T) {
	sess := mutationFixture(t)

	if err := sess.Create("Archive", &imap.CreateOptions{
		SpecialUse: []imap.MailboxAttr{imap.MailboxAttrArchive},
	}); err != nil {
		t.Fatalf("Create with special-use: %v", err)
	}
	got := folderSpecialUse(t, sess, "Archive")
	if got == nil {
		t.Fatal(`special_use is NULL; the client's \Archive attribute was dropped`)
	}
	if *got != `\Archive` {
		t.Errorf(`special_use = %q, want \Archive`, *got)
	}

	// LIST must now report it.
	rows := listForTest(t, sess, false)
	var found bool
	for _, r := range rows {
		if r.name == "Archive" {
			found = true
			if r.specialUse == nil || *r.specialUse != `\Archive` {
				t.Errorf("LIST special_use = %v, want \\Archive", r.specialUse)
			}
		}
	}
	if !found {
		t.Error("Archive missing from LIST output")
	}
}

// TestCreateWithoutSpecialUseLeavesNull keeps ordinary CREATE unchanged.
func TestCreateWithoutSpecialUseLeavesNull(t *testing.T) {
	sess := mutationFixture(t)

	for _, opts := range []*imap.CreateOptions{nil, {}} {
		name := "Plain"
		if opts != nil {
			name = "PlainOpts"
		}
		if err := sess.Create(name, opts); err != nil {
			t.Fatalf("Create(%q): %v", name, err)
		}
		if got := folderSpecialUse(t, sess, name); got != nil {
			t.Errorf("special_use for %q = %q, want NULL", name, *got)
		}
	}
}

// TestCreateRejectsUnsupportedSpecialUse proves an attribute the schema does
// not model is refused rather than stored — otherwise LIST could emit an
// attribute no client asked for.
func TestCreateRejectsUnsupportedSpecialUse(t *testing.T) {
	sess := mutationFixture(t)

	err := sess.Create("Weird", &imap.CreateOptions{
		SpecialUse: []imap.MailboxAttr{imap.MailboxAttr(`\NotAThing`)},
	})
	if err == nil {
		t.Fatal("Create accepted an unsupported special-use attribute")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeNo {
		t.Errorf("err = %v, want a NO imap.Error", err)
	}
	assertFolderAbsent(t, sess, "Weird")
}

// TestCreateRejectsMultipleSpecialUse covers the "column holds one" boundary.
func TestCreateRejectsMultipleSpecialUse(t *testing.T) {
	sess := mutationFixture(t)

	err := sess.Create("Two", &imap.CreateOptions{
		SpecialUse: []imap.MailboxAttr{imap.MailboxAttrSent, imap.MailboxAttrDrafts},
	})
	if err == nil {
		t.Fatal("Create accepted two special-use attributes; the column holds one")
	}
	assertFolderAbsent(t, sess, "Two")
}

// TestCreateAcceptsEveryModelledAttribute walks the schema's documented set.
func TestCreateAcceptsEveryModelledAttribute(t *testing.T) {
	sess := mutationFixture(t)

	for i, attr := range []imap.MailboxAttr{
		imap.MailboxAttrSent, imap.MailboxAttrDrafts, imap.MailboxAttrTrash,
		imap.MailboxAttrJunk, imap.MailboxAttrArchive, imap.MailboxAttrImportant,
		imap.MailboxAttrFlagged, imap.MailboxAttrAll,
	} {
		name := "SU" + string(rune('a'+i))
		if err := sess.Create(name, &imap.CreateOptions{SpecialUse: []imap.MailboxAttr{attr}}); err != nil {
			t.Errorf("Create(%q, %v): %v", name, attr, err)
			continue
		}
		got := folderSpecialUse(t, sess, name)
		if got == nil || *got != string(attr) {
			t.Errorf("special_use for %q = %v, want %q", name, got, attr)
		}
	}
}

func assertFolderAbsent(t *testing.T, sess *Session, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM folders WHERE mailbox_id = $1 AND name = $2`,
		sess.mailboxID, name,
	).Scan(&n); err != nil {
		t.Fatalf("count folders: %v", err)
	}
	if n != 0 {
		t.Errorf("folder %q was created despite the rejected CREATE", name)
	}
}
