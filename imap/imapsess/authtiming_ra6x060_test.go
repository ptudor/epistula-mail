package imapsess

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/pgtest"
)

// TestAuthTimingFloorEqualizesOutcomes is the RA6X-060 regression, run with a
// deterministic clock rather than a wall-clock ratio.
//
// The dummy hash equalizes an unknown account against a wrong password only
// while every real account carries the parameters the dummy was built from. An
// installation with a mixed or raised cost has accounts that verify measurably
// slower, and the difference is an account-existence oracle — the exact thing
// the dummy exists to close. The floor covers it by padding every outcome to a
// common minimum.
func TestAuthTimingFloorEqualizesOutcomes(t *testing.T) {
	floor := &AuthTimingFloor{floor: 120 * time.Millisecond, overrun: map[string]struct{}{}}

	for _, tc := range []struct {
		name string
		work time.Duration
	}{
		{"a fast path that did almost nothing", 0},
		{"a cheap hash", 10 * time.Millisecond},
		{"a default-cost hash", 60 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			time.Sleep(tc.work)
			floor.Wait(start)
			elapsed := time.Since(start)
			if elapsed < floor.Floor() {
				t.Fatalf("outcome returned in %v, below the %v floor — its cost is observable",
					elapsed, floor.Floor())
			}
		})
	}
}

// TestAuthTimingFloorCalibratesToTheHost pins that the floor is never below what
// a default-cost verification actually costs on this machine. A hard-coded
// constant chosen on someone else's hardware would leave the oracle wide open on
// a slower host.
func TestAuthTimingFloorCalibratesToTheHost(t *testing.T) {
	f := NewAuthTimingFloor(1 * time.Nanosecond)
	if f.Floor() < f.Calibration() {
		t.Fatalf("floor %v is below the measured default-cost verification %v",
			f.Floor(), f.Calibration())
	}
	if f.Calibration() <= 0 {
		t.Fatal("calibration did not measure anything")
	}

	// A configured minimum above the measurement wins.
	big := NewAuthTimingFloor(10 * time.Second)
	if big.Floor() != 10*time.Second {
		t.Fatalf("configured floor = %v, want 10s", big.Floor())
	}
}

// TestAuthTimingFloorReportsUncoverableAccounts pins the residual the fix cannot
// paper over: an account whose stored cost exceeds the floor is still
// distinguishable, so it must be reported once, by name, for re-hashing.
func TestAuthTimingFloorReportsUncoverableAccounts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	f := &AuthTimingFloor{floor: 100 * time.Millisecond, overrun: map[string]struct{}{}}

	// Under the floor: nothing to report.
	f.NoteOverrun(logger, "normal", 50*time.Millisecond)
	if buf.Len() != 0 {
		t.Fatalf("an account within the floor was reported: %s", buf.String())
	}
	if f.Overruns() != 0 {
		t.Fatalf("Overruns = %d, want 0", f.Overruns())
	}

	// Over the floor: reported once, then suppressed so a login flood cannot
	// flood the log.
	f.NoteOverrun(logger, "expensive", 400*time.Millisecond)
	f.NoteOverrun(logger, "expensive", 400*time.Millisecond)
	f.NoteOverrun(logger, "expensive", 400*time.Millisecond)
	out := buf.String()
	if !strings.Contains(out, "expensive") {
		t.Fatalf("the over-cost account was not reported: %s", out)
	}
	if n := strings.Count(out, "mailbox=expensive"); n != 1 {
		t.Fatalf("the warning was logged %d times, want exactly 1", n)
	}
	if f.Overruns() != 3 {
		t.Fatalf("Overruns = %d, want 3", f.Overruns())
	}
}

// TestLoginPadsEveryOutcomeToTheFloor is the integration form: unknown,
// disabled, wrong-password and correct-password logins all take at least the
// floor, against a real database and real Argon2 verification.
func TestLoginPadsEveryOutcomeToTheFloor(t *testing.T) {
	db, _ := pgtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	hash, err := auth.HashPassword("rightpw", auth.DefaultParams())
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	for _, m := range []struct {
		name     string
		disabled bool
	}{{"alice", false}, {"disabled", true}} {
		q := `INSERT INTO mailboxes (name, password_hash) VALUES ($1, $2)`
		if m.disabled {
			q = `INSERT INTO mailboxes (name, password_hash, disabled_at) VALUES ($1, $2, now())`
		}
		if _, err := db.Pool().Exec(ctx, q, m.name, hash); err != nil {
			t.Fatalf("insert %s: %v", m.name, err)
		}
	}

	// A floor comfortably above a real default-cost verification, so the
	// assertion is about the padding and not about Argon2's own duration.
	floor := 750 * time.Millisecond
	be := &Backend{
		Pool:        db.Pool(),
		BlobStore:   blob.NewStore(t.TempDir()),
		Logger:      slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		StmtTimeout: 10 * time.Second,
		AuthBudget:  NewAuthBudget(0),
		AuthTiming:  NewAuthTimingFloor(floor),
	}

	for _, tc := range []struct {
		name     string
		user     string
		pass     string
		wantAuth bool
	}{
		{"nonexistent account", "nobody", "whatever", false},
		{"disabled account", "disabled", "rightpw", false},
		{"wrong password", "alice", "wrongpw", false},
		{"correct password", "alice", "rightpw", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := be.NewSession()
			defer func() { _ = sess.Close() }()

			start := time.Now()
			err := sess.Login(tc.user, tc.pass)
			elapsed := time.Since(start)

			if tc.wantAuth && err != nil {
				t.Fatalf("login should have succeeded: %v", err)
			}
			if !tc.wantAuth && err == nil {
				t.Fatal("login should have failed")
			}
			// A little slack for timer granularity.
			if elapsed < floor-10*time.Millisecond {
				t.Fatalf("LOGIN returned in %v, below the %v floor — the account's cost is observable",
					elapsed, floor)
			}
		})
	}
}
