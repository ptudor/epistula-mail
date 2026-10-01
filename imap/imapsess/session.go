// Package imapsess implements the imapserver.Session interface backed by the
// epistula-database Postgres schema and blob store. This file holds the auth,
// folder, namespace, and FETCH read paths; SEARCH, mutations, APPEND, and
// IDLE live in sibling files.
package imapsess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/mail"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/epistula-mail/database/auth"
	"github.com/ptudor/epistula-mail/database/blob"
	"github.com/ptudor/epistula-mail/database/ingest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// Backend is the per-server shared state.
type Backend struct {
	Pool           *pgxpool.Pool
	BlobStore      *blob.Store
	Logger         *slog.Logger
	StmtTimeout    time.Duration
	MaxAppendBytes int64 // 0 = 50 MiB default

	// MaxSearchResults caps a single SEARCH's result set. 0 = unlimited.
	// SEARCH accumulates every match into slices and then serializes the
	// whole NumSet, so an unbounded SEARCH ALL on a large folder is an
	// unbounded allocation reachable by any authenticated client
	// (RO5X-020).
	MaxSearchResults int

	// Connection / login rate limiting. Nil disables the corresponding
	// check (treat as "always allow").
	PerIP        *PerIPLimiter
	PerMailbox   *MailboxSessionLimiter
	LoginLimiter *LoginThrottle

	// CommandRatePerSec caps commands per second per session (token
	// bucket with a one-second burst). <= 0 disables the cap.
	CommandRatePerSec int

	// IdlePool, when set, is a dedicated pool for IDLE's LISTEN
	// connections so simultaneous IDLE sessions can't starve Pool's
	// query connections. Nil falls back to Pool.
	IdlePool *pgxpool.Pool

	// DeleteArchives makes EXPUNGE archive the last copy of a message instead
	// of destroying it, everywhere but \Trash, \Drafts and \Junk
	// (delete_archives.go; [archive] delete_archives).
	DeleteArchives bool

	// IdleHeartbeat overrides the IDLE re-poll interval (the
	// idle_timeout config key). <= 0 uses the 29-minute default that
	// stays inside RFC 9051's 30-minute floor.
	IdleHeartbeat time.Duration

	// AuthBudget rations concurrent Argon2id verification across the whole
	// daemon by the scratch memory each hash declares (RA6X-023). Nil
	// verifies without rationing, which is only appropriate for tests and
	// embedders that impose their own bound.
	AuthBudget *AuthBudget
	authVerify func(password, encoded string) (bool, error) // deterministic tests; production uses auth.VerifyPassword

	// AuthTiming preserves a common minimum for lookup noise and early
	// failures. The common cost workset handles mixed hashes independently.
	// Nil disables only this optional padding.
	AuthTiming *AuthTimingFloor

	// PreAuthTimeout bounds how long a connection may sit between TLS
	// handshake and successful LOGIN (the preauth_timeout config key).
	// Enforced by a one-shot timer armed in NewSessionForNetConn that
	// closes the conn unless Login has flipped the authenticated flag —
	// a read deadline would be silently reset by the library's
	// per-command deadlines (R-039). <= 0 disables the timer.
	PreAuthTimeout time.Duration

	storageOnce sync.Once
	storageDB   *storage.DB // lazily wraps Pool; do NOT use directly

	// active counts open sessions so serve-side shutdown can drain:
	// stop accepting, wait for this to reach zero (with timeout), then
	// close the PG pool. Incremented at session creation, decremented
	// exactly once in Session.Close.
	active atomic.Int64
}

// ActiveSessions returns the number of sessions currently open.
func (b *Backend) ActiveSessions() int64 { return b.active.Load() }

// Storage returns a *storage.DB that wraps Backend.Pool so APPEND can
// invoke the shared Ingest path. The DB does not own the pool — Backend
// keeps lifecycle responsibility. Safe for concurrent use.
func (b *Backend) Storage() *storage.DB {
	b.storageOnce.Do(func() {
		b.storageDB = storage.NewFromPool(b.Pool, storage.Config{StatementTimeout: b.StmtTimeout})
	})
	return b.storageDB
}

// Session is one client connection's state. Created by Backend.NewSession.
type Session struct {
	be *Backend

	// remoteIP is set by NewSessionForConn. Empty when the limiter is
	// disabled or the address can't be parsed; in that case rate-limit
	// checks no-op.
	remoteIP string

	// Set on successful Login. mailboxID == 0 means unauthenticated.
	mailboxID   int64
	mailboxName string
	// tenant is the validated blob-store subtree for this mailbox — the
	// canonical mailboxes.name, parsed once at login. Every blob read (FETCH
	// BODY[]) and APPEND write uses it so the on-disk path matches what
	// delivery wrote. Zero value until authenticated.
	tenant blob.Tenant

	// Set by Select. selectedFolderID == 0 means no mailbox is selected.
	selectedFolderID    int64
	selectedFolderName  string
	selectedReadOnly    bool
	selectedUIDValidity int64
	viewFolderModSeq    int64
	viewStampKnown      bool

	// view is what this client has been told the selected folder contains: an
	// ordered UID list whose index+1 is the client's sequence number
	// (RA6X-001). Every sequence number the client sends is translated through
	// it, and it only changes alongside the untagged responses that justify
	// the change. Nil when no folder is selected.
	view *sessionView

	// True once we've Acquired a mailbox-session slot so Close knows
	// to Release exactly once.
	mailboxSlotHeld bool

	// closed makes Close idempotent so the backend's active-session
	// counter is decremented exactly once per session.
	closed bool

	// sessCtx is cancelled when the session closes. Work that queues on a
	// shared resource on this session's behalf — waiting for authentication
	// budget, acquiring an IDLE connection — derives from it so a client that
	// disconnects gives up its place immediately instead of holding a slot
	// for a result nobody will read.
	sessCtx    context.Context
	sessCancel context.CancelFunc

	// cmdLimiter enforces Backend.CommandRatePerSec. Nil = unlimited.
	cmdLimiter *tokenBucket

	// conn is the client's net.Conn when the session was built via
	// NewSessionForNetConn; nil in tests that construct sessions directly.
	// Used only to lift the pre-auth read deadline on successful Login.
	conn net.Conn

	// preAuthTimer enforces the total "authenticate within PreAuthTimeout"
	// budget independently of the library's per-command read deadline (R-039).
	// authenticated is flipped true by a successful Login and read by the
	// timer goroutine — the two touch it atomically; nothing else does.
	preAuthTimer  *time.Timer
	authenticated atomic.Bool
	// timedOut distinguishes "the pre-auth timer closed this connection" from
	// "it authenticated", since both set authenticated (see the CAS in
	// NewSessionForNetConn).
	timedOut atomic.Bool

	// expungeRaceHook, when non-nil, is called inside Expunge between the
	// candidate SELECT and the DELETE. It exists solely so the RO5X-001
	// regression test can commit a concurrent `STORE -FLAGS (\Deleted)`
	// in that exact window and prove the DELETE's repeated flag predicate
	// skips the un-deleted row. Always nil in production; the branch is a
	// single nil check on a path that already does two round trips.
	expungeRaceHook func()

	// copyRaceHook, when non-nil, is called inside copyInTx after the
	// destination message rows are inserted and before their attachments are
	// duplicated — the exact window behind RA6X-002, where a concurrent
	// EXPUNGE of the source used to produce a destination message with no
	// attachments and no error. Test-only.
	copyRaceHook func()

	// Test barrier after reading SELECT counters and before reading its UID view.
	selectSnapshotHook func()
}

// NewSession returns a fresh per-connection session with no remote
// address context. Use NewSessionForConn from the listener wrapper to
// thread the client IP through.
func (b *Backend) NewSession() *Session {
	return b.newSession("")
}

// newSession is the single construction point, so every session gets its
// lifetime context.
func (b *Backend) newSession(remoteIP string) *Session {
	b.active.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		be:         b,
		remoteIP:   remoteIP,
		cmdLimiter: newTokenBucket(b.CommandRatePerSec),
		sessCtx:    ctx,
		sessCancel: cancel,
	}
}

// NewSessionForConn returns a session annotated with the client's
// remote IP. The IP is used by Login throttling and Close-time
// per-IP connection release.
func (b *Backend) NewSessionForConn(remote net.Addr) *Session {
	return b.newSession(remoteIP(remote))
}

// NewSessionForNetConn is NewSessionForConn plus a hard "authenticate within
// PreAuthTimeout" budget: a client that completes the TLS handshake and then
// never authenticates is cut off instead of holding a connection slot forever.
//
// The budget is enforced with a one-shot timer, NOT a read deadline: the
// library resets its per-command read deadline on every command (including
// before the first read), so a SetReadDeadline here would be silently defeated
// by a client sending a cheap command (NOOP) every few seconds without ever
// logging in (R-039). The timer fires once regardless of command traffic and
// closes the connection unless Login has flipped `authenticated`.
func (b *Backend) NewSessionForNetConn(conn net.Conn) *Session {
	s := b.NewSessionForConn(conn.RemoteAddr())
	s.conn = conn
	if b.PreAuthTimeout > 0 {
		s.preAuthTimer = time.AfterFunc(b.PreAuthTimeout, func() {
			// CompareAndSwap, not Load-then-close: with a plain read, a timer
			// that fired and observed false could still close a connection
			// that Login authenticated a microsecond later — Stop() returns
			// false once the callback is already running, so it does not
			// prevent this. The CAS makes "timer wins" and "login wins"
			// mutually exclusive: whichever flips the flag first, the other
			// takes no action (RO5X-038).
			//
			// Setting authenticated=true here is safe precisely because the
			// connection is being closed: nothing later reads it as "this
			// session is logged in" (Login gates on requireAuth via
			// mailboxID, which stays 0). timedOut records the real reason for
			// the log line.
			if s.authenticated.CompareAndSwap(false, true) {
				s.timedOut.Store(true)
				s.sessCancel()
				_ = conn.Close()
			}
		})
	}
	return s
}

// ----------- not-authenticated state -----------

// Close releases per-connection state. Called by the server on disconnect.
// Also releases any rate-limit slots this connection held (per-mailbox
// session count; per-IP connection count is owned by the listener
// wrapper, which releases when the underlying net.Conn closes).
func (s *Session) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	// Release anything queued on this session's behalf — notably a wait for
	// authentication budget or an IDLE connection — before touching the rest.
	if s.sessCancel != nil {
		s.sessCancel()
	}
	// Stop the pre-auth budget timer if it hasn't fired — a normal disconnect
	// before authenticating must not leave a timer holding the conn reference.
	if s.preAuthTimer != nil {
		s.preAuthTimer.Stop()
	}
	if s.mailboxSlotHeld {
		s.be.PerMailbox.Release(s.mailboxID)
		s.mailboxSlotHeld = false
	}
	s.mailboxID = 0
	s.selectedFolderID = 0
	s.be.active.Add(-1)
	return nil
}

// dummyPasswordHash calibrates the optional minimum duration and supplies the
// fallback cost class when the database has no supported account hashes.
// Admitted logins normally use the complete catalog in authprofiles.go.
var dummyPasswordHash = mustDummyHash()

func mustDummyHash() string {
	h, err := auth.HashPassword("dummy-verify-timing-equalizer", auth.DefaultParams())
	if err != nil {
		// HashPassword only fails if crypto/rand fails, which is unrecoverable.
		panic("imapsess: cannot compute dummy password hash: " + err.Error())
	}
	return h
}

