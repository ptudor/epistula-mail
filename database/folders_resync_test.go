package main

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

func folderUIDValidity(t *testing.T, ctx context.Context, db *storage.DB, mailboxID int64, folder string) (id, uidvalidity, uidnext int64) {
	t.Helper()
	if err := db.Pool().QueryRow(ctx,
		`SELECT id, uidvalidity, uidnext FROM folders WHERE mailbox_id = $1 AND name = $2`, mailboxID, folder,
	).Scan(&id, &uidvalidity, &uidnext); err != nil {
		t.Fatalf("read folder %q: %v", folder, err)
	}
	return id, uidvalidity, uidnext
}

// TestResyncFolder: a resync gives the folder a strictly greater UIDVALIDITY
// (RFC 9051 §2.3.1.1), changes nothing else, and announces the change on
// mail_arrived so IDLE sessions see it at once. On junia on 2026-11-03 this was
// the server-side remedy for an iPhone that kept a message hidden.
func TestResyncFolder(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := gcMustMailbox(t, ctx, db, "resync")
	seedLegacyFolders(t, ctx, db, id, "INBOX", "Sent")
	inboxID, before, uidnext := folderUIDValidity(t, ctx, db, id, "INBOX")
	_, sentBefore, _ := folderUIDValidity(t, ctx, db, id, "Sent")

	// A dry run reports and changes nothing.
	r, err := db.ResyncFolder(ctx, id, "INBOX", true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if r.FolderID != inboxID || r.Before != before || r.After != 0 || r.Messages != 0 {
		t.Errorf("dry run = %+v, want folder %d before %d after 0 messages 0", r, inboxID, before)
	}
	if _, got, _ := folderUIDValidity(t, ctx, db, id, "INBOX"); got != before {
		t.Fatalf("dry run changed UIDVALIDITY %d -> %d", before, got)
	}

	// The notification reaches a listener, as an IDLE session's does.
	conn, err := db.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN mail_arrived`); err != nil {
		t.Fatal(err)
	}

	r, err = db.ResyncFolder(ctx, id, "INBOX", false)
	if err != nil {
		t.Fatalf("resync: %v", err)
	}
	if r.Before != before || r.After <= before {
		t.Fatalf("resync = %+v, want a UIDVALIDITY above %d", r, before)
	}
	gotID, after, gotNext := folderUIDValidity(t, ctx, db, id, "INBOX")
	if gotID != inboxID || after != r.After || gotNext != uidnext {
		t.Errorf("folder after resync: id %d uidvalidity %d uidnext %d, want %d %d %d",
			gotID, after, gotNext, inboxID, r.After, uidnext)
	}
	if _, got, _ := folderUIDValidity(t, ctx, db, id, "Sent"); got != sentBefore {
		t.Errorf("resync of INBOX changed Sent's UIDVALIDITY %d -> %d", sentBefore, got)
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	n, err := conn.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("no mail_arrived notification: %v", err)
	}
	if n.Channel != "mail_arrived" || n.Payload != strconv.FormatInt(inboxID, 10) {
		t.Errorf("notification %s %q, want mail_arrived %d", n.Channel, n.Payload, inboxID)
	}

	if _, err := db.ResyncFolder(ctx, id, "inbox-typo", false); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("unknown folder: err = %v, want ErrNotFound", err)
	}
}

// TestResyncFolderAboveTheSequence: a folder created before migration 018
// took its UIDVALIDITY from the clock and can sit above the shared sequence.
// Its new value must still exceed its old one, and the sequence must move
// past it so later folders get fresh values.
func TestResyncFolderAboveTheSequence(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := gcMustMailbox(t, ctx, db, "clockera")
	seedLegacyFolders(t, ctx, db, id, "INBOX")
	var seq int64
	if err := db.Pool().QueryRow(ctx, `SELECT last_value FROM folder_uidvalidity_seq`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	high := seq + 100000
	if _, err := db.Pool().Exec(ctx,
		`UPDATE folders SET uidvalidity = $1 WHERE mailbox_id = $2 AND name = 'INBOX'`, high, id,
	); err != nil {
		t.Fatal(err)
	}

	r, err := db.ResyncFolder(ctx, id, "INBOX", false)
	if err != nil {
		t.Fatalf("resync: %v", err)
	}
	if r.Before != high || r.After <= high {
		t.Fatalf("resync = %+v, want a UIDVALIDITY above %d", r, high)
	}
	seedLegacyFolders(t, ctx, db, id, "Later")
	if _, later, _ := folderUIDValidity(t, ctx, db, id, "Later"); later <= r.After {
		t.Errorf("a folder created after the resync got UIDVALIDITY %d, not above %d", later, r.After)
	}
}

// TestResyncFolderUnknownMailbox: a mailbox id that no longer exists is
// ErrNotFound from the mailbox lock, before any folder is looked up.
func TestResyncFolderUnknownMailbox(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := db.ResyncFolder(ctx, 987654321, "INBOX", false); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
