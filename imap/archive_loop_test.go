package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

func archiveTestConfig() *Config {
	cfg := DefaultConfig()
	cfg.Postgres.DSN = "postgres://localhost/m?sslmode=disable"
	return cfg
}

// TestArchiveConfigDefaults: both jobs are off unless enabled, and the
// defaults validate so check-config passes on a config that never mentions
// [archive].
func TestArchiveConfigDefaults(t *testing.T) {
	cfg := archiveTestConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.Archive.SortEnabled || cfg.Archive.PurgeEnabled {
		t.Fatal("an archive job is enabled by default")
	}
	interval, settle, retention := cfg.Archive.Durations()
	if interval != time.Minute || settle != 5*time.Minute || retention != 24*time.Hour {
		t.Fatalf("durations = %v %v %v", interval, settle, retention)
	}
	if !cfg.Archive.PurgeAllCopies {
		t.Fatal("purge_all_copies should default on: a deleted message must not survive as a copy")
	}
}

func TestArchiveConfigRejects(t *testing.T) {
	for name, mutate := range map[string]func(*ArchiveConfig){
		"zero interval":        func(a *ArchiveConfig) { a.Interval = "0s" },
		"bad interval":         func(a *ArchiveConfig) { a.Interval = "soon" },
		"negative settle":      func(a *ArchiveConfig) { a.SettleDelay = "-1m" },
		"negative retention":   func(a *ArchiveConfig) { a.TrashRetention = "-24h" },
		"confidence above one": func(a *ArchiveConfig) { a.MinConfidence = 1.1 },
		"negative confidence":  func(a *ArchiveConfig) { a.MinConfidence = -0.1 },
		"zero batch":           func(a *ArchiveConfig) { a.BatchSize = 0 },
		"huge batch":           func(a *ArchiveConfig) { a.BatchSize = 5001 },
	} {
		cfg := archiveTestConfig()
		mutate(&cfg.Archive)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "archive.") {
			t.Errorf("%s: err = %v; want an archive.* refusal", name, err)
		}
	}
}

// TestArchiveLoopRunsAndStops: with the sorter enabled the loop files an
// archived, classified message, and it stops — closing its done channel —
// when its context ends, so serve can wait for it before closing the pool.
func TestArchiveLoopRunsAndStops(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := db.Pool()

	var mailboxID, rootID, msgID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO mailboxes (name, password_hash) VALUES ('loop', 'x') RETURNING id`,
	).Scan(&mailboxID); err != nil {
		t.Fatal(err)
	}
	if err := db.RunTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM mailboxes WHERE id = $1 FOR UPDATE`, mailboxID); err != nil {
			return err
		}
		archiveUse := `\Archive`
		f, _, err := storage.EnsureFolder(ctx, tx, mailboxID, "Archive", &archiveUse)
		rootID = f.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReplaceArchiveCategories(ctx, mailboxID,
		[]storage.ArchiveCategory{{Key: "travel", Folder: "Archive/Travel"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO messages (folder_id, uid, raw_sha256, raw_size, internal_date, created_at)
		VALUES ($1, 1, '\x01', 10, now(), now() - interval '1 hour') RETURNING id`, rootID,
	).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE folders SET uidnext = 2 WHERE id = $1`, rootID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO message_classifications (message_id, category, confidence, model) VALUES ($1, 'travel', 0.9, 'm')`,
		msgID); err != nil {
		t.Fatal(err)
	}

	cfg := archiveTestConfig()
	cfg.Archive.SortEnabled = true
	cfg.Archive.PurgeEnabled = true
	cfg.Archive.Interval = "50ms"
	loopCtx, stopLoop := context.WithCancel(ctx)
	done := startArchiveLoop(loopCtx, pool, cfg, slog.Default())

	deadline := time.Now().Add(20 * time.Second)
	for {
		var folder string
		if err := pool.QueryRow(ctx,
			`SELECT f.name FROM messages m JOIN folders f ON f.id = m.folder_id WHERE m.id = $1`, msgID,
		).Scan(&folder); err != nil {
			t.Fatal(err)
		}
		if folder == "Archive/Travel" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("message still in %q after the loop ran", folder)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stopLoop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the archive loop did not stop when its context ended")
	}
}

// TestArchiveLoopDisabledStartsNothing: the default config starts no
// goroutine and hands back an already-closed channel.
func TestArchiveLoopDisabledStartsNothing(t *testing.T) {
	done := startArchiveLoop(context.Background(), nil, archiveTestConfig(), slog.Default())
	select {
	case <-done:
	default:
		t.Fatal("a disabled archive loop returned an open channel")
	}
}