// Login verifies username + password against mailboxes.password_hash using
// Argon2id. A disabled mailbox is rejected even with a correct password.
// Rate-limit checks: refuses early when the source IP is throttled
// (without doing the Argon2id verify, which is the resource an attacker
// would want to burn); refuses when the mailbox already has
// per_mailbox_connections active sessions.
func (s *Session) Login(username, password string) (err error) {
	defer s.guard("LOGIN", &err)
	// The common cost workset equalizes admitted account categories. Keep the
	// existing minimum for lookup noise and early throttled/invalid requests.
	loginStart := time.Now()
	defer func() { s.be.AuthTiming.Wait(loginStart) }()
	// A session that is already authenticated must not Login again — a
	// second success would orphan the held per-mailbox slot.
	if s.mailboxID != 0 {
		return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "already authenticated"}
	}
	// Pre-auth commands don't pass through requireAuth, so the command
	// rate cap is applied here directly.
	if !s.cmdLimiter.allow() {
		metricLimiterRejections.WithLabelValues("command_rate").Inc()
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeLimit,
			Text: "command rate exceeded; slow down",
		}
	}
	if username == "" || password == "" {
		reason := "password missing"
		if username == "" {
			reason = "username missing"
		}
		s.be.Logger.Info("login failed", "reason", reason, "username", loggableUsername(username), "ip", s.remoteIP)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAuthenticationFailed, Text: "credentials required"}
	}
	// Throttle check FIRST — short-circuits before the password hash
	// comparison so a locked-out attacker can't keep us busy.
	if s.remoteIP != "" && s.be.LoginLimiter != nil && !s.be.LoginLimiter.Allow(s.remoteIP) {
		s.be.Logger.Warn("login throttled", "ip", s.remoteIP, "username", username)
		metricLimiterRejections.WithLabelValues("login_throttle").Inc()
		metricAuthTotal.WithLabelValues("failure").Inc()
		return authFailed()
	}

	ctx, cancel := s.queryCtx()
	defer cancel()

	identity, profiles, err := loadLoginIdentity(ctx, s.be.Pool, username)
	if err != nil {
		s.be.Logger.Error("Login: authentication catalog", "err", err)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeUnavailable, Text: "backend unavailable"}
	}
	id, canonicalName := identity.id, identity.name
	ok, err := s.verifyProfiles(password, identity, profiles)
	if err != nil {
		if errors.Is(err, errAuthBudgetExhausted) {
			// Server-side saturation, not a credential problem. Do not charge
			// it to this client's login-failure budget: that would let an
			// attacker's flood lock out the legitimate users queued behind it.
			// The response depends on daemon load, never on the account, so it
			// is not an existence oracle. epistula-api answers the same condition
			// with 429.
			s.be.Logger.Warn("Login: authentication verification budget exhausted",
				"ip", s.remoteIP, "budget_kib", s.be.AuthBudget.LimitKiB())
			metricLimiterRejections.WithLabelValues("auth_budget").Inc()
			metricAuthTotal.WithLabelValues("failure").Inc()
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeUnavailable,
				Text: "authentication is busy; retry shortly",
			}
		}
		s.be.Logger.Warn("Login: malformed password_hash on mailbox", "username", username, "err", err)
		s.recordLoginFailure()
		return authFailed()
	}
	if !ok {
		s.logLoginFailure(username, identity)
		s.recordLoginFailure()
		return authFailed()
	}

	// The mailbox name is also the on-disk blob tenant. Take the canonical
	// value from the row (never the client-supplied username) and validate it
	// as a path-safe component; a malformed legacy name fails closed rather
	// than risking a bad path. Done before the slot acquire so there is no slot
	// to release on the (should-never-happen) failure path.
	tenant, terr := blob.ParseTenant(canonicalName)
	if terr != nil {
		s.be.Logger.Error("Login: mailbox name is not a valid blob tenant",
			"mailbox", canonicalName, "err", terr)
		return authFailed()
	}

	// The credential is correct, so clear the IP's failure history NOW —
	// before the resource cap below can refuse the session.
	//
	// This used to run only after Acquire succeeded, so a user with the right
	// password whose mailbox was at its session cap got NO [LIMIT] *and* kept
	// their accrued failures: mistype four times, fix it, hit the cap, and you
	// stay one failure from a 60s lockout despite having proved the
	// credential. A correct password is a successful authentication regardless
	// of whether a resource cap then refuses the session — that is what the
	// throttle measures (RO5X-037).
	if s.be.LoginLimiter != nil && s.remoteIP != "" {
		s.be.LoginLimiter.RecordResult(s.remoteIP, true)
	}

	// Per-mailbox session cap. Apply AFTER successful credential check
	// so wrong-password attempts don't have to pay the cap; that way an
	// attacker hammering one mailbox can't lock out the legitimate user
	// just by exhausting the slot count. (This ordering must NOT change.)
	if s.be.PerMailbox != nil && !s.be.PerMailbox.Acquire(id) {
		s.be.Logger.Warn("login refused: per-mailbox session cap",
			"mailbox", username, "ip", s.remoteIP)
		metricLimiterRejections.WithLabelValues("per_mailbox").Inc()
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeLimit,
			Text: "too many concurrent sessions for this mailbox",
		}
	}
	s.mailboxSlotHeld = true

	s.mailboxID = id
	s.mailboxName = canonicalName
	s.tenant = tenant
	// Authenticated: stop the pre-auth budget timer so it can't close a live
	// session, and lift any read deadline. Idle-session policy from here on is
	// the client's IDLE/poll cadence plus the per-IP and per-mailbox caps.
	s.authenticated.Store(true)
	if s.preAuthTimer != nil {
		s.preAuthTimer.Stop()
	}
	if s.conn != nil {
		_ = s.conn.SetReadDeadline(time.Time{})
	}
	metricAuthTotal.WithLabelValues("success").Inc()
	s.be.Logger.Info("login ok", "mailbox", username, "ip", s.remoteIP)
	return nil
}

// recordLoginFailure increments the per-IP failure counter when the
// limiter is configured and an IP is known.
// maxLoggedUsername bounds a client-supplied username in the log. Mailbox
// names are at most 64 bytes, so anything longer is not one.
const maxLoggedUsername = 64

// loggableUsername is a client-supplied username as the log may show it:
// truncated to maxLoggedUsername bytes. slog quotes it, so control characters
// cannot forge a line.
func loggableUsername(username string) string {
	if len(username) <= maxLoggedUsername {
		return username
	}
	return username[:maxLoggedUsername] + "..."
}

// logLoginFailure records why a LOGIN with a username and password failed,
// never with the password. A failure against an existing mailbox is a WARN
// naming it: someone holds a wrong password for a real account, or the
// account is disabled. A username that names no mailbox is INFO: it is
// usually a misconfigured client or a scanner, and the client never learns
// which case it hit, since both answer AUTHENTICATIONFAILED.
func (s *Session) logLoginFailure(username string, identity loginIdentity) {
	switch {
	case identity.id == 0:
		s.be.Logger.Info("login failed", "reason", "no such mailbox",
			"username", loggableUsername(username), "ip", s.remoteIP)
	case identity.disabled != nil:
		s.be.Logger.Warn("login failed", "reason", "mailbox disabled",
			"mailbox", identity.name, "ip", s.remoteIP)
	default:
		s.be.Logger.Warn("login failed", "reason", "wrong password",
			"mailbox", identity.name, "ip", s.remoteIP)
	}
}

func (s *Session) recordLoginFailure() {
	metricAuthTotal.WithLabelValues("failure").Inc()
	if s.be.LoginLimiter != nil && s.remoteIP != "" {
		s.be.LoginLimiter.RecordResult(s.remoteIP, false)
	}
}

// authFailed returns the IMAP-conformant "wrong username or password" error
// without leaking which half failed.
func authFailed() error {
	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeAuthenticationFailed,
		Text: "invalid credentials",
	}
}

// ----------- authenticated state -----------

func (s *Session) requireAuth() error {
	if s.mailboxID == 0 {
		return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "not authenticated"}
	}
	// Every authenticated command flows through here — the natural choke
	// point for the per-session command-rate cap.
	if !s.cmdLimiter.allow() {
		s.be.Logger.Warn("command rate exceeded",
			"mailbox", s.mailboxName, "ip", s.remoteIP)
		metricLimiterRejections.WithLabelValues("command_rate").Inc()
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeLimit,
			Text: "command rate exceeded; slow down",
		}
	}
	return nil
}

