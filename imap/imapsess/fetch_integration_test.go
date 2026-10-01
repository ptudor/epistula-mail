package imapsess

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// fetchTestFixture sets up one folder with one message in a fresh PG and
// returns a Session ready for FETCH. The raw blob is written to a
// per-test blob store so FETCH BODY[] can stream it back.
type fetchTestFixture struct {
	pool      *pgxpool.Pool
	session   *Session
	mailboxID int64
	folderID  int64
	uid       int64
	rawBytes  []byte
}

func newFetchFixture(t *testing.T) *fetchTestFixture {
	t.Helper()
	db, _ := pgtest.Open(t)
	pool := db.Pool()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var mailboxID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('alice', '$argon2id$placeholder') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatalf("insert mailbox: %v", err)
	}

	var folderID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'INBOX', $2, 2) RETURNING id`,
		mailboxID, time.Now().Unix(),
	).Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}

	headers := map[string][]string{
		"Subject":    {"Hello FETCH"},
		"From":       {"Bob <bob@example.invalid>"},
		"To":         {"Alice <alice@example.invalid>"},
		"Reply-To":   {"Bob Replies <bob-replies@example.invalid>"},
		"Bcc":        {"Eve <eve@example.invalid>"},
		"Message-Id": {"<msg-1@example.invalid>"},
	}
	headersJSON, err := json.Marshal(headers)
	if err != nil {
		t.Fatalf("marshal headers: %v", err)
	}
	// Realistic multipart/alternative bodystructure so the BodyStructure
	// decoder test below has something non-trivial to walk.
	bsJSON, _ := json.Marshal(map[string]any{
		"type":    "multipart",
		"subtype": "alternative",
		"params":  map[string]string{"boundary": "--mixed-boundary"},
		"size":    0,
		"parts": []any{
			map[string]any{
				"type": "text", "subtype": "plain",
				"params": map[string]string{"charset": "utf-8"},
				"size":   24, "lines": 2, "encoding": "7bit",
			},
			map[string]any{
				"type": "text", "subtype": "html",
				"params": map[string]string{"charset": "utf-8"},
				"size":   48, "lines": 1, "encoding": "quoted-printable",
			},
		},
	})

	rawBytes := []byte("Subject: Hello FETCH\r\nFrom: Bob <bob@example.invalid>\r\nTo: Alice <alice@example.invalid>\r\nReply-To: Bob Replies <bob-replies@example.invalid>\r\nBcc: Eve <eve@example.invalid>\r\nMessage-ID: <msg-1@example.invalid>\r\nDate: Mon, 18 May 2026 14:00:00 +0000\r\n\r\nbody\r\n")
	sum := sha256.Sum256(rawBytes)
	rawSHA := hex.EncodeToString(sum[:])
	internalDate := time.Date(2026, 5, 18, 14, 30, 0, 0, time.UTC)
	sentDate := time.Date(2026, 5, 18, 14, 0, 0, 0, time.UTC)
	blobDate := internalDate

	// Stand up a real blob store in t.TempDir and write the raw blob to
	// the same bucket the DB row will reference.
	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("blob init: %v", err)
	}
	bucket := blob.BucketFromTime(blobDate)
	bw, err := store.NewWriter(blob.KindRaw, "alice", bucket)
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := bw.Write(rawBytes); err != nil {
		t.Fatalf("blob write: %v", err)
	}
	if _, _, _, err := bw.Close(); err != nil {
		t.Fatalf("blob close: %v", err)
	}

	var uid int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO messages (
			folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
			message_id, in_reply_to, subject, from_addr,
			to_addrs, cc_addrs, sent_date, headers,
			text_body, html_body, bodystructure, flags
		) VALUES (
			$1, 1, $2, $3, $4, $5,
			$6, NULL, $7, $8,
			$9, $10, $11, $12,
			'body', NULL, $13, ARRAY['\Seen','\Flagged']
		) RETURNING uid`,
		folderID, sum[:], internalDate, int64(len(rawBytes)), internalDate,
		"msg-1@example.invalid", "Hello FETCH", "Bob <bob@example.invalid>",
		[]string{"Alice <alice@example.invalid>"},
		[]string{},
		sentDate, headersJSON, bsJSON,
	).Scan(&uid); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	_ = rawSHA // already encoded into the messages row via raw_sha256

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
	sess.selectedFolderID = folderID
	sess.selectedFolderName = "INBOX"

	return &fetchTestFixture{
		pool:      pool,
		session:   sess,
		mailboxID: mailboxID,
		folderID:  folderID,
		uid:       uid,
		rawBytes:  rawBytes,
	}
}

