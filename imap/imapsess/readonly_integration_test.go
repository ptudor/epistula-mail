package imapsess

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/ptudor/epistula-mail/database/blob"
)

// TestExamineRefusesMutations: a folder selected read-only (EXAMINE) must
// reject STORE / MOVE with a NO, per RFC 3501 §6.3.2 — but EXPUNGE must be a
// silent no-op success so CLOSE succeeds (§6.4.2, R-038).
func TestExamineRefusesMutations(t *testing.T) {
	sess := searchFixture(t)
	sess.selectedReadOnly = true // EXAMINE instead of SELECT

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	set := imap.UIDSet{{Start: 1, Stop: 1}}

	storeErr := sess.Store(nil, set, &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen},
	}, nil)
	assertReadOnlyNo(t, "STORE", storeErr)

	// Flag uid=1 \Deleted, then EXPUNGE under EXAMINE: it must return nil (so
	// the library's CLOSE-as-implicit-expunge succeeds) AND remove nothing.
	if _, err := sess.be.Pool.Exec(ctx,
		`UPDATE messages SET flags = array_append(flags, '\Deleted') WHERE folder_id = $1 AND uid = 1`,
		sess.selectedFolderID); err != nil {
		t.Fatalf("flag \\Deleted: %v", err)
	}
	if err := sess.Expunge(nil, &set); err != nil {
		t.Errorf("EXPUNGE on EXAMINEd mailbox = %v, want nil (no-op success)", err)
	}
	var remaining int
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE folder_id = $1 AND uid = 1`,
		sess.selectedFolderID).Scan(&remaining); err != nil {
		t.Fatalf("count after read-only expunge: %v", err)
	}
	if remaining != 1 {
		t.Errorf("read-only EXPUNGE removed the \\Deleted message; want it retained")
	}

	moveErr := sess.Move(nil, set, "Elsewhere")
	assertReadOnlyNo(t, "MOVE", moveErr)

	// COPY only reads the source — it stays allowed under EXAMINE.
	if _, err := sess.Copy(set, "CopyTarget"); err != nil {
		t.Errorf("COPY from EXAMINEd mailbox failed: %v (must be allowed)", err)
	}

	// And after a writable re-select, STORE works again.
	sess.selectedReadOnly = false
	if err := sess.Store(&imapserver.FetchWriter{}, set, &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Flags:  []imap.Flag{imap.FlagSeen},
		Silent: true,
	}, nil); err != nil {
		t.Errorf("STORE after writable select failed: %v", err)
	}
}

func assertReadOnlyNo(t *testing.T, cmd string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s on EXAMINEd mailbox succeeded, want NO", cmd)
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("%s error = %v (%T), want *imap.Error", cmd, err, err)
	}
	if imapErr.Type != imap.StatusResponseTypeNo {
		t.Errorf("%s error type = %v, want NO", cmd, imapErr.Type)
	}
	if !strings.Contains(imapErr.Text, "read-only") {
		t.Errorf("%s error text = %q, want read-only reason", cmd, imapErr.Text)
	}
}

// TestFetchMissingBlobReturnsServerBug: a message row whose blob has been
// removed from disk must produce NO [SERVERBUG], not an empty body.
func TestFetchMissingBlobReturnsServerBug(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mustCreateFolder(t, sess, "INBOX")
	data, err := sess.Append("INBOX", newLiteral([]byte(appendRawMsg)), &imap.AppendOptions{})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var folderID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT folder_id FROM messages WHERE uid = $1`, int64(data.UID),
	).Scan(&folderID); err != nil {
		t.Fatalf("folder lookup: %v", err)
	}
	sess.selectedFolderID = folderID
	sess.selectedFolderName = "INBOX"

	// Sabotage: remove the blob from disk (store corruption).
	var shaHex string
	var blobDate time.Time
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT encode(raw_sha256, 'hex'), raw_blob_date FROM messages WHERE uid = $1`,
		int64(data.UID),
	).Scan(&shaHex, &blobDate); err != nil {
		t.Fatalf("sha lookup: %v", err)
	}
	path, err := sess.be.BlobStore.PathFor(blob.KindRaw, sess.tenant, blob.BucketFromTime(blobDate), shaHex)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	r := fetchRow{
		uid:          int64(data.UID),
		rawSHA256Hex: shaHex,
		rawBlobDate:  blobDate,
		rawSize:      int64(len(appendRawMsg)),
	}
	streamErr := sess.streamRawBody(nil, r, &imap.FetchItemBodySection{})
	var imapErr *imap.Error
	if !errors.As(streamErr, &imapErr) {
		t.Fatalf("streamRawBody = %v (%T), want *imap.Error", streamErr, streamErr)
	}
	if imapErr.Type != imap.StatusResponseTypeNo || imapErr.Code != imap.ResponseCodeServerBug {
		t.Errorf("missing blob response = %v [%v], want NO [SERVERBUG]", imapErr.Type, imapErr.Code)
	}
}
