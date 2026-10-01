package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/epistula-mail/database/auth"
)

// Bearer-token authentication and authorization.
//
// A presented token is "mapi_<id>_<secret>" (see the auth package). The id
// selects a single api_tokens row; the secret verifies against its Argon2id
// hash. Successful verifications are cached in memory for AuthCacheTTL,
// keyed by sha256 of the presented token — Argon2id at interactive cost per
// request would otherwise dominate API CPU. Revocation therefore takes
// effect within at most one TTL.

// apiToken is the resolved authorization context attached to a request.
type apiToken struct {
	ID           int64
	Name         string
	AllMailboxes bool
	// ScopeMailboxIDs is the authoritative scope: durable mailbox IDs, not
	// names (RA6X-012). mailboxes.id is a BIGSERIAL that is never reused, so
	// an ID whose mailbox was deleted resolves to nothing forever, and a
	// mailbox recreated under the same name is a different account this token
	// was never granted. Renames need no rewrite: the token follows the
	// account. Every SQL scope predicate filters on this.
	ScopeMailboxIDs []int64
	permissions     map[string]bool
}

// Can reports whether the token carries the permission. read_content
// subsumes read_metadata: a token allowed to read bodies can necessarily
// enumerate what exists.
func (t *apiToken) Can(perm string) bool {
	if t.permissions[perm] {
		return true
	}
	return perm == auth.PermissionReadMetadata && t.permissions[auth.PermissionReadContent]
}

// InScope reports whether the token may address the mailbox with this ID.
//
// It takes an ID, not a name, deliberately. A name-keyed check was the
// RA6X-012 defect: a token scoped to a deleted account kept authorizing
// against whatever mailbox later took that name, and the verification cache
// made a rename-then-reuse exploitable too. An ID cannot be recycled, so a
// stale scope entry simply matches nothing — including a cached one.
func (t *apiToken) InScope(mailboxID int64) bool {
	if t.AllMailboxes {
		return true
	}
	for _, id := range t.ScopeMailboxIDs {
		if id == mailboxID {
			return true
		}
	}
	return false
}

type cachedToken struct {
	tok     apiToken
	expires time.Time
}

// authenticator verifies bearer tokens against api_tokens with an in-memory
// verify cache and a per-IP failure throttle.
type authenticator struct {
	srv *server

	mu             sync.Mutex
	cache          map[[32]byte]cachedToken
	fails          map[string][]time.Time
	recordsSinceGC int // failures recorded since the last global fails sweep

	// argonSem caps concurrent Argon2id verifications. Each verify is 64 MiB +
	// CPU; without a cap, N parallel bad-token requests (all cache-missing)
	// would each run a full KDF before the failure throttle records anything,
	// a transient memory-DoS window. Non-blocking acquire → 429 when saturated.
	argonSem chan struct{}
}

func newAuthenticator(srv *server) *authenticator {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	return &authenticator{
		srv:      srv,
		cache:    make(map[[32]byte]cachedToken),
		fails:    make(map[string][]time.Time),
		argonSem: make(chan struct{}, n),
	}
}

// clientIP resolves the caller's IP for throttling. The Go process binds
// loopback with Apache in front, so when the TCP peer is loopback and
// Apache appended an X-Forwarded-For entry, the last entry (the one Apache
// itself added) is the real client. A non-loopback peer's XFF is untrusted
// and ignored.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !peer.IsLoopback() {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	parts := strings.Split(xff, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if net.ParseIP(last) != nil {
		return last
	}
	return host
}

// throttled reports whether ip has exceeded the failure budget within the
// rolling window. Caller holds no lock.
func (a *authenticator) throttled(ip string) bool {
	limit := a.srv.cfg.Limits.AuthFailLimit
	window := a.srv.cfg.AuthFailWindowDuration()
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := time.Now().Add(-window)
	fresh := a.fails[ip][:0]
	for _, t := range a.fails[ip] {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) == 0 {
		delete(a.fails, ip)
	} else {
		a.fails[ip] = fresh
	}
	return len(fresh) >= limit
}

// failsSweepInterval bounds how often recordFailure runs the opportunistic
// global prune below.
const failsSweepInterval = 256

func (a *authenticator) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fails[ip] = append(a.fails[ip], time.Now())

	// throttled() only ever prunes the requesting IP, so an IP that fails once
	// and never returns keeps its entry forever — a slow scan from many
	// distinct IPs grows a.fails without bound over the process lifetime. Every
	// failsSweepInterval records, drop every IP whose newest failure is older
	// than the window (R-069). Amortized O(1) per failure; runs under the lock.
	a.recordsSinceGC++
	if a.recordsSinceGC < failsSweepInterval {
		return
	}
	a.recordsSinceGC = 0
	cutoff := time.Now().Add(-a.srv.cfg.AuthFailWindowDuration())
	for k, ts := range a.fails {
		if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
			delete(a.fails, k)
		}
	}
}

