package imapsess

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// fakeLiteral implements imap.LiteralReader for the in-process tests.
type fakeLiteral struct {
	*bytes.Reader
	size int64
}

func newLiteral(b []byte) *fakeLiteral {
	return &fakeLiteral{Reader: bytes.NewReader(b), size: int64(len(b))}
}
func (f *fakeLiteral) Size() int64 { return f.size }

// appendFixture mirrors fetchFixture but stops at "session ready" —
// no message rows pre-inserted, since APPEND is the system under test.
func appendFixture(t *testing.T) (*Session, *blob.Store) {
	t.Helper()
	db, _ := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var mailboxID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', '$argon2id$ph') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("blob init: %v", err)
	}

	be := &Backend{
		Pool:        pool,
		BlobStore:   store,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		StmtTimeout: 10 * time.Second,
	}
	sess := be.NewSession()
	sess.mailboxID = mailboxID
	sess.mailboxName = "alice"
	sess.tenant = "alice" // normally set by Login; tests bypass it
	return sess, store
}

// mustCreateFolder creates a destination folder for an APPEND.
//
// APPEND no longer auto-creates its target: per RFC 3501 6.3.11 a missing
// mailbox is NO [TRYCREATE], so the client can decide (RO5X-012). Tests that
// append therefore have to state the precondition, exactly as a real client
// would by issuing CREATE first.
func mustCreateFolder(t *testing.T, sess *Session, name string) {
	t.Helper()
	if err := sess.Create(name, nil); err != nil {
		var imapErr *imap.Error
		if errors.As(err, &imapErr) && imapErr.Code == imap.ResponseCodeAlreadyExists {
			return
		}
		t.Fatalf("create folder %q: %v", name, err)
	}
}

const appendRawMsg = "From: a@x\r\n" +
	"To: b@y\r\n" +
	"Subject: APPENDed\r\n" +
	"Message-ID: <append-1@x>\r\n" +
	"Date: Mon, 18 May 2026 12:00:00 +0000\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"body of an appended message\r\n"

