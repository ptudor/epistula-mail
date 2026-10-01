package imapsess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// countBlobFiles walks the blob store and counts regular files, so a test can
// assert that a rejected APPEND wrote nothing to disk.
func countBlobFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk blob store: %v", err)
	}
	return n
}

// folderCount reports how many folders exist with the given name.
func folderCount(t *testing.T, sess *Session, name string) int {
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
	return n
}

// TestAppendToMissingMailboxReturnsTryCreate is the RO5X-012 regression.
//
// storage.Ingest upserts the destination folder — right for the LDA and the
// importers, wrong for APPEND, where RFC 3501 §6.3.11 wants NO [TRYCREATE].
// Silently creating turned a client typo into a permanent folder shown on
// every device.
func TestAppendToMissingMailboxReturnsTryCreate(t *testing.T) {
	sess, store := appendFixture(t)
	before := countBlobFiles(t, store.Root())

	_, err := sess.Append("Nope", newLiteral([]byte(appendRawMsg)), nil)
	if err == nil {
		t.Fatal("APPEND to a non-existent mailbox succeeded")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("err = %T (%v), want *imap.Error", err, err)
	}
	if imapErr.Code != imap.ResponseCodeTryCreate {
		t.Errorf("Code = %q, want TRYCREATE", imapErr.Code)
	}
	if imapErr.Type != imap.StatusResponseTypeNo {
		t.Errorf("Type = %v, want NO", imapErr.Type)
	}

	if n := folderCount(t, sess, "Nope"); n != 0 {
		t.Errorf("folder %q was created anyway (%d rows)", "Nope", n)
	}
	// The check runs before the literal is read, so no blob is written —
	// which also closes one of RO5X-009's two orphan-blob paths.
	if after := countBlobFiles(t, store.Root()); after != before {
		t.Errorf("blob count went %d -> %d; a rejected APPEND must write nothing", before, after)
	}
}

// TestAppendAfterCreateSucceeds is the other half of the TRYCREATE contract:
// once the client creates the folder, the same APPEND succeeds.
func TestAppendAfterCreateSucceeds(t *testing.T) {
	sess, _ := appendFixture(t)

	if err := sess.Create("Nope", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	data, err := sess.Append("Nope", newLiteral([]byte(appendRawMsg)), nil)
	if err != nil {
		t.Fatalf("APPEND after CREATE: %v", err)
	}
	if data == nil || data.UID == 0 {
		t.Fatalf("AppendData = %+v, want a non-zero UID", data)
	}
}

// TestAppendInboxIsCaseInsensitive covers the INBOX edge case: RFC
// 3501 §5.1 makes INBOX — and only INBOX — case-insensitive, but
// lookupFolder was exact-match, so `APPEND inbox` would have 404'd where
// `APPEND INBOX` succeeded.
func TestAppendInboxIsCaseInsensitive(t *testing.T) {
	sess, _ := appendFixture(t)
	mustCreateFolder(t, sess, "INBOX")

	for _, name := range []string{"INBOX", "inbox", "InBoX"} {
		if _, err := sess.Append(name, newLiteral([]byte(appendRawMsg)), nil); err != nil {
			t.Errorf("APPEND %q: %v", name, err)
		}
	}
	// All three landed in the one INBOX; no case-variant folders appeared.
	for _, name := range []string{"inbox", "InBoX"} {
		if n := folderCount(t, sess, name); n != 0 {
			t.Errorf("a folder named %q was created; INBOX must normalize", name)
		}
	}
}

// TestOtherFolderNamesStayCaseSensitive is the converse: only INBOX is
// special. Normalizing anything else would silently merge distinct folders.
func TestOtherFolderNamesStayCaseSensitive(t *testing.T) {
	sess, _ := appendFixture(t)
	mustCreateFolder(t, sess, "Sent")

	if _, err := sess.Append("sent", newLiteral([]byte(appendRawMsg)), nil); err == nil {
		t.Error("APPEND to \"sent\" succeeded; only INBOX is case-insensitive")
	}
}

// TestAppendRejectsInvalidFolderNames covers the name-validation edge cases:
// empty, over-long, and control characters.
func TestAppendRejectsInvalidFolderNames(t *testing.T) {
	sess, store := appendFixture(t)
	before := countBlobFiles(t, store.Root())

	for _, tc := range []struct {
		name     string
		mailbox  string
		wantType imap.StatusResponseType
	}{
		{"empty", "", imap.StatusResponseTypeBad},
		{"over 255 bytes", strings.Repeat("x", 256), imap.StatusResponseTypeNo},
		{"NUL", "bad\x00name", imap.StatusResponseTypeNo},
		{"newline", "bad\nname", imap.StatusResponseTypeNo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sess.Append(tc.mailbox, newLiteral([]byte(appendRawMsg)), nil)
			if err == nil {
				t.Fatalf("APPEND to %q succeeded", tc.mailbox)
			}
			var imapErr *imap.Error
			if !errors.As(err, &imapErr) {
				t.Fatalf("err = %T (%v), want *imap.Error", err, err)
			}
			if imapErr.Type != tc.wantType {
				t.Errorf("Type = %v, want %v", imapErr.Type, tc.wantType)
			}
		})
	}
	if after := countBlobFiles(t, store.Root()); after != before {
		t.Errorf("blob count went %d -> %d; rejected names must write nothing", before, after)
	}
}

// TestCreateRejectsInvalidFolderNames applies the same validation to CREATE,
// so CREATE and APPEND accept exactly the same folder names.
func TestCreateRejectsInvalidFolderNames(t *testing.T) {
	sess := mutationFixture(t)

	for _, name := range []string{"", strings.Repeat("y", 256), "bad\x00name", "bad\rname"} {
		if err := sess.Create(name, nil); err == nil {
			t.Errorf("CREATE accepted invalid name %q", name)
		}
	}
	// A 255-byte name is at the boundary and must be accepted.
	ok := strings.Repeat("z", 255)
	if err := sess.Create(ok, nil); err != nil {
		t.Errorf("CREATE rejected a 255-byte name, which is within the limit: %v", err)
	}
}