// Subscribe records the (mailbox, folder) row in folder_subscriptions.
func (s *Session) Subscribe(mailbox string) (err error) {
	defer s.guard("SUBSCRIBE", &err)
	if err := s.requireAuth(); err != nil {
		return err
	}
	ctx, cancel := s.queryCtx()
	defer cancel()

	folderID, err := s.lookupFolder(ctx, mailbox)
	if err != nil {
		return err
	}
	if _, err := s.be.Pool.Exec(ctx,
		`INSERT INTO folder_subscriptions (mailbox_id, folder_id)
		 VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		s.mailboxID, folderID,
	); err != nil {
		s.be.Logger.Error("Subscribe", "err", err)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "subscribe failed"}
	}
	return nil
}

// Unsubscribe removes the subscription row.
func (s *Session) Unsubscribe(mailbox string) (err error) {
	defer s.guard("UNSUBSCRIBE", &err)
	if err := s.requireAuth(); err != nil {
		return err
	}
	ctx, cancel := s.queryCtx()
	defer cancel()

	folderID, err := s.lookupFolder(ctx, mailbox)
	if err != nil {
		return err
	}
	if _, err := s.be.Pool.Exec(ctx,
		`DELETE FROM folder_subscriptions WHERE mailbox_id = $1 AND folder_id = $2`,
		s.mailboxID, folderID,
	); err != nil {
		s.be.Logger.Error("Unsubscribe", "err", err)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "unsubscribe failed"}
	}
	return nil
}

// Namespace exposes a single personal namespace rooted at the empty prefix,
// with "/" as the hierarchy delimiter. Advertising IMAP4rev2/NAMESPACE in
// serve.go requires this method; the upstream server checks the interface at
// connection start.
func (s *Session) Namespace() (*imap.NamespaceData, error) {
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	return &imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{{Prefix: "", Delim: '/'}},
	}, nil
}

// List returns the folders for the authenticated mailbox matching the
// RFC LIST patterns supplied by the client. The emersion server
// translates LSUB (RFC 3501 §6.3.9) into a LIST call with
// options.SelectSubscribed=true, so honoring that flag here gives us
// LSUB for free.
func (s *Session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) (err error) {
	defer s.guard("LIST", &err)
	if err := s.requireAuth(); err != nil {
		return err
	}
	if ref == "" && (len(patterns) == 0 || (len(patterns) == 1 && patterns[0] == "")) {
		return w.WriteList(&imap.ListData{
			Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect},
			Delim: '/',
		})
	}

	ctx, cancel := s.queryCtx()
	defer cancel()

	// When SelectSubscribed is set (LSUB and "LIST (SUBSCRIBED) ..."):
	//   - WHERE clause restricts to folders this mailbox is subscribed to
	//   - the result always carries the \Subscribed attribute, regardless
	//     of whether ReturnSubscribed was also set
	//
	// When ReturnSubscribed is set without SelectSubscribed (RFC 5258
	// "LIST () ... RETURN (SUBSCRIBED)"), we still emit all folders but
	// annotate each with \Subscribed if a subscription row exists.
	query := `
		SELECT f.name, f.special_use,
		       EXISTS(SELECT 1 FROM folder_subscriptions s
		               WHERE s.mailbox_id = f.mailbox_id AND s.folder_id = f.id) AS subscribed
		  FROM folders f
		 WHERE f.mailbox_id = $1`
	if options != nil && options.SelectSubscribed {
		query += ` AND EXISTS (SELECT 1 FROM folder_subscriptions s
		                        WHERE s.mailbox_id = f.mailbox_id AND s.folder_id = f.id)`
	}
	query += ` ORDER BY f.name`

	rows, err := s.be.Pool.Query(ctx, query, s.mailboxID)
	if err != nil {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "list query failed"}
	}
	defer rows.Close()

	for rows.Next() {
		var (
			name       string
			specialUse *string
			subscribed bool
		)
		if err := rows.Scan(&name, &specialUse, &subscribed); err != nil {
			return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "list scan failed"}
		}
		if !patternMatches(name, patterns, ref) {
			continue
		}
		data := &imap.ListData{
			Mailbox: name,
			Delim:   '/',
		}
		if specialUse != nil {
			data.Attrs = append(data.Attrs, imap.MailboxAttr(*specialUse))
		}
		// Emit \Subscribed when the client asked for it (RETURN
		// SUBSCRIBED), or when the LSUB-equivalent SelectSubscribed
		// is set (an LSUB response always implies the folder is
		// subscribed).
		if subscribed && (options == nil || options.ReturnSubscribed || options.SelectSubscribed) {
			data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return rows.Err()
}

func patternMatches(name string, patterns []string, ref string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if imapserver.MatchList(name, '/', ref, p) {
			return true
		}
	}
	return false
}

// Status returns counts for the named folder. It honors the requested
// StatusOptions: MESSAGES, UIDNEXT, UIDVALIDITY, HIGHESTMODSEQ come from the
// folders row; UNSEEN, DELETED, SIZE are aggregated from the folder's
// messages. Every pointer field the client asked for is set non-nil — the
// go-imap writer dereferences them unconditionally when the item was
// requested, so a nil here panics mid-response and tears down the connection.
// Real MUAs poll non-selected folders with `STATUS ... UNSEEN` to render
// unread badges, so this path is exercised by essentially every client.
func (s *Session) Status(mailbox string, options *imap.StatusOptions) (statusData *imap.StatusData, err error) {
	defer s.guard("STATUS", &err)
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	if options == nil {
		options = &imap.StatusOptions{}
	}
	ctx, cancel := s.queryCtx()
	defer cancel()

	folderID, err := s.lookupFolder(ctx, mailbox)
	if err != nil {
		return nil, err
	}

	var uidValidity, uidNext, highestModSeq int64
	if err := s.be.Pool.QueryRow(ctx,
		`SELECT uidvalidity, uidnext, highest_modseq FROM folders WHERE id = $1`,
		folderID,
	).Scan(&uidValidity, &uidNext, &highestModSeq); err != nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "status: folder lookup"}
	}
	if err := protocolIDs(uidValidity, uidNext); err != nil {
		return nil, err
	}

	// One scan of the folder yields every count STATUS can ask for. The FILTER
	// aggregates are cheap relative to the scan itself, so we compute them all
	// regardless of which items were requested and derive pointers below.
	var numMessages, numUnseen, numDeleted, totalSize int64
	if err := s.be.Pool.QueryRow(ctx,
		`SELECT count(*),
		        count(*) FILTER (WHERE NOT ('\Seen' = ANY(flags))),
		        count(*) FILTER (WHERE '\Deleted' = ANY(flags)),
		        COALESCE(SUM(raw_size), 0)
		   FROM messages WHERE folder_id = $1`,
		folderID,
	).Scan(&numMessages, &numUnseen, &numDeleted, &totalSize); err != nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "status: count"}
	}

	data := &imap.StatusData{
		Mailbox:       mailbox,
		UIDNext:       imap.UID(uidNext),
		UIDValidity:   uint32(uidValidity),
		HighestModSeq: uint64(highestModSeq),
	}
	// NumMessages is always populated; the writer emits it only when requested.
	nMsg := uint32(numMessages)
	data.NumMessages = &nMsg
	if options.NumUnseen {
		n := uint32(numUnseen)
		data.NumUnseen = &n
	}
	if options.NumDeleted {
		n := uint32(numDeleted)
		data.NumDeleted = &n
	}
	if options.Size {
		sz := totalSize
		data.Size = &sz
	}
	if options.NumRecent {
		// RECENT is not tracked; report 0 (a non-nil pointer) so the writer can
		// emit it without dereferencing nil.
		var zero uint32
		data.NumRecent = &zero
	}
	if options.DeletedStorage {
		// QUOTA=RES-STORAGE is not advertised, but a client may request it; a
		// non-nil zero keeps the writer from panicking.
		var zero int64
		data.DeletedStorage = &zero
	}
	return data, nil
}

// Select activates a folder for FETCH / SEARCH / STORE / EXPUNGE.
func (s *Session) Select(mailbox string, options *imap.SelectOptions) (selectData *imap.SelectData, err error) {
	defer s.guard("SELECT", &err)
	if err := s.requireAuth(); err != nil {
		return nil, err
	}
	ctx, cancel := s.queryCtx()
	defer cancel()

	folderID, data, snapshot, err := s.readSelection(ctx, mailbox)
	if err != nil {
		return nil, err
	}
	s.selectedFolderID = folderID
	s.selectedFolderName = canonicalFolderName(mailbox)
	s.selectedReadOnly = options != nil && options.ReadOnly
	s.selectedUIDValidity = int64(data.UIDValidity)
	s.viewFolderModSeq = int64(data.HighestModSeq)
	s.viewStampKnown = true
	s.view = newSessionView(snapshot)
	data.PermanentFlags = permanentFlagsFor(s.selectedReadOnly)
	return data, nil
}

// Unselect deselects without expunging. RFC 3691.
func (s *Session) Unselect() (err error) {
	defer s.guard("UNSELECT", &err)
	s.selectedFolderID = 0
	s.selectedFolderName = ""
	s.selectedReadOnly = false
	s.view = nil
	return nil
}

// Poll is called by the server between commands. It reconciles this session's
// UID view with the database and emits the untagged EXPUNGE / EXISTS / FETCH
// FLAGS responses that carry the client from one to the other (RA6X-001).
//
// It used to be `return nil`, which is why a client's sequence numbers could
// silently stop meaning what the client thought: another session's expunge
// renumbered the folder and nothing ever told this one.
//
// allowExpunge is the protocol's rule that EXPUNGE must not arrive while a
// command referencing sequence numbers is in flight. When it is false the
// whole delta is deferred rather than partially applied — the next poll that
// may speak delivers it.
func (s *Session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) (err error) {
	defer s.guard("POLL", &err)
	if s.selectedFolderID == 0 {
		return nil
	}
	ctx, cancel := s.queryCtx()
	defer cancel()
	return s.reconcile(ctx, w, allowExpunge)
}

// Idle subscribes to PG NOTIFY on the mail_arrived channel and emits
// EXISTS updates when a new message lands in the selected folder.
// The implementation lives in idle.go.
func (s *Session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) (err error) {
	defer s.guard("IDLE", &err)
	// Derive from the SESSION's context, not context.Background() (RA6X-022).
	// Everything IDLE does — acquiring a pooled connection, issuing LISTEN,
	// reconciling — must stop when this connection closes or the daemon shuts
	// down, and a background context can express neither.
	base := s.sessCtx
	if base == nil {
		base = context.Background()
	}
	return s.idleConsume(base, w, stop)
}

// ----------- selected state -----------

// Search is implemented in search.go.

// Fetch implements UID, Flags, InternalDate, RFC822Size, Envelope,
// BodyStructure, and BODY[...] section reads. BINARY[...] is deliberately
// rejected until decoded binary-part fetches are implemented.
func (s *Session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) (err error) {
	defer s.guard("FETCH", &err)
	if err := s.requireAuth(); err != nil {
		return err
	}
	if s.selectedFolderID == 0 {
		return responseBadState("FETCH: no mailbox selected")
	}
	if options == nil {
		options = &imap.FetchOptions{}
	}
	if len(options.BinarySection) > 0 || len(options.BinarySectionSize) > 0 {
		return responseCannot("FETCH BINARY: not implemented")
	}

	ctx, cancel := s.queryCtx()
	defer cancel()

	uids, err := s.targetUIDs(ctx, numSet)
	if err != nil {
		return err
	}
	// Apply the complete implicit Seen mutation in one transaction before any
	// response. Only UID scalars are retained; large metadata is read in batches.
	seenChanged := map[int64]bool{}
	if !s.selectedReadOnly && fetchImpliesSeen(options) && len(uids) > 0 {
		seenChanged, err = s.applySeenSideEffect(ctx, uids)
		if err != nil {
			return err
		}
	}
	// Nil writer: tests exercise the side-effect path (the \Seen update
	// above) without a real FetchWriter, mirroring Expunge's affordance.
	if w == nil {
		return nil
	}

	return s.visitFetchRows(ctx, uids, options, func(r fetchRow) error {
		sections := &messageSections{}
		var envelope *imap.Envelope
		if options.Envelope {
			header, err := sections.topHeader(s, r)
			if err != nil {
				return err
			}
			envelope, err = buildRawEnvelope(r, header)
			if err != nil {
				return err
			}
		}
		resp := w.CreateMessage(r.seqNum)
		// UID is implicit in IMAP4rev2 and many clients depend on it being
		// emitted even when not explicitly requested when the command was
		// UID FETCH. Honoring the FetchOptions flag is the common pattern;
		// callers that want it always-on set it.
		if options.UID {
			resp.WriteUID(imap.UID(r.uid))
		}
		if options.Flags || seenChanged[r.uid] {
			resp.WriteFlags(toIMAPFlags(r.flags))
		}
		if options.InternalDate {
			resp.WriteInternalDate(r.internalDate)
		}
		if options.RFC822Size {
			resp.WriteRFC822Size(r.rawSize)
		}
		if options.Envelope {
			resp.WriteEnvelope(envelope)
		}
		if options.BodyStructure != nil {
			bs, err := decodeBodyStructure(r.bodyStructure)
			if err != nil {
				s.be.Logger.Warn("FETCH bodystructure decode", "uid", r.uid, "err", err)
				// Emit a benign single-part placeholder so the client gets
				// something parseable rather than a broken response.
				bs = &imap.BodyStructureSinglePart{
					Type: "application", Subtype: "octet-stream",
					Size: uint32(r.rawSize),
				}
			}
			resp.WriteBodyStructure(bs)
		}
		// One cache per message: every section of this FETCH shares the blob
		// read and the MIME walk, and it is dropped with the message so the
		// retained bytes never exceed one message (RA6X-054).
		for _, sec := range options.BodySection {
			if err := s.writeBodySection(resp, r, sec, sections); err != nil {
				return err
			}
		}
		if err := resp.Close(); err != nil {
			return err
		}
		if options.Flags || seenChanged[r.uid] {
			s.view.note(r.uid, r.modSeq)
		}
		return nil
	})
}

// fetchImpliesSeen reports whether the FETCH carries at least one body
// section without .PEEK — the RFC 9051 trigger for the implicit \Seen.
func fetchImpliesSeen(options *imap.FetchOptions) bool {
	for _, sec := range options.BodySection {
		if !sec.Peek {
			return true
		}
	}
	return false
}

// hasFlagFold reports whether flags contains want, case-insensitively
// (IMAP system flags are case-insensitive per RFC 9051 §2.3.2).
func hasFlagFold(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

// applySeenSideEffect adds \Seen to the given UIDs in the selected folder,
// stamping a fresh modseq exactly like STORE. Returns the set of UIDs whose
// flag set actually changed (a concurrent STORE may have won the race).
func (s *Session) applySeenSideEffect(ctx context.Context, uids []int64) (map[int64]bool, error) {
	// Folder (level 3) then messages (level 4) — the canonical order, the same
	// one STORE takes. The FETCH responses this feeds are written after the
	// transaction, so a deadlock victim can replay it (RA6X-021).
	changed := make(map[int64]bool, len(uids))
	if err := retryTx(ctx, s.be.Pool.Begin, func(tx pgx.Tx) error {
		clear(changed)

		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE folder_id=$1 AND uid=ANY($2) AND NOT ('\Seen'=ANY(flags)))`, s.selectedFolderID, uids).Scan(&pending); err != nil {
			return err
		}
		if !pending {
			return nil
		}
		var newModSeq int64
		if err := tx.QueryRow(ctx,
			`UPDATE folders SET highest_modseq = highest_modseq + 1
			  WHERE id = $1 RETURNING highest_modseq`,
			s.selectedFolderID,
		).Scan(&newModSeq); err != nil {
			return fmt.Errorf("modseq bump: %w", err)
		}

		rows, err := tx.Query(ctx,
			`UPDATE messages SET flags = flags || $3, mod_seq = $4
			  WHERE folder_id = $1 AND uid = ANY($2) AND NOT ($5 = ANY(flags))
			 RETURNING uid`,
			s.selectedFolderID, uids, []string{`\Seen`}, newModSeq, `\Seen`,
		)
		if err != nil {
			return fmt.Errorf("update: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var uid int64
			if err := rows.Scan(&uid); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			changed[uid] = true
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		notifyFolderChanged(ctx, tx, s.be.Logger, s.selectedFolderID)
		return nil
	}); err != nil {
		s.be.Logger.Error("FETCH seen update", "err", err)
		if isRetryableTxError(err) {
			return nil, txAborted("FETCH")
		}
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "FETCH seen update failed"}
	}
	// FETCH began from an earlier metadata snapshot. Do not mark a later
	// database version as reported: Poll must still deliver a peer's changes.

	return changed, nil
}

