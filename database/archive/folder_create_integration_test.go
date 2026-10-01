package archive_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestCreateFolder covers the operator's IMAP CREATE, which archive sorting
// needs for a mailbox that has only ever received mail (only INBOX).
func TestCreateFolder(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := testContext(t)
	f := newFixture(t, ctx, db)
	mb := f.mailbox("creator")
	f.folder(mb, "INBOX", "")

	dry, err := db.CreateFolder(ctx, mb, "Archive/Finance", `\Archive`, true)
	if err != nil || strings.Join(dry.Created, ",") != "Archive,Archive/Finance" {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	if n := f.scalar(`SELECT count(*) FROM folders WHERE mailbox_id = $1`, mb); n != 1 {
		t.Fatal("the dry run created folders")
	}

	res, err := db.CreateFolder(ctx, mb, "Archive", `\archive`, false) // spelling canonicalized
	if err != nil || strings.Join(res.Created, ",") != "Archive" {
		t.Fatalf("create = %+v, %v", res, err)
	}
	if n := f.scalar(`SELECT count(*) FROM folders WHERE mailbox_id = $1 AND name = 'Archive' AND special_use = '\Archive'`, mb); n != 1 {
		t.Fatal("the new folder does not carry \\Archive")
	}
	res, err = db.CreateFolder(ctx, mb, "Deep/Er/Folder", "", false)
	if err != nil || strings.Join(res.Created, ",") != "Deep,Deep/Er,Deep/Er/Folder" {
		t.Fatalf("nested create = %+v, %v", res, err)
	}
	res, err = db.CreateFolder(ctx, mb, "Archive", `\Archive`, false)
	if err != nil || !res.Existed || len(res.Created) != 0 {
		t.Fatalf("re-create = %+v, %v; want existed, unchanged", res, err)
	}

	if _, err := db.CreateFolder(ctx, mb, "Other Archive", `\Archive`, false); !errors.Is(err, storage.ErrSpecialUseTaken) {
		t.Fatalf("second \\Archive: err = %v", err)
	}
	for _, bad := range []string{"inbox", "INBOX/Sub", "a//b", "", "tab\there"} {
		if _, err := db.CreateFolder(ctx, mb, bad, "", false); !errors.Is(err, storage.ErrInvalidFolderName) {
			t.Errorf("%q: err = %v, want ErrInvalidFolderName", bad, err)
		}
	}
	if _, err := db.CreateFolder(ctx, mb, "X", `\Bogus`, false); !errors.Is(err, storage.ErrUnknownSpecialUse) {
		t.Fatalf("unknown attribute: err = %v", err)
	}
}
