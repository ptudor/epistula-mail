# Epistula database — limitations and future work

## Parser corpus coverage

The ingest tests cover synthetic malformed headers, NUL bytes, mislabeled
charsets, and nested forwarded messages. Additional anonymized fixtures for
calendar invitations, S/MIME, PGP/MIME, and unusual real-world MIME structures
would improve coverage. Never commit private mail as a test fixture.

## Cross-binary delivery acceptance

Delivery exit codes, shared ingestion, and IMAP FETCH have individual
integration coverage. A single harness that runs the delivery executable,
connects through IMAP, and compares `BODY[]` byte-for-byte with the submitted
fixture remains useful future work. It should use an isolated test database and
blob root rather than a deployed mailbox.

## Maildir tree import

`import` handles one Maildir and target folder at a time. Maildir++ trees need
one invocation per folder. An `import-tree` command could map `.Sent` to `Sent`
and `.Archive.2003.Q1` to `Archive/2003/Q1` in one operation; it is not implemented.

## Operator interface

The supported administration interface is `epistula-database admin`. A web
operator dashboard is not included. Provisioning, monitoring, and coordinated
database/blob backups remain operator responsibilities; see the deployment
guides and root README.

## IMAP roadmap

The shared schema has modification-sequence columns, but full QRESYNC
vanished-UID tracking is not implemented. This requires an explicit vanished-UID
log and matching IMAP protocol work, not merely a schema setting.
