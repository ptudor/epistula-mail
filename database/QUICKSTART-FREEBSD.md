# Epistula — FreeBSD Quick Start — epistula-database

This takes a bare FreeBSD host to a **verified mail store** that can accept a
message: binary installed, Postgres role + database created, schema applied,
the `serve` daemon healthy, one domain + mailbox provisioned, and a delivery
smoke-tested through the `deliver` LDA **before Postfix is involved**.

After the store is verified, configure Postfix using the new-installation
guide in [`deploy/postfix/INSTALL.md`](deploy/postfix/INSTALL.md).

> **Scope:** this provisions only the `epistula-database` Postgres role (the LDA
> owns the schema). The read-only `epistula-api` role and the `epistula-imap`
> reader role are provisioned separately when you stand those daemons up; see
> their own deploy docs.

## Prerequisites

- FreeBSD 13+ or 14+
- Root (or `sudo`) on the host
- Postfix installed for the final delivery-integration step. PostgreSQL is
  installed below.

---

## 0. Set variables once — every later block reuses them

Paste this into a **root** shell and keep that shell open for the rest of the
guide. The Postgres and mailbox passwords are generated for you; the echoed
block is the only time the Postgres password is printed in the clear (it is
written straight into the config in step 4).

```sh
# --- Service identity (must match deploy/freebsd/epistula_database + master.cf) ---
SVC_USER="epistula-database"                              # unix account daemon + LDA run as
SVC_GROUP="epistula-database"                             # shared group; epistula-imap joins later
STORAGE_ROOT="/var/spool/epistula-database"               # per-mailbox <name>/raw + <name>/att blob trees live here
CONFIG="/usr/local/etc/epistula/epistula-database.toml"
BIN="/usr/local/bin/epistula-database"

# --- PostgreSQL (role + db use underscores — NOT the hyphenated unix user) ---
PG_DB="epistula_database"
PG_ROLE="epistula_database"
PG_PASS="$(openssl rand -base64 24 | tr -d '/+=')"

# --- First domain + mailbox to provision (EDIT THESE) ---
MAIL_DOMAIN="mail.example.invalid"
MAIL_BOX="alice"                                     # mailbox name = on-disk blob tenant: lower-case [a-z0-9._-], 1-64 chars, starts alphanumeric
MBOX_PASS="$(openssl rand -base64 18 | tr -d '/+=')"  # >= 12 chars (admin floor)

echo "================ SAVE THESE ================"
echo "Postgres role : ${PG_ROLE}"
echo "Postgres db   : ${PG_DB}"
echo "Postgres pass : ${PG_PASS}"
echo "Mailbox       : ${MAIL_BOX}@${MAIL_DOMAIN}"
echo "Mailbox pass  : ${MBOX_PASS}"
echo "==========================================="
```

