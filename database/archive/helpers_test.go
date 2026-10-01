package archive_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/storage"
)

// fixture builds mailbox state directly in SQL. The archive code never reads
// blob files, so rows are all these tests need.
type fixture struct {
	t   *testing.T
	ctx context.Context
	db  *storage.DB
	seq int
}

func newFixture(t *testing.T, ctx context.Context, db *storage.DB) *fixture {
	return &fixture{t: t, ctx: ctx, db: db}
}

func (f *fixture) mailbox(name string) int64 {
	f.t.Helper()
	var id int64
	if err := f.db.Pool().QueryRow(f.ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ($1, 'x') RETURNING id`, name,
	).Scan(&id); err != nil {
		f.t.Fatalf("insert mailbox: %v", err)
	}
	return id
}

// folder creates (or finds) a folder, with its ancestors, and optionally a
// special-use attribute.
func (f *fixture) folder(mailboxID int64, name, specialUse string) int64 {
	f.t.Helper()
	var id int64
	err := f.db.RunTx(f.ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(f.ctx, `SELECT id FROM mailboxes WHERE id = $1 FOR UPDATE`, mailboxID); err != nil {
			return err
		}
		var su *string
		if specialUse != "" {
			su = &specialUse
		}
		folder, _, err := storage.EnsureFolder(f.ctx, tx, mailboxID, name, su)
		id = folder.ID
		return err
	})
	if err != nil {
		f.t.Fatalf("ensure folder %q: %v", name, err)
	}
	return id
}

type msgOpt struct {
	content   string // raw content key; identical keys share raw_sha256
	internal  time.Time
	sentLocal *time.Time // the sender's calendar date (sent_date_local)
	flags     []string
	size      int64
}

// message inserts a message row at the folder's next UID and returns its id.
func (f *fixture) message(folderID int64, o msgOpt) int64 {
	f.t.Helper()
	f.seq++
	if o.content == "" {
		o.content = fmt.Sprintf("message-%d", f.seq)
	}
	if o.internal.IsZero() {
		o.internal = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(f.seq) * time.Hour)
	}
	if o.flags == nil {
		o.flags = []string{}
	}
	if o.size == 0 {
		o.size = 100
	}
	sum := sha256.Sum256([]byte(o.content))
	var id int64
	err := f.db.RunTx(f.ctx, func(tx pgx.Tx) error {
		var uid int64
		if err := tx.QueryRow(f.ctx,
			`UPDATE folders SET uidnext = uidnext + 1, highest_modseq = highest_modseq + 1
			  WHERE id = $1 RETURNING uidnext - 1`, folderID,
		).Scan(&uid); err != nil {
			return err
		}
		if err := tx.QueryRow(f.ctx,
			`INSERT INTO messages (folder_id, uid, raw_sha256, raw_size, internal_date, subject, flags, sent_date_local)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
			folderID, uid, sum[:], o.size, o.internal, o.content, o.flags, o.sentLocal,
		).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(f.ctx,
			`UPDATE mailboxes SET used_bytes = used_bytes + $1
			  WHERE id = (SELECT mailbox_id FROM folders WHERE id = $2)`, o.size, folderID)
		return err
	})
	if err != nil {
		f.t.Fatalf("insert message: %v", err)
	}
	return id
}

func (f *fixture) classify(messageID int64, category string, confidence float32) {
	f.t.Helper()
	if _, err := f.db.Pool().Exec(f.ctx,
		`INSERT INTO message_classifications (message_id, category, confidence, model)
		 VALUES ($1, $2, $3, 'test-model')
		 ON CONFLICT (message_id) DO UPDATE SET category = EXCLUDED.category, confidence = EXCLUDED.confidence`,
		messageID, category, confidence,
	); err != nil {
		f.t.Fatalf("classify: %v", err)
	}
}

func (f *fixture) categories(mailboxID int64, cats ...storage.ArchiveCategory) {
	f.t.Helper()
	if _, err := f.db.ReplaceArchiveCategories(f.ctx, mailboxID, cats, false); err != nil {
		f.t.Fatalf("replace categories: %v", err)
	}
}

// age backdates the time a message entered its folder.
func (f *fixture) age(messageID int64, d time.Duration) {
	f.t.Helper()
	if _, err := f.db.Pool().Exec(f.ctx,
		`UPDATE messages SET created_at = now() - make_interval(secs => $2) WHERE id = $1`,
		messageID, d.Seconds(),
	); err != nil {
		f.t.Fatalf("age message: %v", err)
	}
}

type placement struct {
	folder string
	uid    int64
}

func (f *fixture) where(messageID int64) (placement, bool) {
	f.t.Helper()
	var p placement
	err := f.db.Pool().QueryRow(f.ctx,
		`SELECT f.name, m.uid FROM messages m JOIN folders f ON f.id = m.folder_id WHERE m.id = $1`,
		messageID,
	).Scan(&p.folder, &p.uid)
	if err == pgx.ErrNoRows {
		return placement{}, false
	}
	if err != nil {
		f.t.Fatalf("locate message %d: %v", messageID, err)
	}
	return p, true
}

func (f *fixture) mustBeIn(messageID int64, folder string) {
	f.t.Helper()
	p, ok := f.where(messageID)
	if !ok {
		f.t.Fatalf("message %d no longer exists; want it in %q", messageID, folder)
	}
	if p.folder != folder {
		f.t.Fatalf("message %d is in %q; want %q", messageID, p.folder, folder)
	}
}

func (f *fixture) mustBeGone(messageID int64) {
	f.t.Helper()
	if p, ok := f.where(messageID); ok {
		f.t.Fatalf("message %d still exists in %q", messageID, p.folder)
	}
}

func (f *fixture) scalar(query string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.db.Pool().QueryRow(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}
