package main

import (
	"context"
	"os"
	"testing"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestMailboxAddCreatesInbox(t *testing.T) {
	db, dsn := pgtest.Open(t)
	f, err := os.CreateTemp(t.TempDir(), "password")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("a-long-test-password\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()
	if code := adminMailboxAdd([]string{"-config", writeAdminConfig(t, dsn), "-name", "newaccount", "-password-stdin"}); code != EX_OK {
		t.Fatalf("mailbox-add=%d", code)
	}
	var validity int64
	if err := db.Pool().QueryRow(context.Background(), `SELECT f.uidvalidity FROM folders f JOIN mailboxes m ON m.id=f.mailbox_id WHERE m.name='newaccount' AND f.name='INBOX'`).Scan(&validity); err != nil || validity <= 0 {
		t.Fatalf("validity=%d err=%v", validity, err)
	}
}
