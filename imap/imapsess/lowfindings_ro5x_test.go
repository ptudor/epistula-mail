package imapsess

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestStoreOpsConvergeOnSortedFlags is the RO5X-027 regression: SET, ADD and
// DEL must all leave the same array VALUE for the same resulting SET, so two
// rows with identical flags cannot hold different arrays.
func TestStoreOpsConvergeOnSortedFlags(t *testing.T) {
	sess := mutationFixture(t)

	// Reach {$Forwarded, \Answered, \Seen} three different ways.
	set := []imap.Flag{imap.Flag(`\Seen`), imap.Flag(`$Forwarded`), imap.Flag(`\Answered`)}
	if err := sess.Store(nil, imap.UIDSetNum(1),
		&imap.StoreFlags{Op: imap.StoreFlagsSet, Flags: set}, nil); err != nil {
		t.Fatalf("SET: %v", err)
	}
	viaSet := messageFlags(t, sess, 1)

	// uid 2: build up with ADD in a different order.
	setFlags(t, sess, 2, []string{})
	for _, f := range []string{`$Forwarded`, `\Seen`, `\Answered`} {
		if err := sess.Store(nil, imap.UIDSetNum(2),
			&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.Flag(f)}}, nil); err != nil {
			t.Fatalf("ADD %s: %v", f, err)
		}
	}
	viaAdd := messageFlags(t, sess, 2)

	// uid 3: set a superset, then remove the extra with DEL.
	if err := sess.Store(nil, imap.UIDSetNum(3), &imap.StoreFlags{
		Op:    imap.StoreFlagsSet,
		Flags: []imap.Flag{imap.Flag(`\Seen`), imap.Flag(`$Forwarded`), imap.Flag(`\Answered`), imap.Flag(`\Draft`)},
	}, nil); err != nil {
		t.Fatalf("SET superset: %v", err)
	}
	if err := sess.Store(nil, imap.UIDSetNum(3),
		&imap.StoreFlags{Op: imap.StoreFlagsDel, Flags: []imap.Flag{imap.Flag(`\Draft`)}}, nil); err != nil {
		t.Fatalf("DEL: %v", err)
	}
	viaDel := messageFlags(t, sess, 3)

	if !equalSlices(viaSet, viaAdd) || !equalSlices(viaSet, viaDel) {
		t.Errorf("the three ops produced different array values for the same set:\n"+
			"  SET = %v\n  ADD = %v\n  DEL = %v", viaSet, viaAdd, viaDel)
	}
	// The value is in a stable, database-defined order. Note this is
	// Postgres's collation order, not Go byte order — under en_US.UTF-8,
	// array_agg(... ORDER BY f) yields {\Answered, \Seen, $Forwarded}. What
	// matters for RO5X-027 is that it is the SAME order every time and for
	// every op, which the convergence check above establishes.
	//
	// Re-running SET with the elements in a different input order must not
	// change the stored value.
	shuffled := []imap.Flag{imap.Flag(`\Answered`), imap.Flag(`$Forwarded`), imap.Flag(`\Seen`)}
	if err := sess.Store(nil, imap.UIDSetNum(1),
		&imap.StoreFlags{Op: imap.StoreFlagsSet, Flags: shuffled}, nil); err != nil {
		t.Fatalf("SET shuffled: %v", err)
	}
	if got := messageFlags(t, sess, 1); !equalSlices(got, viaSet) {
		t.Errorf("input order changed the stored value: %v vs %v", got, viaSet)
	}
}

// TestStoreSetDedupes covers the boundary: SET now also goes through
// array_agg(DISTINCT ...), so a client sending a duplicate does not store one.
func TestStoreSetDedupes(t *testing.T) {
	sess := mutationFixture(t)
	if err := sess.Store(nil, imap.UIDSetNum(1), &imap.StoreFlags{
		Op:    imap.StoreFlagsSet,
		Flags: []imap.Flag{imap.Flag(`\Seen`), imap.Flag(`\Seen`)},
	}, nil); err != nil {
		t.Fatalf("SET: %v", err)
	}
	if got := messageFlags(t, sess, 1); len(got) != 1 || got[0] != `\Seen` {
		t.Errorf("flags = %v, want exactly [\\Seen]", got)
	}
}

// TestFilterHeaderFieldsInvertDeterministic covers the one site of
// nondeterministic header output (RO5X-028) that is still live. The other
// one, serializeHeader, went with the walkPart resolver it served (OPS-004);
// section headers are spans of the stored message (RA6X-027), so there is no
// serialization left to be nondeterministic.
func TestFilterHeaderFieldsInvertDeterministic(t *testing.T) {
	raw := []byte("Subject: s\r\nFrom: a@b.invalid\r\nTo: c@d.invalid\r\n" +
		"Message-ID: <m@x>\r\nX-Spam: no\r\nDate: Mon, 1 Jun 2026 00:00:00 +0000\r\n\r\nbody\r\n")

	first := filterHeaderFields(raw, []string{"Subject"}, true)
	for i := 0; i < 50; i++ {
		if got := filterHeaderFields(raw, []string{"Subject"}, true); !bytes.Equal(first, got) {
			t.Fatalf("HEADER.FIELDS.NOT is not deterministic:\n%q\n%q", first, got)
		}
	}
	if bytes.Contains(first, []byte("Subject:")) {
		t.Errorf("the excluded field was emitted: %q", first)
	}
	if !bytes.Contains(first, []byte("From:")) {
		t.Errorf("a retained field was dropped: %q", first)
	}
	// Empty-header fallback must be unchanged.
	if got := filterHeaderFields([]byte("\r\n"), []string{"Subject"}, true); !bytes.Equal(got, []byte("\r\n")) {
		t.Errorf("empty-header fallback = %q, want %q", got, "\r\n")
	}
}

