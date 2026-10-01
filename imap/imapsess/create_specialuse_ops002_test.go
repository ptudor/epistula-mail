package imapsess

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestCreateSpecialUseMatchesCaseInsensitively: CREATE now validates USE
// through epistula-database's storage.CanonicalSpecialUse, the list the admin CLI
// shares (OPS-002). RFC 6154 defines the attributes as ABNF literals, which
// are case-insensitive, so `\sent` is accepted and stored as `\Sent` — the one
// spelling LIST ever emits.
func TestCreateSpecialUseMatchesCaseInsensitively(t *testing.T) {
	sess := mutationFixture(t)

	if err := sess.Create("Outgoing", &imap.CreateOptions{
		SpecialUse: []imap.MailboxAttr{imap.MailboxAttr(`\sent`)},
	}); err != nil {
		t.Fatalf(`Create with \sent: %v`, err)
	}
	if got := folderSpecialUse(t, sess, "Outgoing"); got == nil || *got != `\Sent` {
		t.Fatalf(`special_use = %v, want \Sent`, got)
	}
}

// TestCreateSpecialUseAppliesToLeafOnly: ancestors created on the way to a
// special-use leaf are plain folders, as a CREATE of each would make them.
func TestCreateSpecialUseAppliesToLeafOnly(t *testing.T) {
	sess := mutationFixture(t)

	if err := sess.Create("Mail/Archive", &imap.CreateOptions{
		SpecialUse: []imap.MailboxAttr{imap.MailboxAttrArchive},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := folderSpecialUse(t, sess, "Mail"); got != nil {
		t.Errorf("ancestor Mail got special_use %q", *got)
	}
	if got := folderSpecialUse(t, sess, "Mail/Archive"); got == nil || *got != `\Archive` {
		t.Errorf(`leaf special_use = %v, want \Archive`, got)
	}
}

// TestListReportsOperatorAssignedSpecialUse closes the OPS-002 loop: a folder
// that was never CREATEd with USE — here, one an import would have made — gets
// its role from `epistula-database admin folder-set-special-use`, and LIST reports
// it from the same column.
func TestListReportsOperatorAssignedSpecialUse(t *testing.T) {
	sess := mutationFixture(t)
	mustCreateFolder(t, sess, "Sent Messages")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sess.be.Storage().SetFolderSpecialUse(ctx, sess.mailboxID, "Sent Messages", `\Sent`, false); err != nil {
		t.Fatalf("SetFolderSpecialUse: %v", err)
	}

	var found bool
	for _, r := range listForTest(t, sess, false) {
		if r.name != "Sent Messages" {
			continue
		}
		found = true
		if r.specialUse == nil || *r.specialUse != `\Sent` {
			t.Errorf(`LIST special_use = %v, want \Sent`, r.specialUse)
		}
	}
	if !found {
		t.Fatal("Sent Messages missing from LIST output")
	}
}
