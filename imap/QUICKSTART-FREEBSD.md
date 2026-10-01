# Epistula — FreeBSD Quick Start — epistula-imap

**Stage 2.** This assumes the mail store is already standing on this host —
you finished [`epistula-database`'s
QUICKSTART-FREEBSD.md](../database/QUICKSTART-FREEBSD.md):
Postgres `epistula_database` DB, schema applied, the blob store at
`/var/spool/epistula-database`, and a mailbox with a password you can log in as.

Here you stand up the **IMAP4 server**: implicit TLS on `:993`, reading folders
and message bodies from the store and accepting `APPEND` through the shared
ingest path. It owns no schema; `epistula-database` owns it.

```text
Mail.app / Thunderbird / mutt ── implicit TLS :993 ──► epistula-imap
                                                          ├─► Postgres (IMAP state: flags, UIDs, folders, quota)
                                                          └─► raw/ + att/ blobs (read + APPEND-write, shared-group access)
```

> **Co-location assumption:** runs on the same host as `epistula-database` and
> Postgres (example DSN `127.0.0.1:5432`, local blob group-read). A split-host
> deployment needs the blob tree replicated/NFS-mounted and is out of scope.

## Prerequisites

- `epistula-database` quickstart complete on this host; a mailbox provisioned.
- A TLS certificate for the IMAP hostname (e.g. Let's Encrypt
  `fullchain.pem` + `privkey.pem`). Implicit TLS means the daemon needs a real
  cert from first start.
- Root/`sudo`.

---

## 0. Variables

```sh
SVC_USER="epistula-imap"
SVC_GROUP="epistula-imap"
STORAGE_GROUP="epistula-database"                          # epistula-database's blob group
CONFIG="/usr/local/etc/epistula/epistula-imap.toml"
BIN="/usr/local/bin/epistula-imap"

PG_DB="epistula_database"
PG_ROLE="epistula_imap"
PG_PASS="$(openssl rand -base64 24 | tr -d '/+=')"

IMAP_HOST="mail.example.invalid"                       # EDIT: the cert's hostname
echo "epistula_imap PG pass: ${PG_PASS}"               # save it; goes into the config in step 4
```

Build & copy the binary as in the parent guide (`make build-freebsd` →
`${BIN}`, `chmod 755`), then `"${BIN}" version`.

---

## 1. Service account + blob-group membership

```sh
pw groupadd -n "${SVC_GROUP}" 2>/dev/null || true
pw useradd  -n "${SVC_USER}" -g "${SVC_GROUP}" -d /nonexistent \
            -s /usr/sbin/nologin -c "Epistula imap" 2>/dev/null || true

# FETCH reads epistula-database's raw/ tree, and IMAP APPEND (Sent/Drafts saves)
# WRITES new blobs into the raw/ and att/ trees — so the imap user needs the
# storage group AND the store must be group-writable.
pw groupmod "${STORAGE_GROUP}" -m "${SVC_USER}"

# The blob store must be deployed group-writable (setgid dirs 2770, files 0660)
# — epistula-database's QUICKSTART step 2 creates it 2770 and both TOMLs set
# `group_writable = true` under [storage]. Without it, APPEND fails EACCES
# (`NO [SERVERBUG] blob writer failed`). If the tree predates this, migrate once
# as the epistula-database owner: `chmod -R g+ws ${STORAGE_ROOT} && chmod -R g+w ${STORAGE_ROOT}`.
```

---

## 2. PostgreSQL — read+write role

Unlike `epistula-api` (read-plus-sidecar), the IMAP server has a real write
surface: it sets flags (STORE), allocates UIDs and mutates folders
(COPY/MOVE/CREATE/RENAME), expunges, maintains `mailboxes.used_bytes`, and runs
the **same ingest path as the LDA** on `APPEND`. Give it DML on the store; do
**not** reuse the read-only `epistula_api` role. Run as the DB owner:

```sh
su - postgres -c "psql -v ON_ERROR_STOP=1 -d ${PG_DB} <<SQL
CREATE ROLE ${PG_ROLE} LOGIN PASSWORD '${PG_PASS}';
GRANT CONNECT ON DATABASE ${PG_DB} TO ${PG_ROLE};
GRANT USAGE ON SCHEMA public TO ${PG_ROLE};
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ${PG_ROLE};
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ${PG_ROLE};
SQL"
```

This is deliberately broad — the IMAP write surface and the shared ingest path
touch most data tables (`messages`, `attachments`, `folders`, `mailboxes`,
`folder_subscriptions`, `gc_candidates`, …). Tightening to a hand-enumerated
least-privilege grant is a worthwhile follow-up, but it must track every table
the ingest/IMAP mutations write, so start broad and narrow once it's running.

---

## 3. Let the unprivileged daemon read the cert and bind `:993`

The daemon runs as `${SVC_USER}` (never root) and performs no privilege drop,
so two things need arranging:

**a) Cert readability.** The daemon reads `tls_cert`/`tls_key` at startup as
`${SVC_USER}`. Let's Encrypt keys are root-only `0600` by default. Give the
service group read access (refresh in the renewal hook, step 6):

```sh
# Example: a group-readable copy the daemon owns the group on. Adjust to your
# certbot layout; the requirement is simply that ${SVC_USER} can read both PEMs.
install -d -o root -g "${SVC_GROUP}" -m 750 /usr/local/etc/imap-tls
install -o root -g "${SVC_GROUP}" -m 640 \
    /usr/local/etc/letsencrypt/live/${IMAP_HOST}/fullchain.pem /usr/local/etc/imap-tls/fullchain.pem
install -o root -g "${SVC_GROUP}" -m 640 \
    /usr/local/etc/letsencrypt/live/${IMAP_HOST}/privkey.pem  /usr/local/etc/imap-tls/privkey.pem
```

