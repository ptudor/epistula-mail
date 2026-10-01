package imapsess

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestGuardConvertsPanicToServerBug is the RO5X-002 barrier regression: a
// panic inside a Session method must become a tagged NO [SERVERBUG] rather
// than unwinding into go-imap (where the client sees a bare EOF).
func TestGuardConvertsPanicToServerBug(t *testing.T) {
	var logBuf bytes.Buffer
	sess := &Session{be: &Backend{
		Logger: slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError})),
	}}

	boom := func() (err error) {
		defer sess.guard("FETCH", &err)
		panic("synthetic failure")
	}

	err := boom()
	if err == nil {
		t.Fatal("guard swallowed the panic without producing an error")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("err = %T (%v), want *imap.Error", err, err)
	}
	if imapErr.Type != imap.StatusResponseTypeNo {
		t.Errorf("Type = %v, want NO", imapErr.Type)
	}
	if imapErr.Code != imap.ResponseCodeServerBug {
		t.Errorf("Code = %v, want SERVERBUG", imapErr.Code)
	}
	if !strings.Contains(imapErr.Text, "FETCH") {
		t.Errorf("Text = %q, want it to name the failing command", imapErr.Text)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "session panic") {
		t.Errorf("panic was not logged at ERROR; log = %q", logged)
	}
	if !strings.Contains(logged, "synthetic failure") {
		t.Errorf("log does not carry the panic value; log = %q", logged)
	}
}

// TestGuardPassesThroughNormalReturns proves the barrier is inert on the
// happy path and on ordinary error returns.
func TestGuardPassesThroughNormalReturns(t *testing.T) {
	sess := &Session{be: &Backend{Logger: slog.Default()}}

	ok := func() (err error) {
		defer sess.guard("NOOP", &err)
		return nil
	}
	if err := ok(); err != nil {
		t.Errorf("guard altered a nil return: %v", err)
	}

	sentinel := errors.New("ordinary failure")
	failing := func() (err error) {
		defer sess.guard("NOOP", &err)
		return sentinel
	}
	if err := failing(); !errors.Is(err, sentinel) {
		t.Errorf("guard replaced an ordinary error: got %v, want %v", err, sentinel)
	}
}

// TestGuardSurvivesNilLogger keeps the barrier total: it must not panic
// while handling a panic, even on a Session with no backend logger.
func TestGuardSurvivesNilLogger(t *testing.T) {
	sess := &Session{}
	boom := func() (err error) {
		defer sess.guard("LOGIN", &err)
		panic("no logger here")
	}
	if err := boom(); err == nil {
		t.Fatal("expected an imap.Error from the barrier")
	}
}
