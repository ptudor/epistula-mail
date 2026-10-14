package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/jackc/pgx/v5"
	"github.com/ptudor/epistula-mail/database/storage"
	"testing"
)

func TestRetryDeliveryAfterArchiveMove(t *testing.T) {
	cfg, _, teardown := deliverFixture(t)
	db, code := adminOpenDB(cfg)
	if code != EX_OK {
		t.Fatal(code)
	}
	defer db.Close()
	defer teardown()
	raw := "From: sender@ra6x008.invalid\r\nSubject: moved retry\r\n\r\nbody\r\n"
	if code := deliverBytes(t, cfg, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid", raw); code != EX_OK {
		t.Fatal(code)
	}
	ctx := context.Background()
	var id, folder, mailbox int64
	if err := db.Pool().QueryRow(ctx, `SELECT m.id,m.folder_id,f.mailbox_id FROM messages m JOIN folders f ON f.id=m.folder_id`).Scan(&id, &folder, &mailbox); err != nil {
		t.Fatal(err)
	}
	if err := db.RunTx(ctx, func(tx pgx.Tx) error {
		_, err := storage.MoveMessages(ctx, tx, mailbox, []storage.MoveItem{{ID: id, FromFolderID: folder}}, "Archive")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code := deliverBytes(t, cfg, "ra6x008@ra6x008.invalid", "sender@ra6x008.invalid", raw); code != EX_OK {
		t.Fatal(code)
	}
	var messages, duplicates int
	if err := db.Pool().QueryRow(ctx, `SELECT (SELECT count(*) FROM messages),(SELECT count(*) FROM delivery_log WHERE outcome='duplicate')`).Scan(&messages, &duplicates); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || duplicates != 1 {
		t.Fatalf("messages=%d duplicate logs=%d", messages, duplicates)
	}
	// The same-name import wrapper remains folder scoped even with folderID=0.
	sum := sha256.Sum256([]byte(raw))
	got, err := db.RepairDuplicateInFolder(ctx, mailbox, "ra6x008", "INBOX", 0, hex.EncodeToString(sum[:]), func([]storage.StoredBlob) error { t.Fatal("import matched another folder"); return nil })
	if err != nil || got != 0 {
		t.Fatal(got, err)
	}
}
