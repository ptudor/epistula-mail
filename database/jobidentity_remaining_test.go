package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ptudor/epistula-mail/database/pgtest"
)

func TestEffectiveDestinationIdentity(t *testing.T) {
	t.Setenv("PGHOST", "environment.invalid")
	t.Setenv("PGPORT", "5439")
	if got := redactedDSN("dbname='mail archive' user=worker password='secret words'"); got != "environment.invalid:5439/mail archive" {
		t.Fatal(got)
	}
	a := redactedDSN("host=db.invalid port=5432 dbname=mail password=first")
	b := redactedDSN("host=db.invalid port=5433 dbname=mail password=first")
	if a == b {
		t.Fatal("keyword port omitted")
	}
	if a != redactedDSN("postgres://worker:rotated@db.invalid:5432/mail") {
		t.Fatal("password/DSN syntax changed identity")
	}
	service := filepath.Join(t.TempDir(), "pg_service.conf")
	if err := os.WriteFile(service, []byte("[archive]\nhost=service.invalid\nport=5444\ndbname=archive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", service)
	if got := redactedDSN("service=archive"); !strings.Contains(got, "service.invalid:5444/archive") {
		t.Fatal(got)
	}
}

func TestCheckpointRejectsRecreatedTargets(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx := context.Background()
	id := gcMustMailbox(t, ctx, db, "archive")
	job := jobIdentity{Kind: "maildir-import", Source: t.TempDir(), Mailbox: "archive", Folder: "INBOX"}
	if err := bindJob(ctx, db, &job); err != nil {
		t.Fatal(err)
	}
	if job.Targets["archive"].FolderID != 0 {
		t.Fatal("binding created an empty folder")
	}
	folder, err := db.LookupOrCreateFolder(ctx, id, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if err := job.bindCreatedFolder("archive", id, folder); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "checkpoint")
	state, _ := loadCheckpoint(path, false, job, false)
	state.LastKey = "cur/last"
	if err := state.save(path); err != nil {
		t.Fatal(err)
	}
	current := job
	if err := bindJob(ctx, db, &current); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCheckpoint(path, true, current, false); err != nil {
		t.Fatal("lazy folder resume", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM folders WHERE id=$1`, folder); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LookupOrCreateFolder(ctx, id, "INBOX"); err != nil {
		t.Fatal(err)
	}
	if err := bindJob(ctx, db, &current); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCheckpoint(path, true, current, false); err == nil {
		t.Fatal("recreated folder accepted")
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM mailboxes WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	gcMustMailbox(t, ctx, db, "archive")
	if err := bindJob(ctx, db, &current); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCheckpoint(path, true, current, false); err == nil {
		t.Fatal("recreated mailbox accepted")
	}
}

func TestCheckpointConcurrentAtomicPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint")
	job := jobIdentity{Kind: "maildir-import"}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			state, _ := loadCheckpoint(path, false, job, false)
			state.Count = int64(i)
			if err := state.save(path); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state checkpoint
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.Count < 0 || state.Count >= 12 || state.Fingerprint != job.Fingerprint() {
		t.Fatal(state)
	}
	state.Version = 2
	data, _ = json.Marshal(state)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCheckpoint(path, true, job, false); err == nil {
		t.Fatal("weak version 2 identity accepted")
	}
	if _, err := loadCheckpoint(path, true, job, true); err != nil {
		t.Fatal(err)
	}
}
