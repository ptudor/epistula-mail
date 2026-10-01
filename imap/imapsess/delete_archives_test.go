package imapsess

import (
	"context"
	"testing"
	"time"
)

// The delete-archives rule (delete_archives.go): with the feature on, EXPUNGE
// outside Trash, Drafts and Junk archives the last copy of a message instead of
// destroying it. mutationFixture holds INBOX uids 1-3 with uid 3 \Deleted.

func addFolder(t *testing.T, sess *Session, name, specialUse string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var su *string
	if specialUse != "" {
		su = &specialUse
	}
	var id int64
	if err := sess.be.Pool.QueryRow(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext, special_use)
		 VALUES ($1, $2, mail_next_uidvalidity(), 1, $3) RETURNING id`,
		sess.mailboxID, name, su,
	).Scan(&id); err != nil {
		t.Fatalf("insert folder %q: %v", name, err)
	}
	return id
}

// copyMessage puts a byte-identical copy of an INBOX message into folder.
func copyMessage(t *testing.T, sess *Session, inbox, uid, folder int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sess.be.Pool.Exec(ctx, `
		INSERT INTO messages (folder_id, uid, raw_sha256, raw_blob_date, raw_size, internal_date,
		                      headers, bodystructure, flags)
		SELECT $3, 100 + uid, raw_sha256, raw_blob_date, raw_size, internal_date, headers, bodystructure, '{}'
		  FROM messages WHERE folder_id = $1 AND uid = $2`, inbox, uid, folder); err != nil {
		t.Fatalf("copy message: %v", err)
	}
}

func folderOfMessageWithSHA(t *testing.T, sess *Session, uid int64) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := sess.be.Pool.Query(ctx, `
		SELECT f.name FROM messages m JOIN folders f ON f.id = m.folder_id
		 WHERE f.mailbox_id = $1 AND m.raw_sha256 = $2 ORDER BY f.name`, sess.mailboxID, fixtureSHA(uid))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestDeleteArchivesInboxExpungeArchives(t *testing.T) {
	sess := mutationFixture(t)
	sess.be.DeleteArchives = true
	addFolder(t, sess, "Archive", `\Archive`)
	startBytes := usedBytes(t, sess)

	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if messageExists(t, sess, sess.selectedFolderID, 3) {
		t.Fatal("uid 3 is still in INBOX after EXPUNGE")
	}
	if got := folderOfMessageWithSHA(t, sess, 3); len(got) != 1 || got[0] != "Archive" {
		t.Fatalf("uid 3's content is in %v; want it archived, not destroyed", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var flags []string
	if err := sess.be.Pool.QueryRow(ctx, `
		SELECT m.flags FROM messages m JOIN folders f ON f.id = m.folder_id
		 WHERE f.mailbox_id = $1 AND f.name = 'Archive'`, sess.mailboxID).Scan(&flags); err != nil {
		t.Fatal(err)
	}
	for _, f := range flags {
		if f == `\Deleted` {
			t.Fatal("the archived message kept its \\Deleted mark; the archive's next EXPUNGE would destroy it")
		}
	}
	if got := usedBytes(t, sess); got != startBytes {
		t.Fatalf("used_bytes %d → %d; archiving destroys nothing", startBytes, got)
	}
}

func TestDeleteArchivesCopyThenDeleteStillRemoves(t *testing.T) {
	sess := mutationFixture(t)
	sess.be.DeleteArchives = true
	addFolder(t, sess, "Archive", `\Archive`)
	// A client without MOVE drags uid 3 to Trash: COPY, then \Deleted and
	// EXPUNGE in INBOX. The INBOX row goes; nothing lands in Archive.
	trash := addFolder(t, sess, "Deleted Messages", `\Trash`)
	copyMessage(t, sess, sess.selectedFolderID, 3, trash)
	startBytes := usedBytes(t, sess)

	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if got := folderOfMessageWithSHA(t, sess, 3); len(got) != 1 || got[0] != "Deleted Messages" {
		t.Fatalf("uid 3's content is in %v; want only the Trash copy", got)
	}
	if got := usedBytes(t, sess); got != startBytes-100 {
		t.Fatalf("used_bytes = %d, want %d", got, startBytes-100)
	}
}

func TestDeleteArchivesTrashAndDraftsStillDestroy(t *testing.T) {
	for _, role := range []string{`\Trash`, `\Drafts`, `\Junk`} {
		t.Run(role, func(t *testing.T) {
			sess := mutationFixture(t)
			sess.be.DeleteArchives = true
			addFolder(t, sess, "Archive", `\Archive`)
			// Make the fixture's INBOX play the role under test.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := sess.be.Pool.Exec(ctx,
				`UPDATE folders SET special_use = $2 WHERE id = $1`, sess.selectedFolderID, role); err != nil {
				t.Fatal(err)
			}
			if err := sess.Expunge(nil, nil); err != nil {
				t.Fatalf("Expunge: %v", err)
			}
			if got := folderOfMessageWithSHA(t, sess, 3); len(got) != 0 {
				t.Fatalf("EXPUNGE in %s left the message in %v; it must destroy", role, got)
			}
		})
	}
}

func TestDeleteArchivesInsideArchiveKeeps(t *testing.T) {
	sess := mutationFixture(t)
	sess.be.DeleteArchives = true
	addFolder(t, sess, "Archive", `\Archive`)
	// Rename the fixture's INBOX into the archive tree: the selected folder is
	// now a category folder.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sess.be.Pool.Exec(ctx,
		`UPDATE folders SET name = 'Archive/Travel' WHERE id = $1`, sess.selectedFolderID); err != nil {
		t.Fatal(err)
	}
	sess.selectedFolderName = "Archive/Travel"
	modseqBefore := highestModSeq(t, sess, sess.selectedFolderID)

	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if !messageExists(t, sess, sess.selectedFolderID, 3) {
		t.Fatal("an archived message was destroyed by EXPUNGE in the archive")
	}
	for _, f := range messageFlags(t, sess, 3) {
		if f == `\Deleted` {
			t.Fatal("the kept message is still marked \\Deleted")
		}
	}
	if highestModSeq(t, sess, sess.selectedFolderID) <= modseqBefore {
		t.Fatal("clearing the mark did not advance highest_modseq; clients would not see it")
	}
}

func TestDeleteArchivesNeedsArchiveFolderAndSwitch(t *testing.T) {
	// No \Archive folder: EXPUNGE destroys as IMAP says.
	sess := mutationFixture(t)
	sess.be.DeleteArchives = true
	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := folderOfMessageWithSHA(t, sess, 3); len(got) != 0 {
		t.Fatalf("without an \\Archive folder the message went to %v", got)
	}
	// Switch off: destroys even with an \Archive folder.
	sess = mutationFixture(t)
	addFolder(t, sess, "Archive", `\Archive`)
	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := folderOfMessageWithSHA(t, sess, 3); len(got) != 0 {
		t.Fatalf("with delete_archives off the message went to %v", got)
	}
}

// TestDeleteArchivesHonoursConcurrentUndelete: RO5X-001's rescue still works
// when archiving — a message un-deleted in the race window stays in INBOX.
func TestDeleteArchivesHonoursConcurrentUndelete(t *testing.T) {
	sess := mutationFixture(t)
	sess.be.DeleteArchives = true
	addFolder(t, sess, "Archive", `\Archive`)
	setFlags(t, sess, 1, []string{`\Deleted`})
	sess.expungeRaceHook = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := sess.be.Pool.Exec(ctx,
			`UPDATE messages SET flags = '{}'::text[] WHERE folder_id = $1 AND uid = 1`,
			sess.selectedFolderID); err != nil {
			t.Errorf("concurrent un-delete: %v", err)
		}
	}
	if err := sess.Expunge(nil, nil); err != nil {
		t.Fatalf("Expunge: %v", err)
	}
	if !messageExists(t, sess, sess.selectedFolderID, 1) {
		t.Fatal("uid 1 left INBOX although its \\Deleted was cleared first")
	}
	if got := folderOfMessageWithSHA(t, sess, 3); len(got) != 1 || got[0] != "Archive" {
		t.Fatalf("uid 3 is in %v, want Archive", got)
	}
}
