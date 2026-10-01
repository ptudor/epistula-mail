package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// folderState is one folders row as the OPS-001/OPS-002 tests inspect it.
type folderState struct {
	id, uidValidity, uidNext, messages int64
	specialUse                         string // "" = NULL
}

func folderStates(t *testing.T, ctx context.Context, db *storage.DB, mailboxID int64) map[string]folderState {
	t.Helper()
	rows, err := db.Pool().Query(ctx,
		`SELECT f.name, f.id, f.uidvalidity, f.uidnext, COALESCE(f.special_use, ''),
		        (SELECT count(*) FROM messages m WHERE m.folder_id = f.id)
		   FROM folders f
		  WHERE f.mailbox_id = $1`, mailboxID)
	if err != nil {
		t.Fatalf("list folders: %v", err)
	}
	defer rows.Close()
	out := map[string]folderState{}
	for rows.Next() {
		var (
			name string
			s    folderState
		)
		if err := rows.Scan(&name, &s.id, &s.uidValidity, &s.uidNext, &s.specialUse, &s.messages); err != nil {
			t.Fatalf("scan folder: %v", err)
		}
		out[name] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list folders: %v", err)
	}
	return out
}

func folderStateNames(m map[string]folderState) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func uidValiditySeq(t *testing.T, ctx context.Context, db *storage.DB) int64 {
	t.Helper()
	var v int64
	if err := db.Pool().QueryRow(ctx, `SELECT last_value FROM folder_uidvalidity_seq`).Scan(&v); err != nil {
		t.Fatalf("read sequence: %v", err)
	}
	return v
}

// seedLegacyFolders inserts folder rows directly, the way a writer that
// predates OPS-001 left them: leaves without their ancestors.
func seedLegacyFolders(t *testing.T, ctx context.Context, db *storage.DB, mailboxID int64, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
			 VALUES ($1, $2, mail_next_uidvalidity(), 1)`, mailboxID, n,
		); err != nil {
			t.Fatalf("seed folder %q: %v", n, err)
		}
	}
}

// waitForMailboxLockWait returns once another session in this test database is
// blocked on a `... FROM mailboxes WHERE id = $1 FOR UPDATE` row lock. It fails
// if done fires first: the operation under test finished without waiting for
// the mailbox lock the test is holding.
func waitForMailboxLockWait(t *testing.T, ctx context.Context, db *storage.DB, done <-chan error) {
	t.Helper()
	for {
		select {
		case err := <-done:
			t.Fatalf("operation finished (err=%v) while the mailbox row lock was held; it must wait for it", err)
		default:
		}
		var waiting bool
		if err := db.Pool().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			                 WHERE datname = current_database()
			                   AND pid <> pg_backend_pid()
			                   AND wait_event_type = 'Lock'
			                   AND query LIKE '%FROM mailboxes WHERE id = $1 FOR UPDATE%')`,
		).Scan(&waiting); err != nil {
			t.Fatalf("observe lock wait: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newTestStore(t *testing.T) *blob.Store {
	t.Helper()
	store := blob.NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatalf("store init: %v", err)
	}
	return store
}

// TestIngestCreatesFolderAncestors is the OPS-001 regression for the writer.
//
// IMAP CREATE and COPY/MOVE created missing ancestors (RO5X-008), but Ingest —
// which every delivery and import goes through — created only the leaf. A
// Maildir++ import into `-folder Sent/2004/11-Nov` got that folder alone, so
// LIST showed a child whose parents did not exist.
func TestIngestCreatesFolderAncestors(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mailboxID := gcMustMailbox(t, ctx, db, "ancestors")
	store := newTestStore(t)
	p := ingestParamsFor(t, store, mailboxID, "ancestors", "Subject: nested\r\n\r\nbody\r\n")
	p.FolderName = "Sent/2004/11-Nov"
	res, err := db.Ingest(ctx, p)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	got := folderStates(t, ctx, db, mailboxID)
	want := []string{"Sent", "Sent/2004", "Sent/2004/11-Nov"}
	if names := folderStateNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("folders = %q, want %q", names, want)
	}
	leaf := got["Sent/2004/11-Nov"]
	if res.FolderID != leaf.id || leaf.messages != 1 || res.UIDValidity != leaf.uidValidity {
		t.Fatalf("message landed in folder %d (uidvalidity %d); leaf is %+v", res.FolderID, res.UIDValidity, leaf)
	}
	// Ancestors are created plain and empty, exactly as a CREATE of each.
	for _, name := range []string{"Sent", "Sent/2004"} {
		a := got[name]
		if a.messages != 0 || a.uidNext != 1 || a.specialUse != "" {
			t.Errorf("ancestor %q = %+v, want an empty plain folder", name, a)
		}
	}
	seen := map[int64]string{}
	for name, f := range got {
		if other, dup := seen[f.uidValidity]; dup {
			t.Errorf("%q and %q share uidvalidity %d", name, other, f.uidValidity)
		}
		seen[f.uidValidity] = name
	}
}

