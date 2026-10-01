# Epistula — Deployment artifacts for epistula-imap

One long-running surface: the IMAP4 server itself (implicit TLS on :993) plus a loopback observability listener.

| Mode    | Lifecycle    | Surface                                               |
|---------|--------------|-------------------------------------------------------|
| `serve` | long-running | `:993` IMAP4 over TLS + `127.0.0.1:8783` metrics/health |

Delivery, imports, blob garbage collection, and administration live in
[epistula-database](../../database/deploy/README.md). IMAP owns folder and
message-state mutations, archive sorting, and optional Trash retention.
`APPEND` adds message content through the shared ingest package.

## Files

- `freebsd/epistula_imap` — rc.d script for the `serve` daemon. Copy to `/usr/local/etc/rc.d/epistula_imap`, `chmod 555`, then `sysrc epistula_imap_enable=YES` and `service epistula_imap start`. The script runs `check-config` as `start_precmd`.
- `apache/epistula-imap-include.conf` — optional reverse-proxy Include for `/metrics` + `/health` on a public-facing vhost. The IMAP protocol itself is NOT reverse-proxied; clients connect directly to :993.

## What's NOT in `deploy/`

- Linux systemd units and package scripts live in the repository's `packaging/`
  directory; see [the Linux package guide](../../docs/linux-packages.md).
- The TOML config. See `../epistula-imap.toml.example` at the project root.
- The schema. epistula-database owns it; IMAP uses it for reads and mailbox mutations.

## TLS Certificates

The daemon binds with implicit TLS. Cert + key paths come from the TOML (`[server].tls_cert` / `tls_key`), NOT the rc.conf knobs.

**Cert reload on SIGHUP is implemented.** The daemon serves the certificate through an atomic holder: SIGHUP re-reads the keypair from disk and swaps it in. New handshakes get the renewed cert; established connections finish on the old one — nothing is dropped. A failed reload keeps the current certificate and logs at ERROR. The Let's Encrypt renewal hook is simply:

```sh
deploy_hook="service epistula_imap reload"   # sends SIGHUP
```

## Port :993

The rc.d script runs the daemon as `epistula_imap_user`. If you bind public `:993`, make sure that service user is allowed to bind the low port, or put a small TCP/TLS proxy or packet redirect in front of a high local port. The daemon does not perform an internal root-to-user privilege drop, so the deployment wrapper must not start it as root just to acquire the port.

## Smoke test after install

```sh
service epistula_imap start
curl -s http://127.0.0.1:8783/healthz   # expect 200
openssl s_client -connect localhost:993 -servername imap.example.invalid -quiet < /dev/null \
    | head -2                            # expect "* OK ... IMAP4rev2 ..."
```

Then exercise auth from a known mailbox (provision via `epistula-database admin mailbox-add` first):

```sh
openssl s_client -quiet -crlf -connect localhost:993 -servername imap.example.invalid <<'EOF'
A1 LOGIN testuser testpassword
A2 SELECT INBOX
A3 LOGOUT
EOF
```

### Authentication cost policy

Every admitted LOGIN performs the same ordered set of supported stored Argon2 cost classes. It verifies the supplied password against the target hash in that class and uses dummies for the other classes. Unknown, disabled and malformed accounts pay the same complete work; dummy matches are ignored. The catalog and target use a consistent database snapshot and are refreshed each login. The existing minimum-duration setting pads lookup noise and early failures; it is not the mixed-cost defense.

The daemon reserves the largest class's actual scratch-memory cost for the entire sequential set under the existing global budget. Startup logs only cost parameters/lengths and refuses a class it cannot admit. At most 32 classes and 256 distinct supported historical encodings are admitted; excessive catalogs refuse authentication uniformly. A live-added class above the budget causes the same busy response for every account. Raise the budget appropriately or reset outlier passwords with `epistula-database admin mailbox-passwd -name NAME` under the chosen current Argon2 configuration. Password resets converge historical classes; existing hashes are never automatically rewritten. All classes' work must fit the configured pre-authentication time allowance; canceled sessions stop before the next KDF.

A deployment using one cost class pays one KDF per login. Mixed deployments pay the sum of their distinct class costs and read the bounded cost catalog each login, an explicit CPU/query tradeoff for uniform behavior. The isolated `TestAuthenticationMixedCostTCPDistributions` fixture can be run on the deployment host with MAIL_DATABASE_TEST_PG and MAIL_VERIFY_AUTH_SAMPLES=20; it reports randomized repeated failure distributions alongside deterministic work/admission tests.