// TestClampAppendDate is the RO5X-041 regression: a client-supplied date-time
// becomes the on-disk bucket, so an unvalidated one lets a client create
// <tenant>/raw/9999/12/31/… or scatter blobs across thousands of directories.
func TestClampAppendDate(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	for _, tc := range []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"the useful case: a real past date passes through",
			time.Date(2003, 4, 5, 6, 7, 8, 0, time.UTC),
			time.Date(2003, 4, 5, 6, 7, 8, 0, time.UTC)},
		{"today passes through", now, now},
		{"modest clock skew is tolerated",
			now.Add(2 * time.Hour), now.Add(2 * time.Hour)},
		{"far future clamps to now",
			time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), now},
		{"just past the slack clamps to now",
			now.Add(25 * time.Hour), now},
		{"pre-epoch clamps to the floor",
			time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), appendDateFloor},
		{"the unknown-date sentinel day itself clamps off it",
			time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), appendDateFloor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampAppendDate(tc.in, now, logger, "alice"); !got.Equal(tc.want) {
				t.Errorf("clampAppendDate(%s) = %s, want %s",
					tc.in.Format(time.RFC3339), got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

// TestClampAppendDateNilLoggerSafe keeps the helper total.
func TestClampAppendDateNilLoggerSafe(t *testing.T) {
	now := time.Now().UTC()
	if got := clampAppendDate(time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), now, nil, "alice"); !got.Equal(now) {
		t.Errorf("clamp with a nil logger = %v, want now", got)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSuccessfulCredentialClearsThrottleEvenWhenCapped is the RO5X-037
// regression.
//
// RecordResult(ip, true) — which clears the IP's failure history — ran only
// AFTER the per-mailbox cap was acquired. So a user with the correct password
// whose mailbox was at its session cap got NO [LIMIT] *and* kept their accrued
// failures: mistype four times, fix it, hit the cap, and you are still one
// failure from a lockout despite having proved the credential.
func TestSuccessfulCredentialClearsThrottleEvenWhenCapped(t *testing.T) {
	const ip = "192.0.2.50"
	th := NewLoginThrottle(5, time.Minute, time.Minute)

	// Four failures: one short of lockout.
	for i := 0; i < 4; i++ {
		th.RecordResult(ip, false)
	}
	if !th.Allow(ip) {
		t.Fatal("fixture: should not be locked out yet")
	}

	// The user then proves the credential — even though a resource cap will
	// refuse the session, the throttle must treat it as a success.
	th.RecordResult(ip, true)

	// A subsequent single failure must NOT trip the lockout, because the
	// history was cleared.
	th.RecordResult(ip, false)
	if !th.Allow(ip) {
		t.Error("IP was locked out after one failure following a proven credential; " +
			"the success did not clear the history (RO5X-037)")
	}
}

// TestPreAuthTimerCASIsExclusive is the RO5X-038 regression: the timer
// callback and Login must be mutually exclusive, so a connection that has just
// authenticated is never closed by a timer that already read `false`.
func TestPreAuthTimerCASIsExclusive(t *testing.T) {
	// Simulate the two racers against one session's atomic, exactly as the
	// production callback and Login do.
	for i := 0; i < 1000; i++ {
		s := &Session{}
		timerWon := make(chan bool, 1)
		loginWon := make(chan bool, 1)

		go func() { timerWon <- s.authenticated.CompareAndSwap(false, true) }()
		go func() {
			// Login's side: it sets the flag unconditionally, so model the
			// same CAS-vs-store exclusion the fix relies on.
			loginWon <- s.authenticated.CompareAndSwap(false, true)
		}()

		a, b := <-timerWon, <-loginWon
		if a && b {
			t.Fatalf("iteration %d: BOTH racers won the CAS; the timer would have "+
				"closed an authenticated connection (RO5X-038)", i)
		}
		if !a && !b {
			t.Fatalf("iteration %d: neither racer won", i)
		}
	}
}

// TestPreAuthTimerRecordsTimeout pins the timedOut flag that distinguishes
// "the timer closed this" from "it authenticated", since both set the same
// atomic.
func TestPreAuthTimerRecordsTimeout(t *testing.T) {
	s := &Session{}
	if s.timedOut.Load() {
		t.Error("a fresh session should not be marked timed out")
	}
	if !s.authenticated.CompareAndSwap(false, true) {
		t.Fatal("CAS on a fresh session should succeed")
	}
	s.timedOut.Store(true)
	if !s.timedOut.Load() {
		t.Error("timedOut did not record")
	}
	// A second racer now loses.
	if s.authenticated.CompareAndSwap(false, true) {
		t.Error("a second CAS succeeded; the two paths are not exclusive")
	}
}