// writeBodySection streams one BODY[...] response. It handles:
//
//   - BODY[]                            full raw RFC 5322 (streamed from blob)
//   - BODY[HEADER]                      header section incl. blank-line sep
//   - BODY[TEXT]                        everything after the header section
//   - BODY[HEADER.FIELDS (F ...)]       only the named headers
//   - BODY[HEADER.FIELDS.NOT (F ...)]   every header except the named ones
//   - BODY[N]                           body of part N
//   - BODY[N.MIME]                      MIME header of part N
//   - BODY[N.HEADER] / BODY[N.TEXT]     header / body of the message part N
//     encloses (see selectEntityPayload)
//   - BODY[N.HEADER.FIELDS (F ...)]     filtered header of that message
//   - <offset.size> Partial             honored for all of the above
func (s *Session) writeBodySection(resp *imapserver.FetchResponseWriter, r fetchRow, sec *imap.FetchItemBodySection, cache *messageSections) error {
	// BODY[]: stream the raw blob without ever loading the full message.
	if len(sec.Part) == 0 &&
		sec.Specifier == imap.PartSpecifierNone &&
		len(sec.HeaderFields) == 0 && len(sec.HeaderFieldsNot) == 0 {
		return s.streamRawBody(resp, r, sec)
	}

	// Top-level BODY[HEADER] / BODY[HEADER.FIELDS] / BODY[HEADER.FIELDS.NOT]
	// need only the header section — read up to the separator instead of the
	// whole (up to 50 MiB) blob (R-042). Byte-identical to the full-blob path
	// because the header bytes are the same slice splitHeaderBody would extract.
	if len(sec.Part) == 0 && sec.Specifier == imap.PartSpecifierHeader {
		headerBytes, err := cache.topHeader(s, r)
		if err != nil {
			s.be.Logger.Error("FETCH header-section load blob",
				"mailbox", s.mailboxName, "folder", s.selectedFolderName,
				"uid", r.uid, "sha16", safeShortSHA(r.rawSHA256Hex), "err", err)
			return missingBlobErr()
		}
		payload, ok := selectEntityPayload(sec, rawEntity{header: headerBytes})
		if !ok {
			w := resp.WriteBodySection(sec, 0)
			return w.Close()
		}
		payload = applyPartial(payload, sec.Partial)
		w := resp.WriteBodySection(sec, int64(len(payload)))
		if _, err := w.Write(payload); err != nil {
			_ = w.Close()
			return fmt.Errorf("BODY[HEADER] write: %w", err)
		}
		return w.Close()
	}

	// Everything else needs the bytes resident — load the whole blob
	// into memory once. Bounded by message size limits enforced at
	// ingest time (default 50 MiB cap).
	// Resolve the addressed part (empty Part means the top-level message).
	// Everything returned is a sub-slice of the stored message, so the
	// response is what the sender sent (RA6X-005, RA6X-027).
	//
	// Both the blob read and the part walk go through the per-message cache
	// (RA6X-054): a client asking for BODY[1] BODY[2] BODY[3] in ONE command
	// used to re-read the whole message from disk and re-walk its MIME tree
	// once per section, so the I/O and the allocation scaled with the number
	// of sections rather than with the message.
	entity, err := cache.part(s, r, sec.Part)
	if err != nil {
		if errors.Is(err, errBlobUnreadable) {
			s.be.Logger.Error("FETCH body-section load blob",
				"mailbox", s.mailboxName, "folder", s.selectedFolderName,
				"uid", r.uid, "sha16", safeShortSHA(r.rawSHA256Hex), "err", err)
			return missingBlobErr()
		}
		var imapErr *imap.Error
		if errors.As(err, &imapErr) {
			// A stale-tenant error is this session's own problem, not the
			// message's; surface it as-is.
			return imapErr
		}
		s.be.Logger.Warn("FETCH body-section walk part",
			"uid", r.uid, "part", partIntPath(sec.Part), "err", err)
		w := resp.WriteBodySection(sec, 0)
		return w.Close()
	}

	payload, ok := selectEntityPayload(sec, entity)
	if !ok {
		// Unknown / unsupported specifier combination — empty response.
		w := resp.WriteBodySection(sec, 0)
		return w.Close()
	}

	payload = applyPartial(payload, sec.Partial)
	w := resp.WriteBodySection(sec, int64(len(payload)))
	if _, err := w.Write(payload); err != nil {
		_ = w.Close()
		return fmt.Errorf("BODY[%s] write: %w", sec.Specifier, err)
	}
	return w.Close()
}

// selectEntityPayload picks the bytes one BODY[...] specifier asks for out of
// a resolved entity. Returns (payload, true) on a recognized shape, and
// (nil, false) on one the spec does not define.
//
// N.MIME and N.HEADER are DIFFERENT sections and were previously conflated
// (RA6X-015). N.MIME is the part's own MIME header: the header it has inside
// its multipart parent or, for the body of a message that is not multipart,
// that message's header. N.HEADER and N.TEXT address the RFC 5322 message
// ENCLOSED by a message/rfc822 part, its header and its body; for any other
// part they are the part's own header and body.
//
// With no part number, HEADER and TEXT are the top-level message's own header
// and body, even when its Content-Type is message/rfc822. Entering the
// enclosed message there returned the enclosed message's body for BODY[TEXT]
// (OPS-004). MIME requires a part number (RFC 9051 §6.4.5).
func selectEntityPayload(sec *imap.FetchItemBodySection, e rawEntity) ([]byte, bool) {
	numbered := len(sec.Part) > 0
	switch sec.Specifier {
	case imap.PartSpecifierNone:
		// BODY[N] (with Part) or BODY[] (already handled above). For the
		// per-part case we return the body bytes.
		if len(sec.HeaderFields) > 0 || len(sec.HeaderFieldsNot) > 0 {
			// FIELDS modifier on a body specifier is undefined — emit empty.
			return nil, false
		}
		return e.body, true

	case imap.PartSpecifierText:
		if numbered {
			if inner, ok := encapsulatedOf(e); ok {
				return inner.body, true
			}
		}
		return e.body, true

	case imap.PartSpecifierMIME:
		if !numbered {
			return nil, false
		}
		return e.header, true

	case imap.PartSpecifierHeader:
		header := e.header
		if numbered {
			if inner, ok := encapsulatedOf(e); ok {
				header = inner.header
			}
		}
		switch {
		case len(sec.HeaderFields) > 0:
			return selectRawFields(header, sec.HeaderFields, false), true
		case len(sec.HeaderFieldsNot) > 0:
			return selectRawFields(header, sec.HeaderFieldsNot, true), true
		default:
			return header, true
		}
	}
	return nil, false
}

// streamRawBody implements the BODY[] (full message) variant by streaming
// directly from the blob, never loading the bytes into memory.
func (s *Session) streamRawBody(resp *imapserver.FetchResponseWriter, r fetchRow, sec *imap.FetchItemBodySection) error {
	bucket := blob.BucketFromTime(r.rawBlobDate)
	rc, err := s.be.BlobStore.Open(blob.KindRaw, s.tenant, bucket, r.rawSHA256Hex)
	if err != nil {
		if stale := s.staleTenantErr(err); stale != nil {
			return stale
		}
		// Store corruption: the DB row exists but its blob is gone. Page
		// the operator via ERROR + NO [SERVERBUG]; never fake an empty body.
		s.be.Logger.Error("FETCH BODY[] open blob",
			"mailbox", s.mailboxName, "folder", s.selectedFolderName,
			"uid", r.uid, "sha16", safeShortSHA(r.rawSHA256Hex), "bucket", bucket, "err", err)
		return missingBlobErr()
	}
	defer rc.Close()

	// The literal length we are about to announce comes from the DB raw_size.
	// If the on-disk blob is a different length (truncated/corrupt store), the
	// io.CopyN below would write fewer bytes than announced and desync the
	// client's literal into a hang. Stat the OPEN fd (race-free vs a re-stat)
	// and refuse cleanly before writing any literal, matching missing-blob
	// handling (R-060). *os.File satisfies the Stat interface.
	if statter, ok := rc.(interface{ Stat() (os.FileInfo, error) }); ok {
		if fi, statErr := statter.Stat(); statErr == nil && fi.Size() != r.rawSize {
			s.be.Logger.Error("FETCH BODY[] blob size mismatch",
				"mailbox", s.mailboxName, "folder", s.selectedFolderName,
				"uid", r.uid, "sha16", safeShortSHA(r.rawSHA256Hex),
				"db_size", r.rawSize, "disk_size", fi.Size())
			return missingBlobErr()
		}
	}

	offset, size := int64(0), r.rawSize
	if sec.Partial != nil {
		offset = sec.Partial.Offset
		if offset > r.rawSize {
			offset = r.rawSize
		}
		size = r.rawSize - offset
		if sec.Partial.Size > 0 && sec.Partial.Size < size {
			size = sec.Partial.Size
		}
		if size < 0 {
			size = 0
		}
		if _, err := io.CopyN(io.Discard, rc, offset); err != nil {
			return fmt.Errorf("BODY[] partial seek: %w", err)
		}
	}

	w := resp.WriteBodySection(sec, size)
	if _, err := io.CopyN(w, rc, size); err != nil {
		_ = w.Close()
		return fmt.Errorf("BODY[] stream: %w", err)
	}
	return w.Close()
}