**b) Low-port bind.** FreeBSD restricts ports < 1024 to root. Grant just this
uid the right to bind `:993` with `mac_portacl` (precise, no proxy hop):

```sh
kldload mac_portacl
sysctl security.mac.portacl.suser_exempt=1
sysctl security.mac.portacl.port_high=1023
sysctl net.inet.ip.portrange.reservedlow=0
sysctl net.inet.ip.portrange.reservedhigh=0
sysctl security.mac.portacl.rules=uid:$(id -u ${SVC_USER}):tcp:993

# Persist across reboot:
sysrc -f /boot/loader.conf mac_portacl_load=YES
cat >> /etc/sysctl.conf <<EOF
security.mac.portacl.suser_exempt=1
security.mac.portacl.port_high=1023
net.inet.ip.portrange.reservedlow=0
net.inet.ip.portrange.reservedhigh=0
security.mac.portacl.rules=uid:$(id -u ${SVC_USER}):tcp:993
EOF
```

With `port_high=1023` and the rule, `mac_portacl` denies every low-port bind
*except* root and the listed uid+port — disabling the blanket reserved-port
restriction doesn't open low ports to other users. (Alternative: bind a high
port in the config and `pf rdr` `:993 → :high`.)

---

## 4. Configuration

```sh
install -o root -g "${SVC_GROUP}" -m 640 epistula-imap.toml.example "${CONFIG}"

sed -i '' \
  -e "s#^dsn .*#dsn                  = \"postgres://${PG_ROLE}:${PG_PASS}@127.0.0.1:5432/${PG_DB}?sslmode=disable\"#" \
  -e "s#^tls_cert .*#tls_cert        = \"/usr/local/etc/imap-tls/fullchain.pem\"#" \
  -e "s#^tls_key .*#tls_key         = \"/usr/local/etc/imap-tls/privkey.pem\"#" \
  "${CONFIG}"

grep -E '^(dsn|tls_cert|tls_key|listen_addr|root)' "${CONFIG}"
```

Confirm `[storage] root` is `/var/spool/epistula-database` and set
`group_writable = true` under `[storage]` to match epistula-database (required for
APPEND to write blobs into the shared tree). Leave `production = false` and
`sslmode=disable` for the bring-up; **Going to production** flips both.

---

## 5. Validate, start, smoke-test the wire

```sh
"${BIN}" check-config -config "${CONFIG}"     # must exit 0 (also checks the cert is readable)

install -m 555 deploy/freebsd/epistula_imap /usr/local/etc/rc.d/epistula_imap
sysrc epistula_imap_enable=YES
service epistula_imap start

sockstat -4 -l | grep -E '993|8783'
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8783/healthz   # expect 200

# Greeting over implicit TLS:
openssl s_client -connect localhost:993 -servername ${IMAP_HOST} -quiet </dev/null 2>/dev/null | head -2
#   expect: * OK [CAPABILITY ... IMAP4rev2 ...]

# Full login against a provisioned mailbox:
openssl s_client -quiet -crlf -connect localhost:993 -servername ${IMAP_HOST} <<'EOF'
A1 LOGIN testuser testpassword
A2 SELECT INBOX
A3 LOGOUT
EOF
```

`A2 OK [READ-WRITE] SELECT completed` means Postgres state, blob access, and TLS
all line up.

---

## 6. Certificate renewal

The daemon swaps the cert on `SIGHUP` without dropping connections. Wire it into
the renewal so the group-readable copies refresh and the daemon reloads:

```sh
# certbot renew deploy-hook (adapt to your renewal tooling):
deploy_hook="install -o root -g ${SVC_GROUP} -m 640 \
  /usr/local/etc/letsencrypt/live/${IMAP_HOST}/fullchain.pem /usr/local/etc/imap-tls/fullchain.pem && \
  install -o root -g ${SVC_GROUP} -m 640 \
  /usr/local/etc/letsencrypt/live/${IMAP_HOST}/privkey.pem /usr/local/etc/imap-tls/privkey.pem && \
  service epistula_imap reload"
```

A failed reload keeps the current cert and logs at ERROR.

---

## Going to production

```sh
# In ${CONFIG}:
#   production = true
#   accept_insecure_for_dev = false
#   min_tls_version = "1.3"
#   [postgres] dsn = "...?sslmode=verify-full&sslrootcert=/path/to/ca.crt"
"${BIN}" check-config -config "${CONFIG}"
service epistula_imap restart
```

`production = true` requires `sslmode=verify-full` (or `verify-ca`) on the DSN,
refuses a non-loopback admin listener, and refuses cleartext on the IMAP port.
Provision the Postgres TLS cert/CA before flipping it.

---

## Troubleshooting

**`bind: permission denied` on `:993`** — `mac_portacl` step 3b didn't take.
Check `kldstat | grep portacl` and `sysctl security.mac.portacl.rules`; the uid
must match `id -u ${SVC_USER}`.

**`server.tls_cert not readable`** — `check-config` caught it: the `${SVC_USER}`
account can't read the PEMs. Re-check the group-readable copies in step 3a.

**`FETCH BODY[]` → `NO [SERVERBUG] message body unavailable`** — the daemon
can't read the blob: either not in `${STORAGE_GROUP}` (step 1) or the
per-mailbox `<mailbox>/raw` tree isn't group-readable. This logs at ERROR with
the sha256.

**Auth always fails** — confirm the mailbox exists and isn't disabled
(`epistula-database admin mailbox-list`); `AUTH=PLAIN` is refused pre-TLS, so test
over `:993`/`s_client`, never cleartext.

**`postgres.dsn must use sslmode=verify-full`** — you set `production = true`
with a dev DSN. Either provision PG TLS or keep `production = false` for the
bring-up.
