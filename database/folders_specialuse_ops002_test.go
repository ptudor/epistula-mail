package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// specialUseOf reads one folder's attribute; "" means NULL. A stored empty
// string fails the test: LIST would emit it as an empty attribute.
func specialUseOf(t *testing.T, ctx context.Context, db *storage.DB, mailboxID int64, folder string) string {
	t.Helper()
	var su *string
	if err := db.Pool().QueryRow(ctx,
		`SELECT special_use FROM folders WHERE mailbox_id = $1 AND name = $2`, mailboxID, folder,
	).Scan(&su); err != nil {
		t.Fatalf("read special_use of %q: %v", folder, err)
	}
	if su == nil {
		return ""
	}
	if *su == "" {
		t.Fatalf("special_use of %q is an empty string, not NULL", folder)
	}
	return *su
}

// TestSetFolderSpecialUse is the OPS-002 storage contract.
//
// folders.special_use — what LIST reports as \Sent, \Drafts, ... — was only
// ever set by IMAP CREATE ... (USE (...)), so every imported folder came in
// without one. A Maildir++ tree can hold several sent-mail candidates ("Sent",
// "Sent Messages", "sent-mail"), so the operator chooses, and the store keeps
// the choice unambiguous: one folder per attribute per mailbox.
func TestSetFolderSpecialUse(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := gcMustMailbox(t, ctx, db, "roles")
	seedLegacyFolders(t, ctx, db, id, "INBOX", "Sent", "Sent Messages", "sent-mail", "Drafts")
	other := gcMustMailbox(t, ctx, db, "otherroles")
	seedLegacyFolders(t, ctx, db, other, "Sent")
	before := folderStates(t, ctx, db, id)

	if _, err := db.SetFolderSpecialUse(ctx, id, "Sent", `\Outbox`, false); !errors.Is(err, storage.ErrUnknownSpecialUse) {
		t.Fatalf(`\Outbox: err = %v, want ErrUnknownSpecialUse`, err)
	}
	// Folder names are exact: "sent" is not "Sent" (only INBOX folds case).
	if _, err := db.SetFolderSpecialUse(ctx, id, "sent", `\Sent`, false); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unknown folder: err = %v, want ErrNotFound", err)
	}

	// A dry run reports the change and writes nothing.
	change, err := db.SetFolderSpecialUse(ctx, id, "Sent", `\sent`, true)
	if err != nil || change != (storage.SpecialUseChange{Before: "", After: `\Sent`}) {
		t.Fatalf("dry run = %+v, %v", change, err)
	}
	if got := specialUseOf(t, ctx, db, id, "Sent"); got != "" {
		t.Fatalf("dry run wrote %q", got)
	}

	// Set, stored in canonical spelling; repeating it is a no-op.
	change, err = db.SetFolderSpecialUse(ctx, id, "Sent", `\sent`, false)
	if err != nil || change != (storage.SpecialUseChange{Before: "", After: `\Sent`}) {
		t.Fatalf("set = %+v, %v", change, err)
	}
	if got := specialUseOf(t, ctx, db, id, "Sent"); got != `\Sent` {
		t.Fatalf(`stored %q, want \Sent`, got)
	}
	if change, err = db.SetFolderSpecialUse(ctx, id, "Sent", `\Sent`, false); err != nil || change.Changed() {
		t.Fatalf("repeat = %+v, %v; want no change", change, err)
	}

	// A second holder in the same mailbox is refused, by the dry run too, and
	// the refusal names the current holder so the operator knows what to clear.
	for _, dry := range []bool{true, false} {
		_, err := db.SetFolderSpecialUse(ctx, id, "Sent Messages", `\SENT`, dry)
		if !errors.Is(err, storage.ErrSpecialUseTaken) || !strings.Contains(err.Error(), `"Sent"`) {
			t.Fatalf("second \\Sent (dry=%v): err = %v, want ErrSpecialUseTaken naming \"Sent\"", dry, err)
		}
	}
	if got := specialUseOf(t, ctx, db, id, "Sent Messages"); got != "" {
		t.Fatalf("refused assignment wrote %q", got)
	}
	// Another mailbox is independent.
	if _, err := db.SetFolderSpecialUse(ctx, other, "Sent", `\Sent`, false); err != nil {
		t.Fatalf("\\Sent in another mailbox: %v", err)
	}

	// Moving an attribute: clear it where it is, then set it where it belongs.
	change, err = db.SetFolderSpecialUse(ctx, id, "Sent", "", false)
	if err != nil || change != (storage.SpecialUseChange{Before: `\Sent`, After: ""}) {
		t.Fatalf("clear = %+v, %v", change, err)
	}
	if got := specialUseOf(t, ctx, db, id, "Sent"); got != "" {
		t.Fatalf("clear left %q", got)
	}
	if _, err := db.SetFolderSpecialUse(ctx, id, "Sent Messages", `\Sent`, false); err != nil {
		t.Fatalf("move \\Sent: %v", err)
	}
	// Clearing a folder that has none changes nothing.
	if change, err = db.SetFolderSpecialUse(ctx, id, "Sent", "", false); err != nil || change.Changed() {
		t.Fatalf("clear of none = %+v, %v", change, err)
	}

	// Different attributes coexist, and a folder may change its own.
	if _, err := db.SetFolderSpecialUse(ctx, id, "Drafts", `\Drafts`, false); err != nil {
		t.Fatalf("\\Drafts: %v", err)
	}
	change, err = db.SetFolderSpecialUse(ctx, id, "Drafts", `\Archive`, false)
	if err != nil || change != (storage.SpecialUseChange{Before: `\Drafts`, After: `\Archive`}) {
		t.Fatalf("change own attribute = %+v, %v", change, err)
	}

	after := folderStates(t, ctx, db, id)
	want := map[string]string{"INBOX": "", "Sent": "", "Sent Messages": `\Sent`, "sent-mail": "", "Drafts": `\Archive`}
	for name, su := range want {
		if after[name].specialUse != su {
			t.Errorf("%q special_use = %q, want %q", name, after[name].specialUse, su)
		}
		// The attribute is metadata: no folder's identity or UIDs move.
		b, a := before[name], after[name]
		if a.id != b.id || a.uidValidity != b.uidValidity || a.uidNext != b.uidNext {
			t.Errorf("%q identity changed: %+v -> %+v", name, b, a)
		}
	}
}