// loadRawBlob reads the full raw blob into memory for body-section
// slicing. Bounded by the per-message size limit enforced at ingest.
func (s *Session) loadRawBlob(r fetchRow) ([]byte, error) {
	bucket := blob.BucketFromTime(r.rawBlobDate)
	rc, err := s.be.BlobStore.Open(blob.KindRaw, s.tenant, bucket, r.rawSHA256Hex)
	if err != nil {
		if stale := s.staleTenantErr(err); stale != nil {
			return nil, stale
		}
		return nil, err
	}
	defer rc.Close()
	buf := make([]byte, 0, r.rawSize)
	out := bytes.NewBuffer(buf)
	if _, err := io.Copy(out, rc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// loadRawHeaderSection reads ONLY the top-level header section of the raw blob,
// stopping at the header/body separator instead of loading the whole (up to
// 50 MiB) message into memory for a BODY[HEADER] fetch (R-042). The returned
// bytes are byte-identical to splitHeaderBody(fullRaw).header, so on-wire output
// is unchanged.
func (s *Session) loadRawHeaderSection(r fetchRow) ([]byte, error) {
	bucket := blob.BucketFromTime(r.rawBlobDate)
	rc, err := s.be.BlobStore.Open(blob.KindRaw, s.tenant, bucket, r.rawSHA256Hex)
	if err != nil {
		if stale := s.staleTenantErr(err); stale != nil {
			return nil, stale
		}
		return nil, err
	}
	defer rc.Close()
	return readHeaderSection(rc)
}

// readHeaderSection returns the RFC 5322 header section (up to and including the
// separator) from rc, reading no further than necessary. It is byte-identical
// to splitHeaderBody's header for every input: the separator is found by
// ingest.HeaderSeparatorEnd, the function splitHeaderBody itself uses, resumed
// after each read, and the first one found is the message's first since the
// buffer is always a prefix. The no-separator "whole thing is header" case is
// resolved only at EOF.
func readHeaderSection(rc io.Reader) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 32*1024)
	// Whichever empty line comes FIRST wins (RA6X-006).
	//
	// This looked only for CRLF CRLF and fell back to splitHeaderBody at EOF,
	// so an LF-terminated message — which is what Postfix's pipe transport
	// delivers — was read in its ENTIRETY before its header boundary was
	// recognised, defeating the whole point of a header-only read. Worse, a
	// body containing a CRLF blank line ended the scan there, returning body
	// text as headers.
	//
	// It then kept its own copy of the two separator patterns, which drifted
	// from splitHeaderBody's rule: it missed a message that starts with its
	// empty line, and a CRLF empty line after a field ended by a bare LF,
	// ending the section at a later blank line in the body instead
	// (OPS-003). Sharing the definition makes that drift impossible.
	for {
		n, err := rc.Read(tmp)
		if n > 0 {
			scanned := len(buf)
			buf = append(buf, tmp[:n]...)
			if end := ingest.HeaderSeparatorEnd(buf, scanned); end >= 0 {
				return buf[:end], nil
			}
		}
		if err == io.EOF {
			hdr, _ := splitHeaderBody(buf)
			return hdr, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// applyPartial returns p sliced according to the IMAP Partial spec, or
// p unchanged if Partial is nil. Out-of-range offsets clamp to empty.
func applyPartial(p []byte, partial *imap.SectionPartial) []byte {
	if partial == nil {
		return p
	}
	// All arithmetic in int64: partial.Offset/Size come off the wire (up to
	// math.MaxInt64), so `offset + int(partial.Size)` in int space overflows
	// negative for a huge Size, slipping past a naive `end < ...` guard and
	// panicking on p[offset:negative]. Mirror streamRawBody's clamp and never
	// let the end index fall below offset (R-013).
	if partial.Offset >= int64(len(p)) {
		return p[:0]
	}
	offset := partial.Offset
	n := int64(len(p)) - offset
	if partial.Size > 0 && partial.Size < n {
		n = partial.Size
	}
	return p[offset : offset+n]
}

// safeShortSHA returns the first 16 hex chars of s, or "" if shorter.
// Used only for logging — never for path construction.
func safeShortSHA(s string) string {
	if len(s) >= 16 {
		return s[:16]
	}
	return s
}

// fetchRow holds every cheap column for one message — even if the caller
// didn't ask for all of them — to keep the SQL surface to a single SELECT.
// Adding a column to FetchOptions support is just a write call in Fetch.
type fetchRow struct {
	seqNum        uint32
	uid           int64
	modSeq        int64
	rawSize       int64
	internalDate  time.Time
	flags         []string
	messageID     *string
	inReplyTo     *string
	subject       *string
	fromAddr      *string
	toAddrs       []string
	ccAddrs       []string
	sentDate      *time.Time
	headers       []byte // JSONB, lazily decoded if envelope needs Sender / Reply-To / Bcc
	bodyStructure []byte // JSONB, decoded by decodeBodyStructure on demand
	rawSHA256Hex  string // for blob.Store.Open
	rawBlobDate   time.Time
}

// targetRef is the lightweight (seqNum, uid, rawSize) triple a numSet resolves
// to. Sequence numbers are 1-based positions over the folder-wide ORDER BY uid,
// so the resolver queries only the requested UID ranges — three tiny columns,
// never the headers/bodystructure JSONB. STORE/COPY/MOVE need nothing more;
// FETCH loads the heavy columns for the matched UIDs only (R-040).
type targetRef struct {
	seqNum  uint32
	uid     int64
	rawSize int64
}

// resolveTargets maps numSet to the matching messages in ascending UID order
// (also IMAP sequence-number order), reading only (seqnum, uid, raw_size). A
// STORE/COPY/MOVE of a few messages therefore no longer materializes every
// row's JSONB. SeqSet/UIDSet Contains differ, and the "n:*"/bare-"*" dynamic
// forms need the folder's largest seqnum/uid to expand.
func (s *Session) resolveTargets(ctx context.Context, numSet imap.NumSet) ([]targetRef, error) {
	wantUIDs, err := s.targetUIDs(ctx, numSet)
	if err != nil {
		return nil, err
	}
	if len(wantUIDs) == 0 {
		return nil, nil
	}

	// The view says WHICH messages; the database says what they weigh. Rows
	// that have since been deleted simply do not come back — a client may not
	// act on a message that is already gone, and the reconcile that tells it
	// so is the caller's Poll.
	rows, err := s.be.Pool.Query(ctx, `
		SELECT uid, raw_size
		  FROM messages
		 WHERE folder_id = $1 AND uid = ANY($2)
		 ORDER BY uid`, s.selectedFolderID, wantUIDs)
	if err != nil {
		s.be.Logger.Error("resolve targets query", "err", err)
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "fetch query failed"}
	}
	defer rows.Close()

	var matches []targetRef
	for rows.Next() {
		var t targetRef
		if err := rows.Scan(&t.uid, &t.rawSize); err != nil {
			s.be.Logger.Error("resolve targets scan", "err", err)
			return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "fetch scan failed"}
		}
		t.seqNum = s.view.seqOf(t.uid)
		matches = append(matches, t)
	}
	if err := rows.Err(); err != nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "fetch rows error"}
	}
	return matches, nil
}

// fetchProjection is the set of columns one FETCH needs, plus the scan targets
// that fill them. Keeping the column list and the scan destinations in one
// place is what makes the two impossible to get out of step — the defect this
// replaces was a fixed column list that no longer matched what the response
// used (RA6X-054).
type fetchProjection struct {
	columns []string
	// scan builds the destination list for one row, in the same order.
	scan []func(*fetchRow) any
}

func (p fetchProjection) targets(r *fetchRow) []any {
	out := make([]any, len(p.scan))
	for i, f := range p.scan {
		out[i] = f(r)
	}
	return out
}

// fetchEnvelopeNeeded reports whether any requested item is built from the
// envelope columns. ENVELOPE is the obvious one; the header-derived items are
// read from the blob, not from these columns, so they do not pull them in.
func fetchEnvelopeNeeded(o *imap.FetchOptions) bool { return o.Envelope }

// fetchBlobNeeded reports whether any requested item reads the raw blob, which
// is the only thing the content-address columns are for.
func fetchBlobNeeded(o *imap.FetchOptions) bool {
	return o.Envelope || len(o.BodySection) > 0 || len(o.BinarySection) > 0 || len(o.BinarySectionSize) > 0
}

func fetchProjectionFor(o *imap.FetchOptions) fetchProjection {
	if o == nil {
		o = &imap.FetchOptions{}
	}
	p := fetchProjection{}
	add := func(col string, target func(*fetchRow) any) {
		p.columns = append(p.columns, col)
		p.scan = append(p.scan, target)
	}
	// Always: the identity, the size every response may report, the date, and
	// the flags the implicit-\Seen side effect has to inspect.
	add("uid", func(r *fetchRow) any { return &r.uid })
	add("raw_size", func(r *fetchRow) any { return &r.rawSize })
	add("internal_date", func(r *fetchRow) any { return &r.internalDate })
	add("flags", func(r *fetchRow) any { return &r.flags })
	if o.Flags || fetchImpliesSeen(o) {
		add("mod_seq", func(r *fetchRow) any { return &r.modSeq })
	}

	if fetchEnvelopeNeeded(o) {
		add("message_id", func(r *fetchRow) any { return &r.messageID })
		add("in_reply_to", func(r *fetchRow) any { return &r.inReplyTo })
		add("subject", func(r *fetchRow) any { return &r.subject })
		add("from_addr", func(r *fetchRow) any { return &r.fromAddr })
		add("to_addrs", func(r *fetchRow) any { return &r.toAddrs })
		add("cc_addrs", func(r *fetchRow) any { return &r.ccAddrs })
		add("sent_date", func(r *fetchRow) any { return &r.sentDate })
	}
	if o.BodyStructure != nil {
		add("bodystructure", func(r *fetchRow) any { return &r.bodyStructure })
	}
	if fetchBlobNeeded(o) {
		add("encode(raw_sha256, 'hex')", func(r *fetchRow) any { return &r.rawSHA256Hex })
		add("raw_blob_date", func(r *fetchRow) any { return &r.rawBlobDate })
	}
	return p
}

// seqSetContains implements SeqSet's match semantics including "n:*" and
// the bare "*" form, which the upstream library's SeqSet.Contains does not
// resolve without knowing the folder's max sequence number.
func seqSetContains(s imap.SeqSet, num, max uint32) bool {
	for _, r := range s {
		start, stop := r.Start, r.Stop
		if start == 0 {
			start = max
		}
		if stop == 0 {
			stop = max
		}
		if start > stop {
			start, stop = stop, start
		}
		if num >= start && num <= stop {
			return true
		}
	}
	return false
}

func uidSetContains(s imap.UIDSet, uid, max imap.UID) bool {
	for _, r := range s {
		start, stop := r.Start, r.Stop
		if start == 0 {
			start = max
		}
		if stop == 0 {
			stop = max
		}
		if start > stop {
			start, stop = stop, start
		}
		if uid >= start && uid <= stop {
			return true
		}
	}
	return false
}

func toIMAPFlags(ss []string) []imap.Flag {
	out := make([]imap.Flag, 0, len(ss))
	for _, s := range ss {
		out = append(out, imap.Flag(s))
	}
	return out
}

// buildEnvelope assembles an imap.Envelope from the cheap columns plus a
// best-effort decode of the headers JSONB for Sender / Reply-To / Bcc.
// Parse failures degrade gracefully — an unparseable address becomes an
// imap.Address with Name set to the raw value.
func buildEnvelope(r fetchRow) *imap.Envelope {
	var hdrs map[string][]string
	if len(r.headers) > 0 {
		if err := json.Unmarshal(r.headers, &hdrs); err != nil {
			hdrs = nil
		}
	}
	return buildEnvelopeHeaders(r, hdrs)
}

