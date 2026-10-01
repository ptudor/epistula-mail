# epistula-imap — limitations and future work

## End-to-end protocol test

Several tests speak IMAP over TCP to the real session code, and one runs the
`serve` process in cleartext development mode. No test drives the complete
production path — the implicit-TLS listener, LOGIN, SELECT, FETCH and LOGOUT
through `serve` — or uses an external client such as `openssl s_client` or
mutt. Such a test would catch protocol-shape bugs (literal framing, response
ordering) that the narrower tests cannot.

## BINARY (RFC 3516)

The server returns a clean `NO` for BINARY fetches. Serving decoded sections
from the stored MIME parts is not implemented; most clients fetch with
`BODY[...]` instead.

## CONDSTORE / QRESYNC wire surface

Not advertised. The schema and mutation groundwork is in place: STORE, EXPUNGE,
COPY and MOVE bump `folders.highest_modseq` and stamp rows, and `SEARCH MODSEQ N`
filters on them. Advertising the extensions requires the go-imap server library
to parse and write the full RFC 7162 surface (FETCH MODSEQ, STATUS
HIGHESTMODSEQ, STORE UNCHANGEDSINCE, vanished-UID tracking); re-evaluate on each
library upgrade.

## STARTTLS capability set

With implicit TLS there is no pre-TLS state, so no code is needed. A
STARTTLS-fronted deployment would need the pre-TLS capability set to exclude
`AUTH=PLAIN`.

## Operator interface

This daemon has no administration surface. Mailbox, domain and alias
management is `epistula-database admin`; a web operator dashboard is not
included. Folder CREATE, DELETE and RENAME are IMAP protocol operations and are
implemented here.

## Deployment notes

- The rc.d script (`deploy/freebsd/epistula_imap`) has daemon(8) record the Go
  process's pid (`daemon -p`), so `service epistula_imap stop` triggers the
  graceful drain and `service epistula_imap reload` the certificate swap.
  `drain_timeout_seconds` (default 30) bounds the drain.
- Binding a public `:993` needs an explicit low-port strategy (an OS
  capability such as `mac_portacl`, a packet redirect, or a front proxy). The
  daemon never runs as root and performs no privilege drop.
