package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestIngestFullRoundTrip is the canonical storage.Ingest contract test:
// every derived column, the attachment rows, the delivery_log row, the
// used_bytes delta, the uidnext bump, and the pg_notify on commit.
func TestIngestFullRoundTrip(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	raw := []byte("From: Alice Sender <alice@roundtrip.invalid>\r\n" +
		"To: bob@roundtrip.invalid, carol@roundtrip.invalid\r\n" +
		"Cc: dave@roundtrip.invalid\r\n" +
		"Subject: round trip\r\n" +
		"Date: Mon, 31 Oct 2022 12:00:00 +0000\r\n" +
		"Message-ID: <rt-1@roundtrip.invalid>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=RT\r\n" +
		"\r\n" +
		"--RT\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"the searchable body\r\n" +
		"--RT\r\n" +
		"Content-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=doc.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"JVBERi0xLjQK\r\n" +
		"--RT--\r\n")

	msg, err := gcTestParser().Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	const tenant blob.Tenant = "roundtrip"
	blobDate := time.Date(2022, 10, 31, 12, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(blobDate)
	w, err := store.NewWriter(blob.KindRaw, tenant, bucket)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sha, _, _, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	attParams := make([]storage.AttachmentParams, 0, len(msg.Attachments))
	for _, att := range msg.Attachments {
		aw, err := store.NewWriter(blob.KindAttachment, tenant, bucket)
		if err != nil {
			t.Fatalf("att writer: %v", err)
		}
		if _, err := aw.Write(att.Data); err != nil {
			t.Fatalf("att write: %v", err)
		}
		attSHA, _, _, err := aw.Close()
		if err != nil {
			t.Fatalf("att close: %v", err)
		}
		attParams = append(attParams, storage.AttachmentParams{
			PartNumber: att.PartNumber, Filename: att.Filename,
			ContentType: att.ContentType, ContentID: att.ContentID,
			Disposition: att.Disposition, Size: att.Size,
			SHA256Hex: attSHA, BlobDate: blobDate,
		})
	}

	mboxID := gcMustMailbox(t, ctx, db, "roundtrip")

	// LISTEN before the ingest so the commit-time NOTIFY is observable.
	listenConn, err := db.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listen conn: %v", err)
	}
	defer listenConn.Release()
	if _, err := listenConn.Exec(ctx, `LISTEN mail_arrived`); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	res, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID:    mboxID,
		MailboxName:  string(tenant),
		FolderName:   "INBOX",
		EnvelopeFrom: "alice@roundtrip.invalid",
		EnvelopeTo:   "bob@roundtrip.invalid",
		RawSHA256Hex: sha,
		RawSize:      int64(len(raw)),
		RawBlobDate:  blobDate,
		Message:      msg,
		Attachments:  attParams,
		Flags:        []string{`\Seen`},
		EnsureBlobs:  ensureBlobsFunc(store, tenant, bucket, sha, raw, msg.Attachments, attParams),
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.UID != 1 {
		t.Errorf("first UID = %d, want 1", res.UID)
	}
	if res.ModSeq < 1 {
		t.Errorf("ModSeq = %d, want >= 1", res.ModSeq)
	}

	// NOTIFY arrives with the folder id as payload.
	notifyCtx, notifyCancel := context.WithTimeout(ctx, 5*time.Second)
	defer notifyCancel()
	n, err := listenConn.Conn().WaitForNotification(notifyCtx)
	if err != nil {
		t.Fatalf("pg_notify not received: %v", err)
	}
	if n.Payload != strconv.FormatInt(res.FolderID, 10) {
		t.Errorf("notify payload = %q, want folder id %d", n.Payload, res.FolderID)
	}

	// Derived message columns.
	var (
		subject, fromAddr, textBody string
		toAddrs, ccAddrs, flags     []string
		uid, modSeq, rawSize        int64
		internalDate, sentDate      time.Time
		headersJSON, bsJSON         []byte
		messageIDHdr                string
	)
	if err := db.Pool().QueryRow(ctx,
		`SELECT subject, from_addr, text_body, to_addrs, cc_addrs, flags,
		        uid, mod_seq, raw_size, internal_date, sent_date,
		        headers, bodystructure, message_id
		   FROM messages WHERE id = $1`, res.MessageID,
	).Scan(&subject, &fromAddr, &textBody, &toAddrs, &ccAddrs, &flags,
		&uid, &modSeq, &rawSize, &internalDate, &sentDate,
		&headersJSON, &bsJSON, &messageIDHdr); err != nil {
		t.Fatalf("select message: %v", err)
	}
	if subject != "round trip" {
		t.Errorf("subject = %q", subject)
	}
	if fromAddr != "alice@roundtrip.invalid" {
		t.Errorf("from_addr = %q", fromAddr)
	}
	if len(toAddrs) != 2 || len(ccAddrs) != 1 {
		t.Errorf("to/cc = %v / %v, want 2/1 entries", toAddrs, ccAddrs)
	}
	if !contains(flags, `\Seen`) {
		t.Errorf("flags = %v, want \\Seen", flags)
	}
	if uid != res.UID || modSeq != res.ModSeq {
		t.Errorf("row uid/modseq %d/%d != result %d/%d", uid, modSeq, res.UID, res.ModSeq)
	}
	if rawSize != int64(len(raw)) {
		t.Errorf("raw_size = %d, want %d", rawSize, len(raw))
	}
	if sentDate.UTC().Format("2006-01-02") != "2022-10-31" {
		t.Errorf("sent_date = %v", sentDate)
	}
	if messageIDHdr != "rt-1@roundtrip.invalid" {
		t.Errorf("message_id = %q", messageIDHdr)
	}
	if len(headersJSON) == 0 || len(bsJSON) == 0 {
		t.Error("headers/bodystructure JSONB empty")
	}
	if textBody == "" || !utf8.ValidString(textBody) {
		t.Errorf("text_body = %q", textBody)
	}

	// FTS finds the body.
	var ftsHit bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM messages
		  WHERE id = $1 AND fts @@ plainto_tsquery('simple', 'searchable'))`,
		res.MessageID,
	).Scan(&ftsHit); err != nil || !ftsHit {
		t.Errorf("FTS lookup failed: hit=%v err=%v", ftsHit, err)
	}

	// Attachment row.
	var attCount int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM attachments WHERE message_id = $1`, res.MessageID,
	).Scan(&attCount); err != nil || attCount != 1 {
		t.Errorf("attachments = %d (err=%v), want 1", attCount, err)
	}

	// delivery_log row.
	var outcome, shaShort string
	if err := db.Pool().QueryRow(ctx,
		`SELECT outcome, raw_sha256_short FROM delivery_log WHERE message_id = $1`,
		res.MessageID,
	).Scan(&outcome, &shaShort); err != nil {
		t.Fatalf("select delivery_log: %v", err)
	}
	if outcome != "delivered" || shaShort != sha[:16] {
		t.Errorf("delivery_log outcome=%q sha_short=%q", outcome, shaShort)
	}

	// used_bytes delta and uidnext bump.
	var usedBytes, uidNext int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT m.used_bytes, f.uidnext FROM mailboxes m
		   JOIN folders f ON f.mailbox_id = m.id
		  WHERE m.id = $1`, mboxID,
	).Scan(&usedBytes, &uidNext); err != nil {
		t.Fatalf("select quota/uidnext: %v", err)
	}
	if usedBytes != int64(len(raw)) {
		t.Errorf("used_bytes = %d, want %d", usedBytes, len(raw))
	}
	if uidNext != 2 {
		t.Errorf("uidnext = %d, want 2", uidNext)
	}
}

// TestIngestConcurrentUIDAllocation hammers one folder from N goroutines and
// asserts UID and modseq uniqueness plus an exact used_bytes total — the
// serialization point CLAUDE.md stakes the design on.
func TestIngestConcurrentUIDAllocation(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	mboxID := gcMustMailbox(t, ctx, db, "concurrent")
	const n = 20

	uids := make([]int64, n)
	modSeqs := make([]int64, n)
	var totalBytes int64
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		raw := []byte(fmt.Sprintf("From: c%d@conc.invalid\r\nSubject: c%d\r\n\r\nbody %d\r\n", i, i, i))
		totalBytes += int64(len(raw))
		wg.Add(1)
		go func(i int, raw []byte) {
			defer wg.Done()
			msg, err := gcTestParser().Parse(raw)
			if err != nil {
				errs[i] = err
				return
			}
			sum := sha256.Sum256(raw)
			res, err := db.Ingest(ctx, storage.IngestParams{
				MailboxID:    mboxID,
				MailboxName:  "concurrent",
				FolderName:   "INBOX",
				EnvelopeTo:   fmt.Sprintf("c%d@conc.invalid", i),
				RawSHA256Hex: hex.EncodeToString(sum[:]),
				RawSize:      int64(len(raw)),
				Message:      msg,
			})
			if err != nil {
				errs[i] = err
				return
			}
			uids[i] = res.UID
			modSeqs[i] = res.ModSeq
		}(i, raw)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	seenUID := map[int64]bool{}
	seenMod := map[int64]bool{}
	for i := 0; i < n; i++ {
		if uids[i] < 1 || uids[i] > n {
			t.Errorf("uid[%d] = %d, want 1..%d", i, uids[i], n)
		}
		if seenUID[uids[i]] {
			t.Errorf("duplicate UID %d", uids[i])
		}
		if seenMod[modSeqs[i]] {
			t.Errorf("duplicate modseq %d", modSeqs[i])
		}
		seenUID[uids[i]] = true
		seenMod[modSeqs[i]] = true
	}

	var usedBytes, uidNext int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT m.used_bytes, f.uidnext FROM mailboxes m
		   JOIN folders f ON f.mailbox_id = m.id
		  WHERE m.id = $1`, mboxID,
	).Scan(&usedBytes, &uidNext); err != nil {
		t.Fatalf("select quota/uidnext: %v", err)
	}
	if usedBytes != totalBytes {
		t.Errorf("used_bytes = %d, want %d", usedBytes, totalBytes)
	}
	if uidNext != n+1 {
		t.Errorf("uidnext = %d, want %d", uidNext, n+1)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestIngestPoisonousMessageRoundTrip proves the regression that used to
// exit EX_SOFTWARE is gone: un-encoded 8-bit headers, NUL bytes, and invalid
// UTF-8 in the body must survive parse → Ingest → SELECT as sanitized,
// valid UTF-8 rows rather than failing the INSERT.
func TestIngestPoisonousMessageRoundTrip(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw := []byte("From: exp\xe9diteur@poison.invalid\r\n" +
		"To: rcpt@poison.invalid\r\n" +
		"Subject: caf\xe9 \x00 menu \xff\xfe\r\n" +
		"Date: Mon, 15 Jun 2003 10:00:00 +0000\r\n" +
		"Message-ID: <poison@poison.invalid>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"nul\x00byte and invalid \xff sequence\r\n")

	msg, err := gcTestParser().Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	const tenant blob.Tenant = "poison"
	blobDate := time.Date(2003, 6, 15, 10, 0, 0, 0, time.UTC)
	bucket := blob.BucketFromTime(blobDate)
	w, err := store.NewWriter(blob.KindRaw, tenant, bucket)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sha, _, _, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	mboxID := gcMustMailbox(t, ctx, db, string(tenant))
	res, err := db.Ingest(ctx, storage.IngestParams{
		MailboxID:    mboxID,
		MailboxName:  string(tenant),
		FolderName:   "INBOX",
		EnvelopeTo:   "rcpt@poison.invalid",
		RawSHA256Hex: sha,
		RawSize:      int64(len(raw)),
		RawBlobDate:  blobDate,
		Message:      msg,
		InternalDate: &blobDate,
		EnsureBlobs:  ensureBlobsFunc(store, tenant, bucket, sha, raw, nil, nil),
	})
	if err != nil {
		t.Fatalf("Ingest of 8-bit/NUL message failed (the EX_SOFTWARE regression): %v", err)
	}

	var subject, textBody string
	if err := db.Pool().QueryRow(ctx,
		`SELECT subject, text_body FROM messages WHERE id = $1`,
		res.MessageID,
	).Scan(&subject, &textBody); err != nil {
		t.Fatalf("select message: %v", err)
	}
	if !utf8.ValidString(subject) || !utf8.ValidString(textBody) {
		t.Errorf("stored columns are not valid UTF-8: subject=%q text=%q", subject, textBody)
	}
	if subject == "" || textBody == "" {
		t.Errorf("sanitization erased content: subject=%q text=%q", subject, textBody)
	}

	// The raw blob on disk keeps the wire bytes byte-for-byte.
	rc, err := store.Open(blob.KindRaw, tenant, bucket, sha)
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	defer rc.Close()
	onDisk := make([]byte, len(raw)+1)
	n, _ := rc.Read(onDisk)
	if string(onDisk[:n]) != string(raw) {
		t.Error("raw blob does not match wire bytes")
	}
}