func buildEnvelopeHeaders(r fetchRow, hdrs map[string][]string) *imap.Envelope {
	env := &imap.Envelope{
		Subject:   strPtr(r.subject),
		MessageID: trimAngle(strPtr(r.messageID)),
		InReplyTo: splitMsgIDs(strPtr(r.inReplyTo)),
	}
	if r.sentDate != nil {
		env.Date = *r.sentDate
	}
	// From/To/Cc come from the RETAINED HEADERS, not the convenience columns
	// (RA6X-025).
	//
	// messages.from_addr / to_addrs / cc_addrs are deliberately lossy: ingest
	// stores bare addresses there so routing and filtering have a normalized
	// value to match on. Building the IMAP envelope from them threw away every
	// display name and every group structure the sender sent, so a mail client
	// showed "alice@example.com" where the message said "Alice Example". The
	// headers JSONB has kept the original all along — Sender, Reply-To and Bcc
	// were already read from it, which is exactly the inconsistency.
	//
	// The convenience columns remain the fallback, for a row ingested before
	// the headers map carried a value or whose header was unparseable.
	env.From = firstNonEmptyAddrs(
		parseAddressLists(headerValues(hdrs, "From")),
		parseAddressList(strPtr(r.fromAddr)),
	)
	env.To = firstNonEmptyAddrs(
		parseAddressLists(headerValues(hdrs, "To")),
		parseAddressLists(r.toAddrs),
	)
	env.Cc = firstNonEmptyAddrs(
		parseAddressLists(headerValues(hdrs, "Cc")),
		parseAddressLists(r.ccAddrs),
	)
	env.Sender = parseAddressLists(headerValues(hdrs, "Sender"))
	env.ReplyTo = parseAddressLists(headerValues(hdrs, "Reply-To"))
	env.Bcc = parseAddressLists(headerValues(hdrs, "Bcc"))
	// RFC 9051: when Sender / Reply-To are absent, they MUST default to From.
	if len(env.Sender) == 0 {
		env.Sender = env.From
	}
	if len(env.ReplyTo) == 0 {
		env.ReplyTo = env.From
	}
	return env
}

// headerValues looks a field up in the retained headers map, matching the name
// case-insensitively. The map preserves the sender's capitalisation, so a
// message that wrote "FROM:" would be missed by a direct index.
func headerValues(hdrs map[string][]string, name string) []string {
	if hdrs == nil {
		return nil
	}
	if v, ok := hdrs[name]; ok {
		return v
	}
	for k, v := range hdrs {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}

// firstNonEmptyAddrs returns the first non-empty address list, so the retained
// header wins and the convenience column is the fallback.
func firstNonEmptyAddrs(lists ...[]imap.Address) []imap.Address {
	for _, l := range lists {
		if len(l) > 0 {
			return l
		}
	}
	return nil
}

// strPtr dereferences a *string treating nil as "".
func strPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// trimAngle strips angle brackets from a single Message-ID-style value.
func trimAngle(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") {
		return s[1 : len(s)-1]
	}
	return s
}

// splitMsgIDs returns each angle-bracketed id in s, brackets stripped.
// In-Reply-To is the most common multi-id case ("References" is similar).
func splitMsgIDs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Fields(s) {
		out = append(out, trimAngle(part))
	}
	return out
}

// parseAddressList parses a single comma-separated header value into a
// slice of imap.Address. Falls back to a name-only Address for input
// mail.ParseAddressList can't handle.
func parseAddressList(s string) []imap.Address {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out, err := parseGroupedAddresses(s)
	if err != nil {
		return []imap.Address{{Name: s}}
	}
	return out
}

// parseAddressLists flattens a slice of header values (each may itself be
// a comma-separated address group) into one imap.Address slice.
func parseAddressLists(values []string) []imap.Address {
	var out []imap.Address
	for _, v := range values {
		out = append(out, parseAddressList(v)...)
	}
	return out
}

func toIMAPAddress(a *mail.Address) imap.Address {
	mb, host := splitAddress(a.Address)
	return imap.Address{Name: a.Name, Mailbox: mb, Host: host}
}

// splitAddress splits "local@domain" into ("local", "domain"). For input
// without an "@" we return the whole string as the mailbox part — RFC 9051
// doesn't tolerate an empty host but a malformed source address is the
// caller's bug to live with.
func splitAddress(s string) (mailbox, host string) {
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return s, ""
	}
	return s[:at], s[at+1:]
}

// Store / Expunge / Copy / Move are implemented in mutations.go.

// Create / Delete / Rename: structural folder mutations, each one
// transaction. CLAUDE.md's state-changes table lists these as supported,
// and COPY/MOVE already auto-create destination folders, so refusing the
// explicit CREATE was internally inconsistent (Mail.app's "New Mailbox"
// would fail while drag-to-new-folder worked).

// specialUseFromOptions validates a CREATE's USE parameter and reduces it to
// the single attribute folders.special_use can hold. Returns (nil, nil) when
// the client asked for none.
//
// Anything outside the set the schema models is refused rather than stored, so
// LIST can never emit an attribute a client did not ask for. That set lives in
// epistula-database (storage.CanonicalSpecialUse) because the admin CLI assigns
// the same column to folders that were never CREATEd (OPS-002); one list keeps
// the two from drifting. It matches case-insensitively, as RFC 6154's ABNF
// literals do, and returns the canonical spelling.
func specialUseFromOptions(options *imap.CreateOptions) (*string, *imap.Error) {
	if options == nil || len(options.SpecialUse) == 0 {
		return nil, nil
	}
	// The column holds one attribute. RFC 6154 permits several, but this
	// server has never modelled more than one, and quietly keeping the first
	// would be the same silent-drop defect in a smaller box.
	if len(options.SpecialUse) > 1 {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeCannot,
			Text: "CREATE: only one special-use attribute per mailbox is supported",
		}
	}
	v, ok := storage.CanonicalSpecialUse(string(options.SpecialUse[0]))
	if !ok {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeCannot,
			Text: "CREATE: unsupported special-use attribute",
		}
	}
	return &v, nil
}

func (s *Session) Create(mailbox string, options *imap.CreateOptions) (err error) {
	defer s.guard("CREATE", &err)
	if err := s.requireAuth(); err != nil {
		return err
	}
	ctx, cancel := s.queryCtx()
	defer cancel()

	// Canonicalize AND validate, together (RA6X-009). CREATE used to validate
	// only, so `CREATE inbox` created a folder SELECT could never open.
	mailbox, verr := prepareFolderWrite(mailbox)
	if verr != nil {
		return verr
	}

	// RFC 6154 CREATE ... (USE (\Archive)). go-imap parses the USE parameter
	// and hands it to us in options regardless of whether SPECIAL-USE is
	// advertised (verified on the wire), so ignoring options meant the server
	// answered OK while silently discarding an attribute the client believes
	// it set — and LIST, which reads folders.special_use, would then never
	// report it. Persist it instead (RO5X-007 / RO5X-043).
	specialUse, useErr := specialUseFromOptions(options)
	if useErr != nil {
		return useErr
	}

	// Create missing ancestors, then the leaf, in one transaction.
	//
	// Creating `a/b/c` used to insert exactly one row, so LIST returned a
	// child with no ancestors — the third leg of the same hierarchy problem
	// as RENAME and DELETE (RO5X-008). ALREADYEXISTS is returned only when
	// the LEAF already exists; a pre-existing ancestor is normal and must not
	// fail the command.
	//
	// The rows come from storage.EnsureFolder, the creation path every writer
	// of the schema shares, so delivery, import, COPY/MOVE and CREATE cannot
	// disagree about the hierarchy again (OPS-001). It takes uidvalidity from
	// the shared folder_uidvalidity_seq rather than the clock (R-062) and
	// consumes a value only for a row it inserts (RA6X-052).
	//
	// It requires the mailbox row lock, taken first per the canonical order.
	// DELETE holds that lock while it refuses a folder with inferiors; without
	// it here, a DELETE of `a` could commit between this CREATE finding `a`
	// and inserting `a/b`, and leave `a/b` parentless.
	err = s.be.Storage().RunTx(ctx, func(tx pgx.Tx) error {
		if err := lockMailbox(ctx, tx, s.mailboxID); err != nil {
			return fmt.Errorf("lock mailbox: %w", err)
		}
		_, created, err := storage.EnsureFolder(ctx, tx, s.mailboxID, mailbox, specialUse)
		if err != nil {
			return err
		}
		if !created {
			return errFolderExists
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errFolderExists) {
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeAlreadyExists,
				Text: "mailbox already exists",
			}
		}
		s.be.Logger.Error("CREATE folder", "folder", mailbox, "err", err)
		return responseCannot("CREATE failed")
	}
	return nil
}

// errFolderExists signals an in-tx ALREADYEXISTS without leaking a driver error.
var errFolderExists = errors.New("imapsess: folder exists")

// errFolderGone and errFolderHasChildren carry DELETE's two refusals out of its
// transaction without a driver error reaching the client.
var (
	errFolderGone        = errors.New("imapsess: folder no longer exists")
	errFolderHasChildren = errors.New("imapsess: folder has child mailboxes")
)

