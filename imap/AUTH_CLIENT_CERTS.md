# Epistula design: TLS client-certificate auth for IMAP (SASL EXTERNAL)

Status: **not implemented.** This is the design, written 2026-08-07 after
confirming every dependency is already present. Nothing here has been built.

## Why

An operator that already uses client certificates for SMTP relay could also
use certificate credentials for IMAP. This optional design avoids password
verification for certificate-capable clients without introducing a separate
authentication daemon. The operator would provision the trusted CA and bind
each accepted certificate to a mailbox.

The mailbox password (Argon2id PHC in `mailboxes.password_hash`) stays as the
fallback for clients that can't do certificates.

## The core idea: `VerifyClientCertIfGiven` gives you both

```go
tlsCfg := &tls.Config{
    GetCertificate: reloader.GetCertificate,
    MinVersion:     minVer,
    CipherSuites:   aeadCipherSuites,
    ClientAuth:     tls.VerifyClientCertIfGiven,  // <- the whole trick
    ClientCAs:      trustedClientCAPool,          // operator-provisioned client CA
}
```

Three outcomes, which is exactly the behaviour wanted:

| Client presents | TLS handshake | Then |
|---|---|---|
| a valid cert signed by our CA | succeeds, `PeerCertificates` populated | offer `AUTH=EXTERNAL` |
| no cert at all | succeeds, `PeerCertificates` empty | `AUTH=PLAIN` as today |
| an invalid or untrusted cert | **fails during handshake** | connection dies, correctly |

`RequireAndVerifyClientCert` would break every password client. `RequestClientCert`
would accept unverified certs. `VerifyClientCertIfGiven` is the only mode that
means "certs are welcome and checked, absence is fine."

So clients with a certificate use it, clients without one keep their password,
and a forged cert never gets as far as the IMAP layer.

## What exists already (verified, not assumed)

- `github.com/emersion/go-imap/v2 v2.0.0-beta.8` exposes an optional interface
  in `imapserver/session.go:104`:
  ```go
  type SessionSASL interface {
      Session
      AuthenticateMechanisms() []string
      Authenticate(mech string) (sasl.Server, error)
  }
  ```
- `imapserver/capability.go:53` **automatically advertises `AUTH=<mech>`** for
  whatever `AuthenticateMechanisms()` returns. No capability plumbing needed —
  and note `advertisedCaps()` in `serve.go` must NOT be touched for this; auth
  mechanisms are a separate path.
- `github.com/emersion/go-sasl` already ships the server half in `external.go`:
  ```go
  func NewExternalServer(authenticator ExternalAuthenticator) Server
  type ExternalAuthenticator func(identity string) error
  ```
- `serve.go:222` hands the connection to the session:
  ```go
  NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
      return backend.NewSessionForNetConn(c.NetConn()), ...
  }
  ```
  so `c.NetConn().(*tls.Conn).ConnectionState().PeerCertificates` is reachable.

Nothing new needs adding to `go.mod`.

## Work items

**1. `serve.go` — TLS config.** Add `ClientAuth` + `ClientCAs`. Load the CA
bundle from a new config key (`tls_client_ca`); when unset, leave `ClientAuth`
at its zero value so behaviour is unchanged. This must be opt-in.

**2. New table.** Fingerprint → mailbox:

```sql
CREATE TABLE mailbox_certs (
    id           BIGSERIAL PRIMARY KEY,
    mailbox_id   BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    sha256       BYTEA NOT NULL UNIQUE,   -- SHA-256 of the DER cert
    note         TEXT,                    -- which device
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);
```

**Key on the fingerprint, not the CN.** A CN is issuer-controlled and can be
reissued for the same name; a fingerprint identifies one specific certificate.
An operator already using Postfix `relay_clientcerts`, which is also keyed on
certificate fingerprints, then identifies a device the same way in both
services. Revocation becomes a `DELETE` —
no CRL, no OCSP, effective immediately.

**3. Implement `SessionSASL` on `imapsess.Session`.**

```go
func (s *Session) AuthenticateMechanisms() []string {
    // PLAIN must stay first and must always be present, or every
    // password client breaks the moment this ships.
    if s.peerCert != nil { return []string{"PLAIN", "EXTERNAL"} }
    return []string{"PLAIN"}
}

func (s *Session) Authenticate(mech string) (sasl.Server, error) {
    switch mech {
    case "PLAIN":    // delegate to the existing Login path
    case "EXTERNAL": // sasl.NewExternalServer(s.authenticateByCert)
    }
}
```

Read the peer certificate **inside the authenticator, not at session
creation** — Go performs the TLS handshake lazily, so `ConnectionState()` is
not reliably populated when `NewSession` runs.

**4. Identity semantics.** SASL EXTERNAL carries an optional authzid. Start by
**ignoring it** and deriving the mailbox purely from the fingerprint: one cert,
one mailbox, nothing the client asserts is trusted. If a cert ever needs access
to several mailboxes, accept the authzid and verify the mapping allows it —
but do not build that until it is actually needed.

**5. Admin CLI.** `admin cert-add -mailbox M -file cert.pem -note "..."`,
`cert-list`, `cert-revoke -sha256 ...`. Mirrors the existing
`api-token-*` shape.

**6. Tests.** The existing suite has integration tests that speak real IMAP —
follow that idiom. Minimum: valid cert authenticates as the right mailbox;
valid cert for a *different* mailbox cannot cross over; revoked fingerprint
fails; untrusted cert fails at handshake; **no cert still authenticates with
`AUTH=PLAIN`**; `AUTH=EXTERNAL` is absent from `CAPABILITY` when no cert was
presented.

## Prerequisites — none of this is testable first

1. **epistula-imap on `:993`, publicly reachable.** The daemon runs
   unprivileged, so the deployment needs a low-port binding strategy such as
   `mac_portacl` (see `QUICKSTART-FREEBSD.md`).
2. **A CA-issued server certificate**, for example from Let's Encrypt via a
   certbot deploy-hook. This is orthogonal to client certs — you need a valid
   *server* cert either way.
3. **`production = true`.** Its startup safety checks (a verified-TLS Postgres
   DSN, no cleartext on the IMAP port, a loopback-only admin listener) should
   be in force before another authentication mechanism is added.

## Decide before building

**Client-certificate support varies.** Thunderbird and macOS Mail support it.
iOS needs a `.mobileconfig` profile that installs the identity and references
it from the IMAP payload. On Android it is limited outside K-9/Thunderbird.
Webmail would need the server, rather than the browser, to hold the
certificate.

Test the clients a deployment needs *before* writing code. The server half is
the easy half. If only some clients can present a certificate,
`VerifyClientCertIfGiven` still makes the feature worth having — those clients
use certificates and the rest keep passwords — but expect that mixed outcome.

## What this does NOT require

An existing Postfix relay-by-certificate setup needs **no changes**. This
design only brings IMAP up to the same mechanism; it does not change, extend,
or depend on anything in Postfix.