func TestFetchTargetsResolvesSeqSet(t *testing.T) {
	f := newFetchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := f.session.fetchTargets(ctx, imap.SeqSetNum(1), &imap.FetchOptions{UID: true})
	if err != nil {
		t.Fatalf("fetchTargets: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.seqNum != 1 {
		t.Errorf("seqNum = %d, want 1", r.seqNum)
	}
	if r.uid != f.uid {
		t.Errorf("uid = %d, want %d", r.uid, f.uid)
	}
}

func TestFetchTargetsResolvesUIDSet(t *testing.T) {
	f := newFetchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := f.session.fetchTargets(ctx, imap.UIDSetNum(imap.UID(f.uid)), &imap.FetchOptions{})
	if err != nil {
		t.Fatalf("fetchTargets: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].uid != f.uid {
		t.Errorf("uid = %d, want %d", rows[0].uid, f.uid)
	}
}

func TestFetchTargetsDynamicStar(t *testing.T) {
	f := newFetchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// "*" resolves to the max sequence number. SeqSet{Start:0,Stop:0} is
	// how the upstream lib encodes a bare "*" — both endpoints are zero
	// and get filled in from the folder's max seqnum.
	rows, err := f.session.fetchTargets(ctx, imap.SeqSet{{Start: 0, Stop: 0}}, &imap.FetchOptions{})
	if err != nil {
		t.Fatalf("fetchTargets: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
}

func TestFetchEnvelopeBuilds(t *testing.T) {
	f := newFetchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The envelope columns are loaded because ENVELOPE is REQUESTED. This test
	// used to pass an empty FetchOptions and still find them, which is the
	// behaviour RA6X-054 removes: fetchTargets loaded the headers JSONB and
	// every envelope column for every message whatever was asked for.
	rows, err := f.session.fetchTargets(ctx, imap.SeqSetNum(1), &imap.FetchOptions{Envelope: true})
	if err != nil {
		t.Fatalf("fetchTargets: %v", err)
	}
	header, err := f.session.loadRawHeaderSection(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	env, err := buildRawEnvelope(rows[0], header)
	if err != nil {
		t.Fatal(err)
	}

	if env.Subject != "Hello FETCH" {
		t.Errorf("Subject = %q, want %q", env.Subject, "Hello FETCH")
	}
	if env.MessageID != "msg-1@example.invalid" {
		t.Errorf("MessageID = %q, want %q (unbracketed)", env.MessageID, "msg-1@example.invalid")
	}
	if len(env.From) != 1 || env.From[0].Mailbox != "bob" || env.From[0].Host != "example.invalid" {
		t.Errorf("From = %+v, want bob@example.invalid", env.From)
	}
	if len(env.To) != 1 || env.To[0].Mailbox != "alice" {
		t.Errorf("To = %+v, want alice@example.invalid", env.To)
	}
	if len(env.ReplyTo) != 1 || env.ReplyTo[0].Mailbox != "bob-replies" {
		t.Errorf("ReplyTo = %+v, want bob-replies (original Reply-To header)", env.ReplyTo)
	}
	if len(env.Bcc) != 1 || env.Bcc[0].Mailbox != "eve" {
		t.Errorf("Bcc = %+v, want eve (original Bcc header)", env.Bcc)
	}
	// Sender defaults to From per RFC 9051 when absent in headers.
	if len(env.Sender) != 1 || env.Sender[0].Mailbox != "bob" {
		t.Errorf("Sender = %+v, want defaulted-to-From bob", env.Sender)
	}
	// Date prefers sent_date over internal_date. Postgres returns
	// timestamps in the connection's local zone, so normalize to UTC
	// before comparing.
	utc := env.Date.UTC()
	if utc.Hour() != 14 || utc.Minute() != 0 {
		t.Errorf("Date.UTC() = %v, want sent_date 14:00 UTC", utc)
	}
}

func TestFetchBodyStructureFromFixture(t *testing.T) {
	f := newFetchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// BODYSTRUCTURE requested, so the column is loaded (RA6X-054).
	rows, err := f.session.fetchTargets(ctx, imap.SeqSetNum(1),
		&imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{}})
	if err != nil {
		t.Fatalf("fetchTargets: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	bs, err := decodeBodyStructure(rows[0].bodyStructure)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	mp, ok := bs.(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("got %T, want *BodyStructureMultiPart from fixture", bs)
	}
	if mp.Subtype != "alternative" || len(mp.Children) != 2 {
		t.Errorf("multipart shape wrong: subtype=%q children=%d", mp.Subtype, len(mp.Children))
	}
}

func TestFetchTargetsCarryBlobLocator(t *testing.T) {
	f := newFetchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A BODY[] section is requested, which is the only thing the blob locator
	// columns are for (RA6X-054).
	rows, err := f.session.fetchTargets(ctx, imap.SeqSetNum(1),
		&imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}})
	if err != nil {
		t.Fatalf("fetchTargets: %v", err)
	}
	r := rows[0]
	if r.rawSHA256Hex == "" {
		t.Error("fetchRow.rawSHA256Hex empty; BODY[] would have nothing to open")
	}
	if r.rawBlobDate.IsZero() {
		t.Error("fetchRow.rawBlobDate zero; BODY[] would build an unknown-bucket path")
	}
}

func TestFetchBodyRawBlobReadable(t *testing.T) {
	f := newFetchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := f.session.fetchTargets(ctx, imap.SeqSetNum(1),
		&imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}})
	if err != nil {
		t.Fatalf("fetchTargets: %v", err)
	}
	r := rows[0]

	// Re-open the blob using the same locator FETCH BODY[] would.
	bucket := blob.BucketFromTime(r.rawBlobDate)
	rc, err := f.session.be.BlobStore.Open(blob.KindRaw, f.session.tenant, bucket, r.rawSHA256Hex)
	if err != nil {
		t.Fatalf("BlobStore.Open(%s, %q, %s): %v",
			blob.KindRaw, bucket, r.rawSHA256Hex[:16], err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(got, f.rawBytes) {
		t.Errorf("BODY[] payload mismatch:\n got %q\nwant %q", got, f.rawBytes)
	}
}

func TestFetchTargetsSeqSetEmptyForUnknown(t *testing.T) {
	f := newFetchFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := f.session.fetchTargets(ctx, imap.SeqSetNum(42), &imap.FetchOptions{})
	if err != nil {
		t.Fatalf("fetchTargets: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0 (seqNum 42 doesn't exist)", len(rows))
	}
}

// clearSeen strips \Seen from the fixture message so the implicit-\Seen
// tests start from an unseen state.
func clearSeen(t *testing.T, f *fetchTestFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx,
		`UPDATE messages SET flags = ARRAY['\Flagged'] WHERE folder_id = $1 AND uid = $2`,
		f.folderID, f.uid,
	); err != nil {
		t.Fatalf("clear seen: %v", err)
	}
}

func fixtureFlags(t *testing.T, f *fetchTestFixture) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var flags []string
	if err := f.pool.QueryRow(ctx,
		`SELECT flags FROM messages WHERE folder_id = $1 AND uid = $2`,
		f.folderID, f.uid,
	).Scan(&flags); err != nil {
		t.Fatalf("flags lookup: %v", err)
	}
	return flags
}

func TestFetchNonPeekSetsSeen(t *testing.T) {
	f := newFetchFixture(t)
	clearSeen(t, f)

	opts := &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}}, // BODY[] — no .PEEK
	}
	if err := f.session.Fetch(nil, imap.SeqSetNum(1), opts); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	flags := fixtureFlags(t, f)
	found := false
	for _, fl := range flags {
		if fl == `\Seen` {
			found = true
		}
	}
	if !found {
		t.Errorf("flags = %v, want \\Seen added by non-peek BODY[] fetch", flags)
	}

	// The side effect must stamp a fresh modseq like STORE does.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var msgModSeq, folderModSeq int64
	if err := f.pool.QueryRow(ctx,
		`SELECT m.mod_seq, fo.highest_modseq
		   FROM messages m JOIN folders fo ON fo.id = m.folder_id
		  WHERE m.folder_id = $1 AND m.uid = $2`,
		f.folderID, f.uid,
	).Scan(&msgModSeq, &folderModSeq); err != nil {
		t.Fatalf("modseq lookup: %v", err)
	}
	if msgModSeq != folderModSeq || msgModSeq < 2 {
		t.Errorf("mod_seq = %d, highest_modseq = %d; want equal and bumped", msgModSeq, folderModSeq)
	}
}

func TestFetchPeekDoesNotSetSeen(t *testing.T) {
	f := newFetchFixture(t)
	clearSeen(t, f)

	opts := &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}
	if err := f.session.Fetch(nil, imap.SeqSetNum(1), opts); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, fl := range fixtureFlags(t, f) {
		if fl == `\Seen` {
			t.Error("BODY.PEEK[] must not set \\Seen")
		}
	}
}

func TestFetchReadOnlyDoesNotSetSeen(t *testing.T) {
	f := newFetchFixture(t)
	clearSeen(t, f)
	f.session.selectedReadOnly = true

	opts := &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}},
	}
	if err := f.session.Fetch(nil, imap.SeqSetNum(1), opts); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, fl := range fixtureFlags(t, f) {
		if fl == `\Seen` {
			t.Error("EXAMINE'd (read-only) mailbox must not get \\Seen from a non-peek fetch")
		}
	}
}