func (s *Session) Delete(mailbox string) (err error) {
	defer s.guard("DELETE", &err)
	if err := s.requireAuth(); err != nil {
		return err
	}
	if strings.EqualFold(mailbox, "INBOX") {
		return responseCannot("DELETE: INBOX cannot be deleted")
	}
	ctx, cancel := s.queryCtx()
	defer cancel()

	var deletedFolderID int64

	// Everything that decides the outcome happens INSIDE the transaction,
	// under the canonical lock order (RA6X-020, RA6X-021).
	//
	// Before, folder identity and the child-folder check were read outside it
	// and the quota debit was computed by a scalar subquery over an unlocked
	// message set. Two concurrent DELETEs of the same folder could therefore
	// each read the same 100-byte snapshot and each subtract it: the second
	// affected zero folders and still committed its subtraction, leaving
	// used_bytes below the real total. GREATEST(0, ...) hid the underflow
	// rather than preventing it.
	err = retryTx(ctx, s.be.Pool.Begin, func(tx pgx.Tx) error {
		deletedFolderID = 0

		if err := lockMailbox(ctx, tx, s.mailboxID); err != nil {
			return fmt.Errorf("lock mailbox: %w", err)
		}

		// Resolve and LOCK the target inside the transaction. A folder that
		// has already gone is not an error the accounting may act on: it is a
		// no-op that must leave used_bytes exactly where it is.
		var folderID int64
		if err := tx.QueryRow(ctx,
			`SELECT id FROM folders WHERE mailbox_id = $1 AND name = $2 FOR UPDATE`,
			s.mailboxID, mailbox,
		).Scan(&folderID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errFolderGone
			}
			return fmt.Errorf("lock folder: %w", err)
		}

		// RFC 3501 §6.3.4: deleting a name with inferior hierarchical names
		// must not remove the inferiors. Deleting only the exact row left
		// `Archive/2026` parented to nothing. Refusing is the simplest
		// RFC-conformant option and matches the read-mostly posture; the
		// \Noselect-placeholder alternative would need a folders flag column
		// and a migration, which is not worth taking unless a client needs it
		// (RO5X-008).
		//
		// Re-checked here rather than before the transaction: a child created
		// between an outside check and the delete would otherwise be orphaned.
		var hasChildren bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM folders
			    WHERE mailbox_id = $1 AND name LIKE $2 ESCAPE '\'
			 )`,
			s.mailboxID, escapeLike(mailbox)+`/%`,
		).Scan(&hasChildren); err != nil {
			return fmt.Errorf("child check: %w", err)
		}
		if hasChildren {
			return errFolderHasChildren
		}

		// Level 4: the messages whose bytes are about to be reclaimed. Summing
		// the LOCKED set is what makes the debit correspond to rows this
		// transaction actually removes, rather than to a snapshot another
		// transaction may be subtracting at the same moment.
		locked, err := lockAllMessagesInFolder(ctx, tx, folderID)
		if err != nil {
			return fmt.Errorf("lock messages: %w", err)
		}
		var reclaim int64
		for _, m := range locked {
			reclaim += m.rawSize
		}

		tag, err := tx.Exec(ctx,
			`DELETE FROM folders WHERE id = $1 AND mailbox_id = $2`,
			folderID, s.mailboxID,
		)
		if err != nil {
			return fmt.Errorf("delete folder: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Somebody else removed it despite the lock — impossible in
			// practice, but the accounting must not move if it happens.
			return errFolderGone
		}

		if reclaim > 0 {
			if _, err := tx.Exec(ctx,
				`UPDATE mailboxes
				    SET used_bytes = GREATEST(0, used_bytes - $1), updated_at = now()
				  WHERE id = $2`,
				reclaim, s.mailboxID,
			); err != nil {
				return fmt.Errorf("reclaim quota: %w", err)
			}
		}
		deletedFolderID = folderID
		return nil
	})
	switch {
	case errors.Is(err, errFolderGone):
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "DELETE: no such mailbox",
		}
	case errors.Is(err, errFolderHasChildren):
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "DELETE: mailbox has child mailboxes",
		}
	case isRetryableTxError(err):
		s.be.Logger.Warn("DELETE folder aborted after retries", "folder", mailbox, "err", err)
		return txAborted("DELETE")
	case err != nil:
		s.be.Logger.Error("DELETE folder", "folder", mailbox, "err", err)
		return responseCannot("DELETE failed")
	}
	folderID := deletedFolderID
	if s.selectedFolderID == folderID {
		s.selectedFolderID = 0
		s.selectedFolderName = ""
	}
	return nil
}

func (s *Session) Rename(mailbox, newName string, options *imap.RenameOptions) (err error) {
	defer s.guard("RENAME", &err)
	if err := s.requireAuth(); err != nil {
		return err
	}
	if strings.EqualFold(mailbox, "INBOX") {
		// RFC 3501 gives INBOX-rename special move-the-messages
		// semantics; we don't support that — INBOX is permanent.
		return responseCannot("RENAME: INBOX cannot be renamed")
	}
	ctx, cancel := s.queryCtx()
	defer cancel()

	// The destination goes through the same gate every other folder write
	// does (RA6X-009): RENAME validated neither the target nor the names its
	// descendants would move onto, so it could introduce a name CREATE refuses.
	newName, verr := prepareFolderWrite(newName)
	if verr != nil {
		return verr
	}
	mailbox = canonicalFolderName(mailbox)

	// A folder cannot be renamed into itself or into its own subtree
	// (RA6X-009). `Rename("A", "A/B")` renamed the parent to `A/B` and then ran
	// a descendant UPDATE whose `name LIKE 'A/%'` predicate MATCHED THE
	// JUST-RENAMED PARENT, processing it a second time. Refused before
	// anything is touched.
	if newName == mailbox {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeCannot,
			Text: "RENAME: source and destination are the same mailbox",
		}
	}
	if strings.HasPrefix(newName, mailbox+"/") {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeCannot,
			Text: "RENAME: cannot move a mailbox inside itself",
		}
	}

	folderID, err := s.lookupFolder(ctx, mailbox)
	if err != nil {
		return err
	}

	// Every descendant's NEW name must also be legal (RA6X-009): renaming a
	// short parent to a longer one lengthens every descendant, so a rename
	// within the limit itself can push a child past it.
	if verr := s.checkDescendantNames(ctx, mailbox, newName); verr != nil {
		return verr
	}

	// RFC 3501 §6.3.5 requires RENAME to rename inferior hierarchical names
	// too: renaming `Archive` to `Old` must also turn `Archive/2026` into
	// `Old/2026`. Renaming only the exact row left the child parented to a
	// name that no longer exists — Mail.app and Thunderbird render that as a
	// phantom node or drop the subtree from the tree entirely, so the user's
	// messages are still there but unreachable through the UI (RO5X-008).
	//
	// Parent and descendants move in ONE transaction, with the collision
	// pre-check inside it, so a rename that would clash with an existing name
	// fails atomically before any row is touched.
	//
	// Every renamed row gets a fresh uidvalidity per R-062 — clients must
	// discard cached state for all of them — from the shared
	// folder_uidvalidity_seq rather than time.Now().Unix(), so a rename in the
	// same second a folder was created cannot collide on the old value.
	var renamed int64
	err = s.be.Storage().RunTx(ctx, func(tx pgx.Tx) error {
		// 0. Level 1 of the canonical lock order, first in the transaction.
		//    Step 2 creates rows, and the creation path requires this lock for
		//    the reason CREATE gives. It also keeps the re-prefix in step 4
		//    from missing a child that a delivery or COPY is creating under
		//    the old name at the same moment, which would be left behind
		//    without its parent (OPS-001).
		if err := lockMailbox(ctx, tx, s.mailboxID); err != nil {
			return fmt.Errorf("lock mailbox: %w", err)
		}

		// 1. Refuse if the target name, or any name a descendant would move
		//    onto, already exists.
		var clash bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM folders
			    WHERE mailbox_id = $1
			      AND (name = $2 OR name LIKE $3 ESCAPE '\')
			 )`,
			s.mailboxID, newName, escapeLike(newName)+`/%`,
		).Scan(&clash); err != nil {
			return fmt.Errorf("rename collision check: %w", err)
		}
		if clash {
			return errRenameTargetExists
		}

		// 2. The destination's missing ancestors. RFC 3501 §6.3.5: the
		//    server SHOULD create any superior hierarchical names the RENAME
		//    needs. Renaming `Drafts` to `Old/2026/Drafts` used to leave a
		//    folder whose parents LIST never returned (OPS-001). None of them
		//    can be the source or one of its descendants — that would put the
		//    destination inside the source, which is refused above.
		if _, err := storage.EnsureFolderAncestors(ctx, tx, s.mailboxID, newName); err != nil {
			return fmt.Errorf("create destination ancestors: %w", err)
		}

		// 3. The parent.
		tag, err := tx.Exec(ctx,
			`UPDATE folders
			    SET name = $1, uidvalidity = mail_next_uidvalidity()
			  WHERE id = $2 AND mailbox_id = $3`,
			newName, folderID, s.mailboxID)
		if err != nil {
			return fmt.Errorf("rename parent: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return errRenameTargetExists // vanished under us
		}
		renamed = tag.RowsAffected()

		// 4. Re-prefix every descendant. substring() keeps the part after the
		//    old prefix, so `Archive/2026/Q1` under old=`Archive`,
		//    new=`Old` becomes `Old` || `/2026/Q1`.
		//
		//    The suffix is sliced by CHARACTERS, not bytes (RA6X-007). Go's
		//    len() counts UTF-8 bytes while PostgreSQL's substring(text from
		//    n) counts characters, so for a parent named `é` the bound was 3
		//    where the correct position is 2: renaming `é` to `B` produced
		//    `BChild` instead of `B/Child`. Longer multibyte names discard
		//    proportionally more of the suffix, silently merging distinct
		//    folders or colliding and rolling the whole rename back.
		//
		//    char_length() of the OLD name, evaluated in the database, is the
		//    only value that agrees with substring()'s own unit — deriving it
		//    in Go would just be a second guess at PostgreSQL's semantics.
		dtag, err := tx.Exec(ctx,
			`UPDATE folders
			    SET name = $1 || substring(name from char_length($2::text) + 1),
			        uidvalidity = mail_next_uidvalidity()
			  WHERE mailbox_id = $3 AND name LIKE $4 ESCAPE '\'`,
			newName, mailbox, s.mailboxID, escapeLike(mailbox)+`/%`)
		if err != nil {
			return fmt.Errorf("rename descendants: %w", err)
		}
		renamed += dtag.RowsAffected()
		return nil
	})
	if err != nil {
		if errors.Is(err, errRenameTargetExists) {
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeAlreadyExists,
				Text: "target mailbox already exists",
			}
		}
		s.be.Logger.Error("RENAME folder", "folder", mailbox, "new", newName, "err", err)
		return responseCannot("RENAME failed")
	}
	s.be.Logger.Info("renamed folder", "from", mailbox, "to", newName, "folders", renamed)

	// Fix up the selected-folder name. This must also fire when the SELECTed
	// folder is a *descendant* of the renamed parent, not just the parent
	// itself — otherwise the session keeps reporting a name that no longer
	// exists.
	if s.selectedFolderID == folderID {
		s.selectedFolderName = newName
	} else if strings.HasPrefix(s.selectedFolderName, mailbox+"/") {
		s.selectedFolderName = newName + strings.TrimPrefix(s.selectedFolderName, mailbox)
	}
	return nil
}

// errRenameTargetExists signals the in-tx collision pre-check so the caller can
// map it to ALREADYEXISTS without leaking a driver error.
var errRenameTargetExists = errors.New("imapsess: rename target exists")

// Append is implemented in append.go.

// ----------- internal helpers -----------

// lookupFolder returns the folder id for (s.mailboxID, name). Returns an
// IMAP-shaped error suitable for surfacing directly to the client.
// maxFolderNameBytes is the widely-assumed IMAP mailbox-name ceiling. The
// schema does not constrain folders.name, so without this a client could
// park a 10 KB name in the folder list for every device to render.
const maxFolderNameBytes = 255

// canonicalFolderName normalizes INBOX — and only INBOX — case-insensitively.
//
// RFC 3501 §5.1 makes INBOX the one case-insensitive mailbox name; every
// other name is case-sensitive. lookupFolder was exact-match, so `APPEND
// inbox` 404'd where `APPEND INBOX` succeeded, while Delete and Rename
// already special-cased it with strings.EqualFold. Doing it here fixes
// Select, Status, List, Append, Subscribe, and Unsubscribe in one place
// (RO5X-012).
func canonicalFolderName(name string) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}

// prepareFolderWrite is the single gate every path that WRITES a folder name
// must pass through (RA6X-009).
//
// The three writers disagreed. CREATE validated but did not canonicalize, so
// `CREATE inbox` inserted a literal "inbox" that `SELECT inbox` — which does
// canonicalize — could never select, leaving a folder the user sees in LIST
// and can never open. COPY/MOVE auto-created their destination with neither
// validation nor canonicalization, so a control character or an over-length
// name that CREATE refuses walked in through a drag-and-drop. RENAME validated
// neither its destination nor the names its descendants would move onto.
//
// Returning the canonical form and the error together makes the correct usage
// the obvious one: a caller cannot validate and then forget to canonicalize.
func prepareFolderWrite(name string) (string, *imap.Error) {
	canonical := canonicalFolderName(name)
	if verr := validateFolderName(canonical); verr != nil {
		return "", verr
	}
	return canonical, nil
}