// TestNestedFolderCreationSpendsOneUIDValidityPerRow keeps RA6X-052 true now
// that creating one folder can create several rows: a value is spent for each
// row actually inserted, and none for a folder or ancestor that already
// exists, however much mail is delivered.
func TestNestedFolderCreationSpendsOneUIDValidityPerRow(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mailboxID := gcMustMailbox(t, ctx, db, "seqnest")
	store := newTestStore(t)
	deliver := func(folder string, n int) storage.IngestResult {
		t.Helper()
		p := ingestParamsFor(t, store, mailboxID, "seqnest", fmt.Sprintf("Subject: m%d\r\n\r\nbody %d\r\n", n, n))
		p.FolderName = folder
		res, err := db.Ingest(ctx, p)
		if err != nil {
			t.Fatalf("ingest into %q: %v", folder, err)
		}
		return res
	}
	spent := func(since int64) int64 { return uidValiditySeq(t, ctx, db) - since }

	if _, err := db.LookupOrCreateFolder(ctx, mailboxID, "a"); err != nil {
		t.Fatalf("create a: %v", err)
	}
	before := uidValiditySeq(t, ctx, db)
	deliver("a/b/c", 0)
	if n := spent(before); n != 2 {
		t.Fatalf("creating a/b/c under an existing a spent %d UIDVALIDITY values, want 2 (a/b, a/b/c)", n)
	}

	before = uidValiditySeq(t, ctx, db)
	for i := 1; i <= 5; i++ {
		deliver("a/b/c", i)
	}
	if n := spent(before); n != 0 {
		t.Fatalf("five deliveries into an existing nested folder spent %d values, want 0", n)
	}

	before = uidValiditySeq(t, ctx, db)
	sibling := deliver("a/b/d", 6)
	if n := spent(before); n != 1 {
		t.Fatalf("creating the sibling a/b/d spent %d values, want 1", n)
	}

	// LookupOrCreateFolder shares the path: an existing folder is found, not
	// recreated, and a new nested one gets its ancestors.
	before = uidValiditySeq(t, ctx, db)
	id, err := db.LookupOrCreateFolder(ctx, mailboxID, "a/b/d")
	if err != nil || id != sibling.FolderID {
		t.Fatalf("LookupOrCreateFolder(a/b/d) = %d, %v; want existing folder %d", id, err, sibling.FolderID)
	}
	if n := spent(before); n != 0 {
		t.Fatalf("looking up an existing folder spent %d values", n)
	}
	if _, err := db.LookupOrCreateFolder(ctx, mailboxID, "x/y/z"); err != nil {
		t.Fatalf("LookupOrCreateFolder(x/y/z): %v", err)
	}
	if n := spent(before); n != 3 {
		t.Fatalf("creating x/y/z from nothing spent %d values, want 3", n)
	}
	want := []string{"a", "a/b", "a/b/c", "a/b/d", "x", "x/y", "x/y/z"}
	if names := folderStateNames(folderStates(t, ctx, db, mailboxID)); !reflect.DeepEqual(names, want) {
		t.Fatalf("folders = %q, want %q", names, want)
	}
}