func TestAppendCreatesMessageRowAndBlob(t *testing.T) {
	sess, store := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mustCreateFolder(t, sess, "Sent")
	data, err := sess.Append("Sent", newLiteral([]byte(appendRawMsg)), nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if data == nil || data.UID == 0 {
		t.Fatalf("AppendData = %+v, want non-zero UID", data)
	}
	if data.UIDValidity == 0 {
		t.Error("UIDValidity should be set for UIDPLUS APPENDUID response")
	}

	// Message row must exist with the expected subject and folder.
	var subject string
	var folderName string
	if err := sess.be.Pool.QueryRow(ctx, `
		SELECT m.subject, f.name
		  FROM messages m JOIN folders f ON f.id = m.folder_id
		 WHERE m.uid = $1 AND f.mailbox_id = $2 AND f.name = 'Sent'`,
		int64(data.UID), sess.mailboxID,
	).Scan(&subject, &folderName); err != nil {
		t.Fatalf("messages row lookup: %v", err)
	}
	if subject != "APPENDed" {
		t.Errorf("subject = %q, want %q", subject, "APPENDed")
	}

	// Blob must be readable at the bucket the message row references.
	var bucketDate time.Time
	var shaHex string
	if err := sess.be.Pool.QueryRow(ctx, `
		SELECT raw_blob_date, encode(raw_sha256, 'hex') FROM messages WHERE uid = $1`,
		int64(data.UID),
	).Scan(&bucketDate, &shaHex); err != nil {
		t.Fatalf("blob locator lookup: %v", err)
	}
	rc, err := store.Open(blob.KindRaw, "alice", blob.BucketFromTime(bucketDate), shaHex)
	if err != nil {
		t.Fatalf("re-open raw blob: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != appendRawMsg {
		t.Errorf("blob round-trip mismatch (got %d bytes, want %d)", len(got), len(appendRawMsg))
	}
}

func TestAppendRespectsFlags(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	options := &imap.AppendOptions{
		Flags: []imap.Flag{imap.FlagSeen, imap.FlagDraft},
	}
	mustCreateFolder(t, sess, "Drafts")
	data, err := sess.Append("Drafts", newLiteral([]byte(appendRawMsg)), options)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var flags []string
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT flags FROM messages WHERE uid = $1`, int64(data.UID),
	).Scan(&flags); err != nil {
		t.Fatalf("flags lookup: %v", err)
	}
	if len(flags) != 2 {
		t.Fatalf("flags = %v, want 2", flags)
	}
	// Order isn't guaranteed; just check both are present.
	seen := false
	draft := false
	for _, f := range flags {
		if f == string(imap.FlagSeen) {
			seen = true
		}
		if f == string(imap.FlagDraft) {
			draft = true
		}
	}
	if !seen || !draft {
		t.Errorf("flags = %v, want \\Seen + \\Draft", flags)
	}
}

func TestAppendRejectsOversizeLiteral(t *testing.T) {
	sess, _ := appendFixture(t)
	sess.be.MaxAppendBytes = 100 // tight cap

	big := bytes.Repeat([]byte("X"), 200)
	_, err := sess.Append("INBOX", newLiteral(big), nil)
	if err == nil {
		t.Fatal("expected error for oversize APPEND, got nil")
	}
	imapErr, ok := err.(*imap.Error)
	if !ok {
		t.Fatalf("err = %T %v, want *imap.Error", err, err)
	}
	if imapErr.Code != imap.ResponseCodeTooBig {
		t.Errorf("code = %v, want TOOBIG", imapErr.Code)
	}
}

func TestAppendRejectsMalformedRFC5322(t *testing.T) {
	sess, _ := appendFixture(t)

	// Empty literal — parser rejects.
	mustCreateFolder(t, sess, "INBOX")
	_, err := sess.Append("INBOX", newLiteral(nil), nil)
	if err == nil {
		t.Fatal("expected error for empty/malformed APPEND, got nil")
	}
	imapErr, ok := err.(*imap.Error)
	if !ok {
		t.Fatalf("err = %T %v, want *imap.Error", err, err)
	}
	if !strings.Contains(imapErr.Text, "malformed") {
		t.Errorf("text = %q, want malformed-RFC5322 reason", imapErr.Text)
	}
}

func TestAppendHonorsExplicitTimeForBucket(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	want := time.Date(2003, 4, 15, 0, 0, 0, 0, time.UTC)
	options := &imap.AppendOptions{Time: want}
	mustCreateFolder(t, sess, "Archive/2003")
	data, err := sess.Append("Archive/2003", newLiteral([]byte(appendRawMsg)), options)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got, internal time.Time
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT raw_blob_date, internal_date FROM messages WHERE uid = $1`, int64(data.UID),
	).Scan(&got, &internal); err != nil {
		t.Fatalf("blob date lookup: %v", err)
	}
	if !got.UTC().Equal(want) {
		t.Errorf("raw_blob_date = %v, want %v (client-supplied AppendOptions.Time)", got.UTC(), want)
	}
	// RFC 3501/9051 §6.3.12: the optional date-time argument sets the
	// message's INTERNALDATE, not just our on-disk bucket.
	if !internal.UTC().Equal(want) {
		t.Errorf("internal_date = %v, want %v (client-supplied AppendOptions.Time)", internal.UTC(), want)
	}
}

func TestAppendWithoutTimeUsesServerClock(t *testing.T) {
	sess, _ := appendFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	before := time.Now().UTC().Add(-time.Minute)
	mustCreateFolder(t, sess, "INBOX")
	data, err := sess.Append("INBOX", newLiteral([]byte(appendRawMsg)), &imap.AppendOptions{})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	after := time.Now().UTC().Add(time.Minute)

	var internal time.Time
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT internal_date FROM messages WHERE uid = $1`, int64(data.UID),
	).Scan(&internal); err != nil {
		t.Fatalf("internal date lookup: %v", err)
	}
	if internal.UTC().Before(before) || internal.UTC().After(after) {
		t.Errorf("internal_date = %v, want roughly now (no client time supplied)", internal.UTC())
	}
}
