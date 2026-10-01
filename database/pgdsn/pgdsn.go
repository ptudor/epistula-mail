// Package pgdsn answers one security question for every consumer of the mail
// store: does this PostgreSQL DSN actually give us an authenticated,
// encrypted connection?
//
// It exists because the answer cannot be obtained by looking at the DSN text.
// The production guard in epistula-database, epistula-imap and epistula-api used to
// substring-search the lowercased DSN for "sslmode=verify-full", which is
// wrong in both directions (RA6X-029):
//
//	postgres://u@db/mail?sslmode=%64isable&application_name=sslmode=verify-full
//
// passes that check — the positive substring is in application_name, and the
// negative substring never appears because "disable" is percent-escaped — while
// pgx resolves the effective mode to `disable` and connects in plaintext.
// Conversely a perfectly secure DSN whose password or application_name happens
// to contain "sslmode=allow" is rejected.
//
// So this package asks pgx the same way the connection layer does: it calls
// pgconn.ParseConfig and inspects the *resolved* TLS configuration, including
// every fallback pgx is permitted to attempt. Both DSN spellings (URL and
// libpq keyword/value), percent- and backslash-escaping, repeated-parameter
// precedence, PG* environment variables and service files are handled by that
// one call, because it is literally the same code path that opens the socket.
package pgdsn

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNoDSN reports an empty DSN. Callers usually check this earlier with a
// clearer per-project message; it exists so RequireVerifiedTLS never reports
// an empty DSN as a TLS problem.
var ErrNoDSN = errors.New("postgres DSN is empty")

// RequireVerifiedTLS reports nil when every connection pgx could make with
// this DSN is either
//
//   - a Unix-domain socket, or
//   - a TCP connection with TLS whose server certificate is verified against
//     the configured roots (libpq sslmode=verify-full, sslmode=verify-ca, or
//     sslmode=require with an sslrootcert, which libpq and pgx both promote to
//     verify-ca semantics).
//
// It returns a descriptive error otherwise. The error never contains the DSN,
// because a DSN routinely carries a password.
//
// # Unix-domain socket policy
//
// A Unix socket is accepted without TLS, deliberately. There is no network to
// intercept: the peer is on this host and access is governed by filesystem
// permissions on the socket, which is the same trust boundary that protects
// the TOML config file holding the DSN in the first place. Requiring TLS there
// would gain nothing and would push deployments toward TCP, which is strictly
// worse. pgx identifies these hosts the same way libpq does — a host beginning
// with "/" (a socket directory) or "@" (a Linux abstract socket).
//
// # Fallbacks
//
// sslmode=allow and sslmode=prefer both resolve to a *list* of TLS configs
// containing a nil entry, meaning "try plaintext". pgx will use it whenever the
// TLS attempt fails, so an attacker who can break the TLS handshake — or a
// server with TLS simply switched off — downgrades the connection silently.
// Every entry must therefore be verified, not just the first.
func RequireVerifiedTLS(dsn string) error {
	if strings.TrimSpace(dsn) == "" {
		return ErrNoDSN
	}

	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		// Reaching here is itself a finding: whatever the connection layer
		// cannot parse, it also cannot connect with. The common real cause is
		// an sslrootcert/sslcert/sslkey the daemon user cannot read, which is
		// exactly what check-config should catch before a restart.
		//
		// The cause is worth showing, but pgconn's ParseConfigError embeds the
		// connection string, so scrub any verbatim copy of the DSN out of it
		// rather than trusting an upstream redaction to cover every secret.
		return fmt.Errorf("postgres DSN is not usable by the connection layer: %s", scrub(dsn, err))
	}

	// The primary target plus every permitted fallback. pgx tries them in
	// order and any one of them can be the connection that actually carries
	// credentials and mail.
	targets := make([]target, 0, 1+len(cfg.Fallbacks))
	targets = append(targets, target{host: cfg.Host, port: cfg.Port, tls: cfg.TLSConfig})
	for _, fb := range cfg.Fallbacks {
		targets = append(targets, target{host: fb.Host, port: fb.Port, tls: fb.TLSConfig})
	}

	// Report the most severe problem first. A mode like `prefer` is BOTH
	// unverified on its TLS attempt and plaintext on its fallback; naming the
	// plaintext fallback is the more useful diagnosis.
	for _, t := range targets {
		if !isUnixSocket(t.host) && t.tls == nil {
			return fmt.Errorf("effective sslmode for host %q allows an unencrypted TCP connection "+
				"(sslmode=disable, allow or prefer); use verify-full (verify-ca permitted)",
				redactHost(t.host, t.port))
		}
	}
	for _, t := range targets {
		// pgx expresses verify-ca as InsecureSkipVerify with an explicit chain
		// check in VerifyPeerCertificate, and verify-full as ordinary
		// verification against ServerName. Plain `require` sets
		// InsecureSkipVerify with no replacement check: encrypted, but to an
		// unauthenticated peer.
		if isUnixSocket(t.host) || t.tls == nil {
			continue
		}
		if t.tls.InsecureSkipVerify && t.tls.VerifyPeerCertificate == nil {
			return fmt.Errorf("effective sslmode for host %q encrypts but does not verify the server "+
				"certificate (sslmode=require without sslrootcert); use verify-full (verify-ca permitted)",
				redactHost(t.host, t.port))
		}
	}
	return nil
}

type target struct {
	host string
	port uint16
	tls  *tls.Config
}

// scrub renders err without any verbatim copy of the DSN. A DSN carries a
// password, and url.Parse-style errors quote their whole input.
func scrub(dsn string, err error) string {
	msg := err.Error()
	for _, secret := range []string{dsn, strings.TrimSpace(dsn)} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "<dsn redacted>")
		}
	}
	return msg
}

// isUnixSocket matches pgx/libpq's own rule for a local socket host.
func isUnixSocket(host string) bool {
	return strings.HasPrefix(host, "/") || strings.HasPrefix(host, "@")
}

// redactHost renders a target for an operator without leaking credentials. The
// host and port come from the parsed config, never from the raw DSN string, so
// no userinfo can ride along.
func redactHost(host string, port uint16) string {
	if host == "" {
		return "(unset)"
	}
	if port == 0 {
		return host
	}
	return fmt.Sprintf("%s:%d", host, port)
}
