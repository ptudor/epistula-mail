package imapsess

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// folderNames returns every folder name in the session's mailbox, sorted.
func folderNames(t *testing.T, sess *Session) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := sess.be.Pool.Query(ctx,
		`SELECT name FROM folders WHERE mailbox_id = $1 ORDER BY name`, sess.mailboxID)
	if err != nil {
		t.Fatalf("list folders: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// folderUIDValidity reads one folder's uidvalidity.
func folderUIDValidity(t *testing.T, sess *Session, name string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var v int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT uidvalidity FROM folders WHERE mailbox_id = $1 AND name = $2`,
		sess.mailboxID, name,
	).Scan(&v); err != nil {
		t.Fatalf("uidvalidity for %q: %v", name, err)
	}
	return v
}

// TestRenameMovesDescendants is the RO5X-008 RENAME regression.
//
// RFC 3501 §6.3.5 requires RENAME to rename inferior hierarchical names too.
// Renaming only the exact row left `Archive/2026` parented to a name that no
// longer existed, which clients render as a phantom node or drop entirely.
func TestRenameMovesDescendants(t *testing.T) {
	sess := mutationFixture(t)
	for _, n := range []string{"Archive", "Archive/2026", "Archive/2026/Q1"} {
		if err := sess.Create(n, nil); err != nil {
			t.Fatalf("Create %q: %v", n, err)
		}
	}
	before := map[string]int64{}
	for _, n := range []string{"Archive", "Archive/2026", "Archive/2026/Q1"} {
		before[n] = folderUIDValidity(t, sess, n)
	}

	if err := sess.Rename("Archive", "Old", nil); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	got := folderNames(t, sess)
	for _, want := range []string{"Old", "Old/2026", "Old/2026/Q1"} {
		if !contains(got, want) {
			t.Errorf("folder %q missing after rename; got %v", want, got)
		}
	}
	for _, gone := range []string{"Archive", "Archive/2026", "Archive/2026/Q1"} {
		if contains(got, gone) {
			t.Errorf("folder %q survived the rename; got %v", gone, got)
		}
	}

	// Every renamed row gets a fresh, distinct uidvalidity (R-062), so
	// clients discard cached state for the whole subtree.
	seen := map[int64]bool{}
	for _, n := range []string{"Old", "Old/2026", "Old/2026/Q1"} {
		v := folderUIDValidity(t, sess, n)
		oldName := "Archive" + n[len("Old"):]
		if v == before[oldName] {
			t.Errorf("%q kept uidvalidity %d across the rename", n, v)
		}
		if seen[v] {
			t.Errorf("uidvalidity %d reused within one rename", v)
		}
		seen[v] = true
	}
}

// TestRenameKeepsMessagesReachable proves the subtree's mail survives the move.
func TestRenameKeepsMessagesReachable(t *testing.T) {
	sess := mutationFixture(t)
	for _, n := range []string{"Archive", "Archive/2026"} {
		if err := sess.Create(n, nil); err != nil {
			t.Fatalf("Create %q: %v", n, err)
		}
	}
	// Put a message in the child.
	if _, err := sess.Copy(imap.UIDSetNum(1), "Archive/2026"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	countIn := func(folder string) int {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var n int
		if err := sess.be.Pool.QueryRow(ctx,
			`SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
			  WHERE f.mailbox_id = $1 AND f.name = $2`, sess.mailboxID, folder,
		).Scan(&n); err != nil {
			t.Fatalf("count in %q: %v", folder, err)
		}
		return n
	}
	if countIn("Archive/2026") != 1 {
		t.Fatalf("fixture: expected 1 message in Archive/2026")
	}

	if err := sess.Rename("Archive", "Old", nil); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := countIn("Old/2026"); got != 1 {
		t.Errorf("message count in Old/2026 = %d, want 1 — mail was stranded", got)
	}
}

// TestRenameRefusesCollisionAtomically covers the collision edge case: a
// rename that would collide with an existing descendant name must fail before
// any row is touched.
func TestRenameRefusesCollisionAtomically(t *testing.T) {
	sess := mutationFixture(t)
	for _, n := range []string{"Archive", "Archive/2026", "Old/2026"} {
		if err := sess.Create(n, nil); err != nil {
			t.Fatalf("Create %q: %v", n, err)
		}
	}
	before := folderNames(t, sess)

	// Renaming Archive → Old would move Archive/2026 onto the existing
	// Old/2026.
	err := sess.Rename("Archive", "Old", nil)
	if err == nil {
		t.Fatal("Rename onto a colliding descendant succeeded")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeAlreadyExists {
		t.Errorf("err = %v, want ALREADYEXISTS", err)
	}

	// Nothing moved.
	after := folderNames(t, sess)
	if len(before) != len(after) {
		t.Fatalf("folder set changed: %v -> %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("folder set changed: %v -> %v", before, after)
			break
		}
	}
}

// TestDeleteRefusesWhenInferiorsExist is the RO5X-008 DELETE regression.
func TestDeleteRefusesWhenInferiorsExist(t *testing.T) {
	sess := mutationFixture(t)
	for _, n := range []string{"Old", "Old/2026", "Old/2026/Q1"} {
		if err := sess.Create(n, nil); err != nil {
			t.Fatalf("Create %q: %v", n, err)
		}
	}

	if err := sess.Delete("Old"); err == nil {
		t.Fatal("DELETE of a folder with children succeeded")
	}
	for _, n := range []string{"Old", "Old/2026", "Old/2026/Q1"} {
		if !contains(folderNames(t, sess), n) {
			t.Errorf("folder %q vanished from a refused DELETE", n)
		}
	}

	// The leaf has no inferiors, so it deletes.
	if err := sess.Delete("Old/2026/Q1"); err != nil {
		t.Fatalf("DELETE of a leaf: %v", err)
	}
	if contains(folderNames(t, sess), "Old/2026/Q1") {
		t.Error("leaf folder survived its DELETE")
	}
	// And now its parent is a leaf too.
	if err := sess.Delete("Old/2026"); err != nil {
		t.Fatalf("DELETE of the newly-childless parent: %v", err)
	}
}

// TestCreateMakesMissingAncestors is the RO5X-008 CREATE regression: creating
// `a/b/c` used to leave a child with no ancestors in LIST.
func TestCreateMakesMissingAncestors(t *testing.T) {
	sess := mutationFixture(t)

	if err := sess.Create("Projects/2026/Q3", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := folderNames(t, sess)
	for _, want := range []string{"Projects", "Projects/2026", "Projects/2026/Q3"} {
		if !contains(got, want) {
			t.Errorf("ancestor %q was not created; got %v", want, got)
		}
	}
}

// TestCreateAlreadyExistsOnlyForLeaf covers the rule that a pre-existing
// ancestor is normal and must not fail the command; only a pre-existing LEAF
// is ALREADYEXISTS.
func TestCreateAlreadyExistsOnlyForLeaf(t *testing.T) {
	sess := mutationFixture(t)

	if err := sess.Create("Projects", nil); err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	// Ancestor exists — creating the child must still succeed.
	if err := sess.Create("Projects/2026", nil); err != nil {
		t.Fatalf("Create child under an existing ancestor: %v", err)
	}
	// Leaf exists — ALREADYEXISTS.
	err := sess.Create("Projects/2026", nil)
	if err == nil {
		t.Fatal("re-creating an existing leaf succeeded")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeAlreadyExists {
		t.Errorf("err = %v, want ALREADYEXISTS", err)
	}
}

// The ancestor helper's edge cases are covered where it now lives, by
// TestFolderAncestors in epistula-database's storage package (OPS-001).

// TestInboxStillUndeletableAndUnrenamable pins the "must not change" clause.
func TestInboxStillUndeletableAndUnrenamable(t *testing.T) {
	sess := mutationFixture(t)
	if err := sess.Delete("INBOX"); err == nil {
		t.Error("INBOX was deleted")
	}
	if err := sess.Rename("INBOX", "Elsewhere", nil); err == nil {
		t.Error("INBOX was renamed")
	}
}

// TestRenameFixesSelectedDescendantName covers the descendant edge case: the
// selected-folder fix-up must fire when the SELECTed folder is a *descendant*
// of the renamed parent, not only when it is the parent itself.
func TestRenameFixesSelectedDescendantName(t *testing.T) {
	sess := mutationFixture(t)
	for _, n := range []string{"Archive", "Archive/2026"} {
		if err := sess.Create(n, nil); err != nil {
			t.Fatalf("Create %q: %v", n, err)
		}
	}
	// Pretend the child is selected.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var childID int64
	if err := sess.be.Pool.QueryRow(ctx,
		`SELECT id FROM folders WHERE mailbox_id = $1 AND name = 'Archive/2026'`,
		sess.mailboxID).Scan(&childID); err != nil {
		t.Fatalf("child id: %v", err)
	}
	sess.selectedFolderID = childID
	sess.selectedFolderName = "Archive/2026"

	if err := sess.Rename("Archive", "Old", nil); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if sess.selectedFolderName != "Old/2026" {
		t.Errorf("selectedFolderName = %q, want %q", sess.selectedFolderName, "Old/2026")
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