// TestSetFolderSpecialUseChecksUnderTheMailboxLock proves the one-holder rule
// survives two operators at once: the check runs only after the mailbox row
// lock is held, so an assignment that commits first is always seen.
func TestSetFolderSpecialUseChecksUnderTheMailboxLock(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := gcMustMailbox(t, ctx, db, "racingroles")
	seedLegacyFolders(t, ctx, db, id, "Sent", "Sent Messages")

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT id FROM mailboxes WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatalf("hold mailbox lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := db.SetFolderSpecialUse(ctx, id, "Sent", `\Sent`, false)
		done <- err
	}()
	waitForMailboxLockWait(t, ctx, db, done)

	// The other operator's assignment commits first.
	if _, err := tx.Exec(ctx,
		`UPDATE folders SET special_use = '\Sent' WHERE mailbox_id = $1 AND name = 'Sent Messages'`, id,
	); err != nil {
		t.Fatalf("competing assignment: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-done; !errors.Is(err, storage.ErrSpecialUseTaken) {
		t.Fatalf("second assignment = %v, want ErrSpecialUseTaken", err)
	}
	if got := specialUseOf(t, ctx, db, id, "Sent"); got != "" {
		t.Fatalf("both folders now carry \\Sent (Sent = %q)", got)
	}
}

// TestAdminFolderSetSpecialUse covers the operator command: flag validation,
// the attribute accepted with or without its backslash and in any case, the
// mailbox name folded like every admin subcommand (R-048), -dry-run, and the
// one-holder refusal.
func TestAdminFolderSetSpecialUse(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	id := gcMustMailbox(t, ctx, db, "clirole")
	seedLegacyFolders(t, ctx, db, id, "INBOX", "Sent", "Sent Messages", "Deleted Messages")
	cfg := writeAdminConfig(t, dsn)
	run := func(args ...string) int {
		return adminFolderSetSpecialUse(append([]string{"-config", cfg}, args...))
	}

	for _, bad := range [][]string{
		{"-mailbox", "clirole", "-folder", "Sent"},                            // neither -use nor -clear
		{"-mailbox", "clirole", "-folder", "Sent", "-use", `\Sent`, "-clear"}, // both
		{"-mailbox", "clirole", "-use", `\Sent`},                              // no folder
		{"-folder", "Sent", "-use", `\Sent`},                                  // no mailbox
		{"-mailbox", "clirole", "-folder", "Sent", "-use", `\Outbox`},         // not modelled
		{"-mailbox", "clirole", "-folder", "Sent", "-use", `\`},               // empty attribute
		{"-mailbox", "nobody", "-folder", "Sent", "-use", `\Sent`},            // unknown mailbox
		{"-mailbox", "clirole", "-folder", "sent", "-use", `\Sent`},           // folder names are exact
	} {
		if code := run(bad...); code != EX_USAGE {
			t.Errorf("folder-set-special-use %q exit=%d, want EX_USAGE", bad, code)
		}
	}
	for _, f := range []string{"INBOX", "Sent", "Sent Messages", "Deleted Messages"} {
		if got := specialUseOf(t, ctx, db, id, f); got != "" {
			t.Fatalf("a refused command set %q on %q", got, f)
		}
	}

	if code := run("-mailbox", "CliRole", "-folder", "Sent", "-use", "sent", "-dry-run"); code != EX_OK {
		t.Fatalf("dry run exit=%d", code)
	}
	if got := specialUseOf(t, ctx, db, id, "Sent"); got != "" {
		t.Fatalf("-dry-run wrote %q", got)
	}

	if code := run("-mailbox", "CliRole", "-folder", "Sent", "-use", "sent"); code != EX_OK {
		t.Fatalf("set exit=%d", code)
	}
	if got := specialUseOf(t, ctx, db, id, "Sent"); got != `\Sent` {
		t.Fatalf(`stored %q, want \Sent`, got)
	}
	if code := run("-mailbox", "clirole", "-folder", "Sent Messages", "-use", `\Sent`); code != EX_USAGE {
		t.Fatalf("second holder exit=%d, want EX_USAGE", code)
	}
	if code := run("-mailbox", "clirole", "-folder", "Sent", "-clear"); code != EX_OK {
		t.Fatalf("clear exit=%d", code)
	}
	if code := run("-mailbox", "clirole", "-folder", "Sent Messages", "-use", `\Sent`); code != EX_OK {
		t.Fatalf("set after clear exit=%d", code)
	}
	if code := run("-mailbox", "clirole", "-folder", "Deleted Messages", "-use", `\TRASH`); code != EX_OK {
		t.Fatalf("\\Trash exit=%d", code)
	}

	want := map[string]string{"INBOX": "", "Sent": "", "Sent Messages": `\Sent`, "Deleted Messages": `\Trash`}
	for f, su := range want {
		if got := specialUseOf(t, ctx, db, id, f); got != su {
			t.Errorf("%q special_use = %q, want %q", f, got, su)
		}
	}
}