// authenticate resolves the request's bearer token, or writes the RFC 7807
// error response and returns nil. The fixed failure delay keeps invalid-id,
// bad-secret, and revoked outcomes indistinguishable by timing class
// (Argon2id cost differences remain, as on any verify path).
func (a *authenticator) authenticate(w http.ResponseWriter, r *http.Request) *apiToken {
	ip := clientIP(r)
	if a.throttled(ip) {
		metricAuthFailures.WithLabelValues("throttled").Inc()
		problemTooMany(w, r, "Too many failed authentications; retry later.")
		return nil
	}

	header := r.Header.Get("Authorization")
	bearer, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || bearer == "" {
		metricAuthFailures.WithLabelValues("missing").Inc()
		problemUnauthorized(w, r, "Missing bearer token.")
		return nil
	}

	key := sha256.Sum256([]byte(bearer))
	now := time.Now()
	a.mu.Lock()
	if c, hit := a.cache[key]; hit && now.Before(c.expires) {
		tok := c.tok
		a.mu.Unlock()
		return &tok
	}
	a.mu.Unlock()

	// Reserve an Argon2 slot before the (expensive) verify. Non-blocking: a
	// saturated verifier returns 429 rather than piling up 64 MiB KDFs. The
	// worker never contends here — its valid token is served from the cache
	// after the first verify. (R-016)
	select {
	case a.argonSem <- struct{}{}:
		defer func() { <-a.argonSem }()
	default:
		metricAuthFailures.WithLabelValues("argon_saturated").Inc()
		problemTooMany(w, r, "Authentication is busy; retry shortly.")
		return nil
	}

	tok, err := a.verify(r.Context(), bearer)
	if err != nil {
		if !errors.Is(err, errAuthRejected) {
			slog.Error("token verify", "err", err)
			problemUnavailable(w, r, "Authentication backend unavailable.")
			return nil
		}
		a.recordFailure(ip)
		metricAuthFailures.WithLabelValues("rejected").Inc()
		time.Sleep(a.srv.cfg.AuthFailDelayDuration())
		problemUnauthorized(w, r, "Invalid or revoked token.")
		return nil
	}

	if ttl := a.srv.cfg.AuthCacheTTLDuration(); ttl > 0 {
		a.mu.Lock()
		// Lazy prune keeps the map bounded by the live-token count.
		for k, c := range a.cache {
			if now.After(c.expires) {
				delete(a.cache, k)
			}
		}
		a.cache[key] = cachedToken{tok: *tok, expires: now.Add(ttl)}
		a.mu.Unlock()
	}
	return tok
}

// errAuthRejected is the "client presented bad credentials" class; every
// other verify error is an infrastructure failure (5xx, not 401).
var errAuthRejected = errors.New("auth rejected")

func (a *authenticator) verify(ctx context.Context, bearer string) (*apiToken, error) {
	id, secret, err := auth.ParseAPIToken(bearer)
	if err != nil {
		return nil, errAuthRejected
	}

	var (
		name         string
		hash         string
		allMailboxes bool
		scopeIDs     []int64
		perms        []string
		revokedAt    *time.Time
	)
	err = a.srv.pool.QueryRow(ctx,
		`SELECT name, token_hash, scope_all_mailboxes, scope_mailbox_ids, permissions, revoked_at
		   FROM api_tokens WHERE id = $1`,
		id,
	).Scan(&name, &hash, &allMailboxes, &scopeIDs, &perms, &revokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errAuthRejected
		}
		return nil, err
	}
	if revokedAt != nil {
		return nil, errAuthRejected
	}
	ok, err := auth.VerifyPassword(secret, hash)
	if err != nil || !ok {
		return nil, errAuthRejected
	}

	// Best-effort usage stamp; bounded to one write per cache TTL per
	// token because verified tokens come from the cache in between.
	if _, err := a.srv.pool.Exec(ctx,
		`UPDATE api_tokens SET last_used_at = now() WHERE id = $1`, id,
	); err != nil {
		slog.Warn("api token last_used_at update", "token_id", id, "err", err)
	}

	tok := &apiToken{
		ID:              id,
		Name:            name,
		AllMailboxes:    allMailboxes,
		ScopeMailboxIDs: scopeIDs,
		permissions:     make(map[string]bool, len(perms)),
	}
	for _, p := range perms {
		tok.permissions[p] = true
	}
	return tok, nil
}

// requirePermission writes a 403 and returns false when the token lacks
// perm. The detail names the permission, never the token's actual grants.
func requirePermission(w http.ResponseWriter, r *http.Request, tok *apiToken, perm string) bool {
	if tok.Can(perm) {
		return true
	}
	problemForbidden(w, r, "Token lacks the "+perm+" permission.")
	return false
}

// requireMailboxScope writes a 403 and returns false when the mailbox is
// outside the token's scope. Out-of-scope is 403 (authenticated but not
// authorized), never 404 — per the project contract.
//
// The check is by durable mailbox ID; mailboxName is only for the message.
func requireMailboxScope(w http.ResponseWriter, r *http.Request, tok *apiToken, mailboxID int64, mailboxName string) bool {
	if tok.InScope(mailboxID) {
		return true
	}
	problemForbidden(w, r, "Token is not scoped to mailbox '"+mailboxName+"'.")
	return false
}