// validateFolderName rejects names the store should never hold. Applied by
// CREATE and APPEND alike (RO5X-012).
func validateFolderName(name string) *imap.Error {
	if name == "" {
		// go-imap should reject this at the parser; guard anyway.
		return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "mailbox name is required"}
	}
	if len(name) > maxFolderNameBytes {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeCannot,
			Text: fmt.Sprintf("mailbox name exceeds %d bytes", maxFolderNameBytes),
		}
	}
	for _, r := range name {
		// NUL would truncate the name for anything reading it as a C
		// string; the other controls corrupt the LIST response framing.
		if r < 0x20 || r == 0x7f {
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeCannot,
				Text: "mailbox name contains a control character",
			}
		}
	}
	return nil
}

func (s *Session) lookupFolder(ctx context.Context, name string) (int64, error) {
	var id int64
	err := s.be.Pool.QueryRow(ctx,
		`SELECT id FROM folders WHERE mailbox_id = $1 AND name = $2`,
		s.mailboxID, canonicalFolderName(name),
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: fmt.Sprintf("no such mailbox: %s", name),
		}
	}
	if err != nil {
		return 0, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "folder lookup failed"}
	}
	return id, nil
}

// queryCtx returns a context bounded by the per-statement timeout the
// daemon was configured with.
func (s *Session) queryCtx() (context.Context, context.CancelFunc) {
	t := s.be.StmtTimeout
	if t <= 0 {
		t = 10 * time.Second
	}
	base := s.sessCtx
	if base == nil {
		base = context.Background()
	}
	return context.WithTimeout(base, t)
}

// responseCannot returns a polite IMAP NO with the given text. Used when the
// server declines or cannot complete a request: an unsupported form, a
// protected INBOX, a failed folder operation.
func responseCannot(text string) error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Text: text}
}

// responseBadState returns a BAD for a command issued in the wrong session
// state (e.g. FETCH with no mailbox selected) — RFC 3501 §7.1.3 reserves
// BAD for protocol-level errors, NO for operational failures.
func responseBadState(text string) error {
	return &imap.Error{Type: imap.StatusResponseTypeBad, Text: text}
}

// requireWritable refuses mutations on a folder selected via EXAMINE.
// RFC 3501 §6.3.2: a read-only selection must reject state changes
// (STORE, EXPUNGE, MOVE's source-side delete).
func (s *Session) requireWritable(cmd string) error {
	if s.selectedReadOnly {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: cmd + ": mailbox is selected read-only (EXAMINE)",
		}
	}
	return nil
}

// missingBlobErr is the FETCH response for a message row whose on-disk
// blob is missing — store corruption, an operator-paging condition.
func missingBlobErr() error {
	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeServerBug,
		Text: "message body unavailable",
	}
}

// standardFlags returns the set of IMAP system flags this server supports.
// User-defined keywords (e.g., $Forwarded for Maildir's P flag) are not
// advertised in PERMANENTFLAGS but clients can still STORE them; some
// servers omit them from the advertised set and let them propagate
// implicitly. We do the same.
func standardFlags() []imap.Flag {
	return []imap.Flag{
		imap.FlagSeen,
		imap.FlagAnswered,
		imap.FlagFlagged,
		imap.FlagDeleted,
		imap.FlagDraft,
	}
}

// permanentFlagsFor builds the PERMANENTFLAGS a SELECT or EXAMINE advertises
// (RA6X-064).
//
// Both used to be handed standardFlags(): the five system flags and nothing
// else, despite STORE persisting arbitrary keywords. A conforming client reads
// PERMANENTFLAGS to decide what it may set, so it either disabled its
// keyword/tag controls or assumed a keyword it did set was session-only and
// would not survive — while the server was in fact storing it durably.
//
// readOnly gets an EMPTY set, which RFC 9051 §7.3.2 defines as "no permanent
// changes are possible": EXAMINE is exactly that, and advertising a writable
// set for it invited a client to try a STORE the server would refuse.
//
// The `\*` wildcard says new keywords may be created, which is what STORE
// actually does — it writes whatever keyword arrives, with no registry to
// consult.
func permanentFlagsFor(readOnly bool) []imap.Flag {
	if readOnly {
		return []imap.Flag{}
	}
	// The known flags, then the wildcard. Order is not significant to the
	// protocol; keeping the wildcard last matches how the RFC's examples read.
	out := standardFlags()
	return append(out, imap.FlagWildcard)
}

// closeStaleSession tears down a connection whose cached mailbox identity is no
// longer valid — the account's blob tenant tree is being moved, so the name
// this session resolved at LOGIN no longer says where its mail lives
// (RA6X-013).
//
// The cached name is not repairable in place: the session may be mid-command,
// holding a selected folder and a client-side view built from the old identity,
// and a silent re-resolve would leave the client believing nothing happened.
// Dropping the connection is the honest signal; every IMAP client reconnects,
// and reconnecting re-resolves the mailbox from the database.
//
// Safe to call more than once and safe when there is no underlying net.Conn
// (in-process tests): it only cancels the session context and, if a connection
// is attached, closes it.
func (s *Session) closeStaleSession() {
	if s.sessCancel != nil {
		s.sessCancel()
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

// staleTenantErr explains a failed blob read that is caused by this session's
// cached mailbox identity having gone stale, rather than by a corrupt store
// (RA6X-013).
//
// A session resolves the mailbox name — which IS the on-disk blob tenant path —
// once at LOGIN and holds it until disconnect. If an operator moves that tree,
// every blob path this session computes points at a directory that no longer
// exists, and the reads fail with ENOENT. Reported as-is that looks exactly
// like store corruption: an ERROR log and NO [SERVERBUG], paging an operator
// who is in the middle of a planned maintenance.
//
// So on a not-found, ask the database whether this mailbox has moved or is
// being moved. If it has, the session's whole view is stale, not just this
// read, so the connection is dropped and the client told to reconnect —
// reconnecting re-resolves the mailbox. Returns nil when the mailbox is exactly
// where this session thinks it is, which means the missing blob is real
// corruption and the caller's existing handling is correct.
func (s *Session) staleTenantErr(cause error) error {
	if !errors.Is(cause, fs.ErrNotExist) {
		return nil
	}
	if s.mailboxID == 0 {
		return nil
	}
	ctx, cancel := s.queryCtx()
	defer cancel()

	var currentName string
	var maintenanceAt *time.Time
	if err := s.be.Pool.QueryRow(ctx,
		`SELECT name, maintenance_at FROM mailboxes WHERE id = $1`, s.mailboxID,
	).Scan(&currentName, &maintenanceAt); err != nil {
		// Cannot tell. Fall through to the caller's corruption handling rather
		// than inventing a reason.
		return nil
	}
	if maintenanceAt == nil && currentName == s.mailboxName {
		return nil
	}

	s.be.Logger.Warn("blob read failed because this session's mailbox is being moved; closing the session",
		"session_mailbox", s.mailboxName, "current_mailbox", currentName,
		"in_maintenance", maintenanceAt != nil)
	s.closeStaleSession()
	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeUnavailable,
		Text: "mailbox is temporarily unavailable; reconnect",
	}
}

// checkDescendantNames verifies that every descendant of oldName would still
// be a legal folder name after being re-prefixed with newName (RA6X-009).
//
// The transformation changes each descendant's length, so a rename that is
// itself within the limit can push a child past it — and the rename would then
// either commit a name the store should never hold, or fail deep inside the
// descendant UPDATE with an error the client cannot act on.
func (s *Session) checkDescendantNames(ctx context.Context, oldName, newName string) *imap.Error {
	rows, err := s.be.Pool.Query(ctx,
		`SELECT name FROM folders WHERE mailbox_id = $1 AND name LIKE $2 ESCAPE '\'`,
		s.mailboxID, escapeLike(oldName)+`/%`)
	if err != nil {
		s.be.Logger.Error("RENAME descendant check", "folder", oldName, "err", err)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "RENAME failed"}
	}
	defer rows.Close()
	oldRunes := len([]rune(oldName))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			s.be.Logger.Error("RENAME descendant scan", "err", err)
			return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "RENAME failed"}
		}
		// Sliced by RUNES, matching what the SQL does with char_length.
		r := []rune(name)
		if len(r) < oldRunes {
			continue
		}
		candidate := newName + string(r[oldRunes:])
		if verr := validateFolderName(candidate); verr != nil {
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeCannot,
				Text: "RENAME: descendant would become invalid: " + verr.Text,
			}
		}
	}
	if err := rows.Err(); err != nil {
		s.be.Logger.Error("RENAME descendant rows", "err", err)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "RENAME failed"}
	}
	return nil
}

// errBlobUnreadable marks a cache miss caused by the message's blob being
// unreadable, so the caller can answer NO [SERVERBUG] rather than an empty
// section (store corruption is an operator-paging condition).
var errBlobUnreadable = errors.New("imapsess: raw blob unreadable")

// messageSections caches, for the lifetime of ONE message within ONE FETCH,
// the raw blob bytes and every MIME part already resolved from them
// (RA6X-054).
//
// A multi-section FETCH — `BODY[1] BODY[2] BODY[1.MIME]`, which is what a
// client rendering a multipart message issues — used to read the whole message
// from disk and walk its MIME tree once PER SECTION. The I/O and the
// allocation therefore scaled with the number of sections asked for rather
// than with the size of the message, and every walk produced identical results
// from identical bytes.
//
// The cache is deliberately per message and not per session: it is dropped as
// soon as the message's response is written, so the bytes retained at any
// moment are bounded by one message — the same bound the ingest-time size cap
// already enforces — rather than by the size of the fetched set.
type messageSections struct {
	raw     []byte
	rawErr  error
	rawDone bool

	header     []byte
	headerErr  error
	headerDone bool

	parts map[string]rawEntity
}

// topHeader returns the message's own header section, reading no further than
// the separator when nothing has needed the full blob yet. If the whole
// message is already resident, it is split from that rather than read again.
func (c *messageSections) topHeader(s *Session, r fetchRow) ([]byte, error) {
	if c.headerDone {
		return c.header, c.headerErr
	}
	c.headerDone = true
	if c.rawDone && c.rawErr == nil {
		c.header, _ = splitHeaderBody(c.raw)
		return c.header, nil
	}
	c.header, c.headerErr = s.loadRawHeaderSection(r)
	return c.header, c.headerErr
}

// rawBytes returns the whole message, reading it at most once.
func (c *messageSections) rawBytes(s *Session, r fetchRow) ([]byte, error) {
	if c.rawDone {
		return c.raw, c.rawErr
	}
	c.rawDone = true
	c.raw, c.rawErr = s.loadRawBlob(r)
	if c.rawErr != nil {
		// A stale-tenant error is an IMAP error the caller must surface as-is;
		// anything else is store corruption.
		if _, isIMAP := c.rawErr.(*imap.Error); !isIMAP {
			c.rawErr = fmt.Errorf("%w: %v", errBlobUnreadable, c.rawErr)
		}
	}
	return c.raw, c.rawErr
}

// part resolves one MIME part path against the message, reusing an earlier
// walk for the same path.
func (c *messageSections) part(s *Session, r fetchRow, path []int) (rawEntity, error) {
	key := partIntPath(path)
	if e, ok := c.parts[key]; ok {
		return e, nil
	}
	raw, err := c.rawBytes(s, r)
	if err != nil {
		return rawEntity{}, err
	}
	e, err := resolveRawPart(raw, path)
	if err != nil {
		return rawEntity{}, err
	}
	if c.parts == nil {
		c.parts = make(map[string]rawEntity, 4)
	}
	c.parts[key] = e
	return e, nil
}
