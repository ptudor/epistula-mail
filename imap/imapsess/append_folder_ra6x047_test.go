package imapsess

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestAppendDoesNotRecreateADeletedFolder is the RA6X-047 regression.
//
// APPEND checked that the destination existed, threw the folder id away, and
// then let storage.Ingest resolve the name again and upsert it. A folder
// deleted between those two points was silently recreated and the message
// landed in the new one — an APPEND undoing another session's DELETE.
func TestAppendDoesNotRecreateADeletedFolder(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A folder that exists at lookup time and is gone by commit time.
	if err := sess.Create("Doomed", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	folderID, err := folderIDByName(sess, "Doomed")
	if err != nil {
		t.Fatalf("lookup Doomed: %v", err)
	}

	if _, err := sess.be.Pool.Exec(ctx, `DELETE FROM folders WHERE id = $1`, folderID); err != nil {
		t.Fatalf("delete folder: %v", err)
	}

	raw := "From: s@x.invalid\r\nSubject: orphan\r\n\r\nbody\r\n"
	_, aerr := sess.Append("Doomed", strings.NewReader(raw), &imap.AppendOptions{})
	if aerr == nil {
		t.Fatal("APPEND into a deleted folder succeeded")
	}
	var ierr *imap.Error
	if !asIMAPError(aerr, &ierr) || ierr.Code != imap.ResponseCodeTryCreate {
		t.Fatalf("APPEND returned %v; want NO [TRYCREATE]", aerr)
	}

	// And the folder must NOT have come back.
	var exists bool
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM folders WHERE mailbox_id = $1 AND name = 'Doomed')`,
		sess.mailboxID,
	).Scan(&exists); err != nil {
		t.Fatalf("existence check: %v", err)
	}
	if exists {
		t.Fatal("APPEND recreated a folder another session had deleted")
	}
}

// TestAppendUIDPlusIsConsistent pins that APPENDUID's UID and UIDVALIDITY come
// from one committed operation. A second post-commit query could report a
// validity from a later rename — or zero, if the folder had been deleted — and
// a client trusts the pair it is given.
func TestAppendUIDPlusIsConsistent(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := sess.Create("Sent", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	folderID, err := folderIDByName(sess, "Sent")
	if err != nil {
		t.Fatalf("lookup Sent: %v", err)
	}
	var wantValidity int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT uidvalidity FROM folders WHERE id = $1`, folderID).Scan(&wantValidity); err != nil {
		t.Fatalf("read uidvalidity: %v", err)
	}

	raw := "From: s@x.invalid\r\nSubject: sent\r\n\r\nbody\r\n"
	data, aerr := sess.Append("Sent", strings.NewReader(raw), &imap.AppendOptions{})
	if aerr != nil {
		t.Fatalf("APPEND: %v", aerr)
	}
	if data == nil {
		t.Fatal("APPEND returned no APPENDUID for a healthy folder")
	}
	if data.UIDValidity != uint32(wantValidity) {
		t.Fatalf("APPENDUID validity = %d, want %d", data.UIDValidity, wantValidity)
	}
	if data.UID == 0 {
		t.Fatal("APPENDUID reported UID 0")
	}

	// The pair must actually address the stored message.
	var stored int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE folder_id = $1 AND uid = $2`,
		folderID, int64(data.UID),
	).Scan(&stored); err != nil {
		t.Fatalf("locate stored message: %v", err)
	}
	if stored != 1 {
		t.Fatalf("APPENDUID does not identify the stored message (%d rows)", stored)
	}
}

// TestDeliveryStillAutoCreatesItsFolder pins that the must-exist policy is
// APPEND's alone: the LDA and the importers still create INBOX on first
// delivery, which is the behaviour storage.Ingest has to keep.
func TestDeliveryStillAutoCreatesItsFolder(t *testing.T) {
	sess := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var before bool
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM folders WHERE mailbox_id = $1 AND name = 'Fresh')`,
		sess.mailboxID).Scan(&before); err != nil {
		t.Fatalf("existence check: %v", err)
	}
	if before {
		t.Fatal("fixture already has the folder")
	}

	// MustExistFolderID left zero: the auto-create policy.
	res, err := deliverLikeIngest(t, ctx, sess, "Fresh")
	if err != nil {
		t.Fatalf("delivery-style ingest: %v", err)
	}
	if res.FolderID == 0 {
		t.Fatal("delivery did not create the folder")
	}
	if res.UIDValidity <= 0 {
		t.Fatalf("delivery returned uidvalidity %d", res.UIDValidity)
	}
}

// deliverLikeIngest runs storage.Ingest the way the LDA does — no must-exist
// folder — so the auto-create policy stays covered.
func deliverLikeIngest(t *testing.T, ctx context.Context, sess *Session, folder string) (storage.IngestResult, error) {
	t.Helper()
	raw := []byte("From: s@x.invalid\r\nSubject: delivered\r\n\r\nbody\r\n")
	parser := ingest.New(ingest.Limits{
		MaxMessageBytes:       10 << 20,
		MaxMimeDepth:          10,
		MaxMimeParts:          200,
		MaxHeaderBytes:        16 << 10,
		MaxHeaderSectionBytes: 256 << 10,
		MaxTransferExpansion:  10,
	})
	msg, err := parser.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	when := time.Now().UTC()
	w, err := sess.be.BlobStore.NewWriter(blob.KindRaw, sess.tenant, blob.BucketFromTime(when))
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("blob write: %v", err)
	}
	sha, _, _, err := w.Close()
	if err != nil {
		t.Fatalf("blob close: %v", err)
	}
	return sess.be.Storage().Ingest(ctx, storage.IngestParams{
		MailboxID:    sess.mailboxID,
		MailboxName:  sess.mailboxName,
		FolderName:   folder,
		EnvelopeTo:   sess.mailboxName + "@example.invalid",
		RawSHA256Hex: sha,
		RawSize:      int64(len(raw)),
		RawBlobDate:  when,
		Message:      msg,
	})
}