> The unix account is `epistula-database` (hyphen — that's the rc.d / `pipe(8)`
> default), while the Postgres role and database are `epistula_database`
> (underscore — the example DSN's identifiers). That split is intentional;
> keep it.

---

## 1. Build & install the binary

The binary is fully static — no runtime packages needed. Build on your dev box
and copy it over:

```sh
# On the build host, from the database/ directory:
make build-freebsd          # produces build/epistula-database-freebsd-amd64
# Copy it to the server as ${BIN} (your transfer of choice), then on the server:
chmod 755 "${BIN}"
"${BIN}" version
```

---

## 2. Service account, group, directories

```sh
pw groupadd -n "${SVC_GROUP}" 2>/dev/null || true
pw useradd  -n "${SVC_USER}" -g "${SVC_GROUP}" -d /nonexistent \
            -s /usr/sbin/nologin -c "Epistula database LDA" 2>/dev/null || true

# Blob store root: owned by the daemon, SETGID + group-writable (2770) so the
# epistula-imap account (added to ${SVC_GROUP} later) can both read the blobs
# AND write them on IMAP APPEND — Sent/Drafts saves go through the same blob
# store. The setgid bit makes every subtree created on demand inherit the
# shared group. Only the root is created here — blobs are partitioned per
# mailbox (${STORAGE_ROOT}/<mailbox>/{raw,att,tmp}/...) and those subtrees are
# created on demand at first delivery. (Tip: for per-user isolation you can
# later make each ${STORAGE_ROOT}/<mailbox> its own ZFS dataset.)
#
# You MUST set `group_writable = true` under [storage] in BOTH the
# epistula-database and epistula-imap TOMLs so the daemons create the setgid
# group-writable modes (owner-only 0750/0640 is the default). If you deploy
# single-daemon (no IMAP), leave it false.
install -d -o "${SVC_USER}" -g "${SVC_GROUP}" -m 2770 "${STORAGE_ROOT}"
# Config dir (root-owned, group-readable so the daemon can read the TOML).
install -d -o root -g "${SVC_GROUP}" -m 750 /usr/local/etc/epistula

# rc.d serve daemon's log file.
install -o "${SVC_USER}" -g "${SVC_GROUP}" -m 640 /dev/null /var/log/epistula_database.log
```

---

## 3. PostgreSQL — install, role, database

```sh
pkg install -y postgresql16-server postgresql16-client

sysrc postgresql_enable=YES
/usr/local/etc/rc.d/postgresql initdb 2>/dev/null || true
service postgresql start

# Create the owning role + database with the generated password.
su - postgres -c "psql -v ON_ERROR_STOP=1 -c \"CREATE ROLE ${PG_ROLE} LOGIN PASSWORD '${PG_PASS}';\""
su - postgres -c "createdb -O ${PG_ROLE} ${PG_DB}"

# Confirm the role can connect (password auth over loopback).
psql "postgres://${PG_ROLE}:${PG_PASS}@127.0.0.1:5432/${PG_DB}?sslmode=disable" -c "SELECT 1;"
```

---

## 4. Configuration

Copy the example and stamp in the DSN + storage root. Mode `640`, group
`${SVC_GROUP}`, so only the daemon's group can read the embedded password.

```sh
# Run from the project source tree (where epistula-database.toml.example lives):
install -o root -g "${SVC_GROUP}" -m 640 epistula-database.toml.example "${CONFIG}"

sed -i '' \
  -e "s#^dsn = .*#dsn = \"postgres://${PG_ROLE}:${PG_PASS}@127.0.0.1:5432/${PG_DB}?sslmode=disable\"#" \
  -e "s#^root = .*#root = \"${STORAGE_ROOT}\"#" \
  "${CONFIG}"

# Sanity-check the two lines you just rewrote:
grep -E '^(dsn|root) =' "${CONFIG}"
```

> This leaves `production = false` and `sslmode=disable` — correct for getting
> first mail flowing over loopback. See **Going to production** at the bottom
> before this carries real traffic. Strict mode requires verified TLS for TCP
> database connections; a local Unix socket is also supported.

---

## 5. Apply the schema

The migrations are embedded in the binary and read the DSN from `${CONFIG}`:

```sh
"${BIN}" migrate up -config "${CONFIG}"       # applies the embedded migrations
"${BIN}" migrate status -config "${CONFIG}"   # every migration should read "applied"
```

(Equivalent fresh-bootstrap alternative: `psql -U ${PG_ROLE} -d ${PG_DB} -f schema.sql`.)

---

## 6. Validate config, start `serve`, check health

`serve` exposes Prometheus + health on loopback. It is **not** the delivery
path — Postfix execs `deliver` directly — but it's the rc.d-managed daemon and
its health endpoint is the quickest "is everything wired?" check.

```sh
"${BIN}" check-config -config "${CONFIG}"     # must exit 0

install -m 555 deploy/freebsd/epistula_database /usr/local/etc/rc.d/epistula_database
sysrc epistula_database_enable=YES
service epistula_database start

sockstat -4 -l | grep 8782
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8782/healthz   # expect 200
```

---

## 7. Provision routing (one domain + mailbox)

```sh
"${BIN}" admin domain-add  -name "${MAIL_DOMAIN}"
printf '%s' "${MBOX_PASS}" | "${BIN}" admin mailbox-add -name "${MAIL_BOX}" \
    -quota-bytes 10737418240 -password-stdin
"${BIN}" admin alias-add   -domain "${MAIL_DOMAIN}" -localpart "${MAIL_BOX}" -mailbox "${MAIL_BOX}"

# Optional knobs:
#   "${BIN}" admin alias-add -domain "${MAIL_DOMAIN}" -mailbox "${MAIL_BOX}" -catchall
#   "${BIN}" admin acl-add   -domain "${MAIL_DOMAIN}" -localpart spam-bait -deny -note "honeypot"

"${BIN}" admin domain-list
"${BIN}" admin mailbox-list
```

---

## 8. Smoke-test delivery — *before Postfix*

Pipe a fixture straight into `deliver`, running as `${SVC_USER}` so the blobs
land with the same ownership the Postfix pipe (`user=epistula-database`) will use.
This isolates the store + routing from Postfix entirely — if this works, the
only thing left is teaching Postfix to route here.

```sh
printf 'From: smoke@example.invalid\r\nTo: %s@%s\r\nSubject: quickstart smoke\r\n\r\nhello\r\n' \
    "${MAIL_BOX}" "${MAIL_DOMAIN}" \
  | su -m "${SVC_USER}" -c "${BIN} deliver -config=${CONFIG} -recipient=${MAIL_BOX}@${MAIL_DOMAIN} -sender=smoke@example.invalid"
echo "exit=$?"     # 0 = delivered; non-zero meanings are in exitcodes.go

"${BIN}" admin log-tail -mailbox "${MAIL_BOX}"   # expect outcome=delivered
```

A clean `exit=0` and an `outcome=delivered` line means the store is ready for
real mail.

---

## 9. Hand off to Postfix

The store is verified. Follow [`deploy/postfix/INSTALL.md`](deploy/postfix/INSTALL.md)
to add the pipe transport, set its single-recipient limit, and configure the
served domains and transport map. Validate Postfix's configuration before
reloading, then deliver a test message through SMTP and inspect `admin log-tail`.

---

## Going to production

Once delivery is proven, harden the config before it carries real traffic:

```sh
# In ${CONFIG}:
#   production = true
#   [postgres] dsn = "...?sslmode=verify-full&sslrootcert=/path/to/ca.crt"
"${BIN}" check-config -config "${CONFIG}"   # strict mode rejects weak sslmode / argon2 params
service epistula_database restart
```

Strict mode (`production = true`) requires `sslmode=verify-full` for TCP
connections and stronger Argon2id parameters. Provision the PostgreSQL TLS
certificate and CA first, or use a local Unix socket with appropriate role
authentication and socket permissions.

---

## Useful commands

```sh
# Service
service epistula_database start | stop | restart | status
tail -f /var/log/epistula_database.log

# Health / metrics (loopback)
curl -s http://127.0.0.1:8782/healthz
curl -s http://127.0.0.1:8782/metrics | head

# Operator CLI
"${BIN}" admin mailbox-list
"${BIN}" admin log-tail -mailbox "${MAIL_BOX}" -since 5m
"${BIN}" admin mailbox-passwd -name "${MAIL_BOX}" -password-stdin
"${BIN}" migrate status

# Blob GC (operator-initiated; never automatic)
"${BIN}" gc -phase mark
"${BIN}" gc -phase sweep
```

## Troubleshooting

**`serve` won't start** — `check-config` surfaces the reason; the rc.d prestart
runs it and refuses to start on failure:
```sh
"${BIN}" check-config -config "${CONFIG}"
```

**Postgres connection refused / auth failed** — confirm the DSN the daemon sees
and that the role connects:
```sh
grep '^dsn =' "${CONFIG}"
psql "postgres://${PG_ROLE}:${PG_PASS}@127.0.0.1:5432/${PG_DB}?sslmode=disable" -c "SELECT 1;"
```

**`deliver` exits non-zero** — the code maps to a `sysexits.h` value (see
`exitcodes.go`); `EX_TEMPFAIL` (75) tells Postfix to retry. Check ownership of
`${STORAGE_ROOT}` (must be `${SVC_USER}`) and that the role can write the DB.

**Blob permission errors after a root-run test** — if you ran `deliver` as root
by mistake, fix ownership:
```sh
chown -R "${SVC_USER}:${SVC_GROUP}" "${STORAGE_ROOT}"
```