// TestConcurrentNestedIngestsCreateEachFolderOnce runs deliveries into two
// sibling folders that share missing ancestors at the same time. The mailbox
// row lock serializes them, so none fails, every row is created once, and no
// UIDVALIDITY is spent on a lost race.
func TestConcurrentNestedIngestsCreateEachFolderOnce(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	mailboxID := gcMustMailbox(t, ctx, db, "concnest")
	store := newTestStore(t)
	const n = 8
	params := make([]storage.IngestParams, n)
	for i := range params {
		params[i] = ingestParamsFor(t, store, mailboxID, "concnest", fmt.Sprintf("Subject: c%d\r\n\r\nbody %d\r\n", i, i))
		params[i].FolderName = []string{"Projects/2026/Q3", "Projects/2026/Q4"}[i%2]
	}

	before := uidValiditySeq(t, ctx, db)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range params {
		wg.Add(1)
		go func(p storage.IngestParams) {
			defer wg.Done()
			_, err := db.Ingest(ctx, p)
			errs <- err
		}(params[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent ingest: %v", err)
		}
	}

	got := folderStates(t, ctx, db, mailboxID)
	want := []string{"Projects", "Projects/2026", "Projects/2026/Q3", "Projects/2026/Q4"}
	if names := folderStateNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("folders = %q, want %q", names, want)
	}
	if got["Projects/2026/Q3"].messages != n/2 || got["Projects/2026/Q4"].messages != n/2 {
		t.Fatalf("messages Q3=%d Q4=%d, want %d each", got["Projects/2026/Q3"].messages, got["Projects/2026/Q4"].messages, n/2)
	}
	if spent := uidValiditySeq(t, ctx, db) - before; spent != 4 {
		t.Fatalf("spent %d UIDVALIDITY values creating 4 folders", spent)
	}
}

// TestImportIntoNestedFolderCreatesAncestors drives the command a Maildir++
// import runs: one `import -folder a/b/c` per Maildir++ folder.
func TestImportIntoNestedFolderCreatesAncestors(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mailboxID := gcMustMailbox(t, ctx, db, "importnest")
	root := t.TempDir()
	writeMaildirMessage(t, root, "0001", dryRunMsg("one"))
	writeMaildirMessage(t, root, "0002", dryRunMsg("two"))
	cfg := writeAdminConfig(t, dsn)
	const folder = "oldmbox/mail/IN/Old/20020417"

	// A dry run creates no folder, ancestors included (R-023).
	if code := runImport([]string{"-config", cfg, "-maildir", root, "-mailbox", "importnest", "-folder", folder, "-dry-run"}); code != EX_OK {
		t.Fatalf("dry-run import exit=%d, want EX_OK", code)
	}
	if got := folderStates(t, ctx, db, mailboxID); len(got) != 0 {
		t.Fatalf("dry-run created folders %q", folderStateNames(got))
	}

	if code := runImport([]string{"-config", cfg, "-maildir", root, "-mailbox", "importnest", "-folder", folder}); code != EX_OK {
		t.Fatalf("import exit=%d, want EX_OK", code)
	}
	got := folderStates(t, ctx, db, mailboxID)
	want := []string{
		"oldmbox",
		"oldmbox/mail",
		"oldmbox/mail/IN",
		"oldmbox/mail/IN/Old",
		folder,
	}
	if names := folderStateNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("folders = %q, want %q", names, want)
	}
	if got[folder].messages != 2 {
		t.Fatalf("leaf holds %d messages, want 2", got[folder].messages)
	}
	for _, name := range want[:4] {
		if got[name].messages != 0 {
			t.Errorf("ancestor %q holds %d messages", name, got[name].messages)
		}
	}
}

