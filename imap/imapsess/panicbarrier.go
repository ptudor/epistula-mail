package imapsess

import (
	"log/slog"
	"runtime/debug"

	"github.com/emersion/go-imap/v2"
)

// guard is the per-command panic barrier (RO5X-002).
//
// Every exported Session method defers it with a pointer to its named error
// return. A panic inside the method is logged at ERROR with a stack trace and
// converted into `NO [SERVERBUG]`, so the client gets a well-formed tagged
// response naming the failing command instead of an abrupt connection drop.
//
// go-imap's imapserver.(*Conn).serve already recovers panics per connection
// (conn.go:98 in v2.0.0-beta.8), so an unguarded panic kills one connection
// rather than the daemon — verified empirically. That makes this barrier
// defence-in-depth for availability, but it is the difference between a
// client seeing EOF mid-command and seeing a diagnosable protocol error, and
// it keeps the session's own invariants (mailbox slot accounting, selected
// state) from being skipped by an unwind through library code.
//
// The pattern this follows lives in epistula-api/serve.go (api) and
// epistula-database/deliver.go (recoverDeliver); imapsess was the only
// long-lived server in the stack without one.
func (s *Session) guard(op string, errp *error) {
	p := recover()
	if p == nil {
		return
	}

	// A panic barrier must itself be total: fall back to the default
	// logger rather than nil-dereferencing while handling a panic.
	logger := slog.Default()
	if s != nil && s.be != nil && s.be.Logger != nil {
		logger = s.be.Logger
	}
	mailbox := ""
	if s != nil {
		mailbox = s.mailboxName
	}
	logger.Error("session panic",
		"op", op,
		"panic", p,
		"mailbox", mailbox,
		"stack", string(debug.Stack()),
	)

	*errp = &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeServerBug,
		Text: op + ": internal server error",
	}
}