// TestRepairFolderAncestors is the OPS-001 backfill contract. Folders written
// before the fix exist without their parents; the repair creates exactly the
// missing ones, leaves every existing row (and the UIDVALIDITY a client has
// cached for it) alone, stays inside one mailbox, and is idempotent.
func TestRepairFolderAncestors(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mailboxID := gcMustMailbox(t, ctx, db, "legacy")
	seedLegacyFolders(t, ctx, db, mailboxID,
		"INBOX",
		"Sent",
		"Sent Messages",
		"Sent/2004/11-Nov",
		"Sent/2008to2015",
		"webmail-archive/2010-and-before/2010-inbox",
		"oldmbox/mail/IN/Old/20020417",
		"oldmbox/mail/OUT/200205/alice/project",
	)
	otherID := gcMustMailbox(t, ctx, db, "bystander")
	seedLegacyFolders(t, ctx, db, otherID, "Orphan/Child")

	wantCreated := []string{
		"Sent/2004",
		"oldmbox",
		"oldmbox/mail",
		"oldmbox/mail/IN",
		"oldmbox/mail/IN/Old",
		"oldmbox/mail/OUT",
		"oldmbox/mail/OUT/200205",
		"oldmbox/mail/OUT/200205/alice",
		"webmail-archive",
		"webmail-archive/2010-and-before",
	}
	before := folderStates(t, ctx, db, mailboxID)
	seqBefore := uidValiditySeq(t, ctx, db)

	// Dry run: the plan, and nothing else.
	planned, err := db.RepairFolderAncestors(ctx, mailboxID, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !reflect.DeepEqual(planned, wantCreated) {
		t.Fatalf("dry run planned %q\nwant %q", planned, wantCreated)
	}
	if after := folderStates(t, ctx, db, mailboxID); !reflect.DeepEqual(after, before) {
		t.Fatalf("dry run changed the folder set: %q", folderStateNames(after))
	}
	if uidValiditySeq(t, ctx, db) != seqBefore {
		t.Fatal("dry run consumed UIDVALIDITY values")
	}

	// Real run: exactly the plan.
	created, err := db.RepairFolderAncestors(ctx, mailboxID, false)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !reflect.DeepEqual(created, wantCreated) {
		t.Fatalf("repair created %q\nwant %q", created, wantCreated)
	}
	after := folderStates(t, ctx, db, mailboxID)
	if len(after) != len(before)+len(wantCreated) {
		t.Fatalf("folder count %d, want %d", len(after), len(before)+len(wantCreated))
	}
	for name, was := range before {
		if after[name] != was {
			t.Errorf("existing folder %q changed: %+v -> %+v", name, was, after[name])
		}
	}
	for _, name := range wantCreated {
		f, ok := after[name]
		if !ok {
			t.Errorf("ancestor %q missing after repair", name)
			continue
		}
		if f.uidNext != 1 || f.messages != 0 || f.specialUse != "" || f.uidValidity <= seqBefore {
			t.Errorf("ancestor %q = %+v, want a fresh plain empty folder", name, f)
		}
	}
	if spent := uidValiditySeq(t, ctx, db) - seqBefore; spent != int64(len(wantCreated)) {
		t.Fatalf("repair spent %d UIDVALIDITY values for %d folders", spent, len(wantCreated))
	}
	for name := range after {
		for _, a := range storage.FolderAncestors(name) {
			if _, ok := after[a]; !ok {
				t.Errorf("%q still lacks ancestor %q", name, a)
			}
		}
	}

	// The other mailbox was not touched.
	if names := folderStateNames(folderStates(t, ctx, db, otherID)); !reflect.DeepEqual(names, []string{"Orphan/Child"}) {
		t.Fatalf("repair of one mailbox changed another: %q", names)
	}

	// Idempotent: nothing left to do, nothing spent.
	seqAfter := uidValiditySeq(t, ctx, db)
	for _, dry := range []bool{true, false} {
		again, err := db.RepairFolderAncestors(ctx, mailboxID, dry)
		if err != nil || len(again) != 0 {
			t.Fatalf("second repair (dry=%v) = %q, %v; want nothing", dry, again, err)
		}
	}
	if uidValiditySeq(t, ctx, db) != seqAfter {
		t.Fatal("a repair with nothing to do consumed UIDVALIDITY values")
	}
}

// TestRepairFolderAncestorsLocksOnlyForRealRun pins the backfill to the
// canonical lock order. The real run waits for the mailbox row that delivery,
// COPY/MOVE and IMAP DELETE hold, and reads the folder set only once it has
// it; the dry run takes no lock at all, so previewing never stalls delivery.
func TestRepairFolderAncestorsLocksOnlyForRealRun(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mailboxID := gcMustMailbox(t, ctx, db, "lockedrepair")
	seedLegacyFolders(t, ctx, db, mailboxID, "Sent", "Sent/2004/11-Nov")

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT id FROM mailboxes WHERE id = $1 FOR UPDATE`, mailboxID); err != nil {
		t.Fatalf("hold mailbox lock: %v", err)
	}

	dryCtx, dryCancel := context.WithTimeout(ctx, 5*time.Second)
	planned, err := db.RepairFolderAncestors(dryCtx, mailboxID, true)
	dryCancel()
	if err != nil {
		t.Fatalf("dry run while the mailbox lock is held: %v", err)
	}
	if !reflect.DeepEqual(planned, []string{"Sent/2004"}) {
		t.Fatalf("dry run planned %q", planned)
	}

	var created []string
	done := make(chan error, 1)
	go func() {
		var err error
		created, err = db.RepairFolderAncestors(ctx, mailboxID, false)
		done <- err
	}()
	waitForMailboxLockWait(t, ctx, db, done)

	// The lock holder adds another orphan and commits. A repair that read the
	// folder set before it held the lock would miss it.
	if _, err := tx.Exec(ctx,
		`INSERT INTO folders (mailbox_id, name, uidvalidity, uidnext)
		 VALUES ($1, 'Archive/2026/Q1', mail_next_uidvalidity(), 1)`, mailboxID,
	); err != nil {
		t.Fatalf("insert under lock: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("repair: %v", err)
	}
	if want := []string{"Archive", "Archive/2026", "Sent/2004"}; !reflect.DeepEqual(created, want) {
		t.Fatalf("repair created %q, want %q", created, want)
	}
}

// TestAdminFolderRepairAncestors covers the operator command around the
// storage call: flag validation, the case-insensitive mailbox name every
// admin subcommand accepts (R-048), -dry-run writing nothing, and -all.
func TestAdminFolderRepairAncestors(t *testing.T) {
	db, dsn := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	jdoe := gcMustMailbox(t, ctx, db, "jdoe")
	seedLegacyFolders(t, ctx, db, jdoe, "INBOX", "Sent", "Sent/2004/11-Nov")
	other := gcMustMailbox(t, ctx, db, "other")
	seedLegacyFolders(t, ctx, db, other, "Lists/golang")
	cfg := writeAdminConfig(t, dsn)
	run := func(args ...string) int {
		return adminFolderRepairAncestors(append([]string{"-config", cfg}, args...))
	}

	for _, bad := range [][]string{
		{},                                 // neither
		{"-mailbox", "jdoe", "-all"},       // both
		{"-mailbox", "nobody"},             // unknown mailbox
		{"-mailbox", "   ", "-dry-run"},    // blank after trimming
		{"-mailbox", "jdoe", "-bogus"},     // unknown flag
		{"-all", "-mailbox", "", "-extra"}, // unknown flag with -all
	} {
		if code := run(bad...); code != EX_USAGE {
			t.Errorf("folder-repair-ancestors %q exit=%d, want EX_USAGE", bad, code)
		}
	}

	if code := run("-mailbox", "JDoe", "-dry-run"); code != EX_OK {
		t.Fatalf("dry run exit=%d", code)
	}
	if _, ok := folderStates(t, ctx, db, jdoe)["Sent/2004"]; ok {
		t.Fatal("-dry-run created a folder")
	}

	if code := run("-mailbox", " JDoe "); code != EX_OK {
		t.Fatalf("repair exit=%d", code)
	}
	if _, ok := folderStates(t, ctx, db, jdoe)["Sent/2004"]; !ok {
		t.Fatal("repair did not create Sent/2004")
	}
	if _, ok := folderStates(t, ctx, db, other)["Lists"]; ok {
		t.Fatal("-mailbox jdoe repaired another mailbox")
	}

	if code := run("-all", "-dry-run"); code != EX_OK {
		t.Fatalf("-all -dry-run exit=%d", code)
	}
	if code := run("-all"); code != EX_OK {
		t.Fatalf("-all exit=%d", code)
	}
	if _, ok := folderStates(t, ctx, db, other)["Lists"]; !ok {
		t.Fatal("-all did not repair the second mailbox")
	}
	if code := run("-all"); code != EX_OK {
		t.Fatalf("re-running -all exit=%d", code)
	}

	// A mailbox deleted after -all listed it surfaces as ErrNotFound, which the
	// command reports and skips rather than failing the whole run.
	if _, err := db.RepairFolderAncestors(ctx, -1, false); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("repair of a missing mailbox = %v, want ErrNotFound", err)
	}
}
