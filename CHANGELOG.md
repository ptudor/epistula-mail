# Epistula changelog

## Unreleased

- epistula-api: add `GET /v1/messages/{id}/attachments/{part}`, which streams one
  attachment's decoded bytes to a `read_content` token in the message's mailbox
  scope. The response is a non-rendering download (`Content-Disposition:
  attachment`, `nosniff`, a sandboxing CSP), and the blob's size is checked
  against the database before any header is sent, as for `/raw`. The service
  user now also reads the `<mailbox>/att` blob tree, which the storage group
  already covers.

## 1.0.0 — 2026-10-01

- Establish Epistula's public repository from the current five-component baseline,
  with a separate explanatory introduction commit for each source file.
- Provide PostgreSQL mail delivery, immutable message and attachment blobs,
  administrator commands, Maildir import, verification, recovery, and garbage collection.
- Provide IMAP access over implicit TLS, scoped HTTP search and export, optional
  model annotations and archive classification, and an MCP connector.
- Define the PostgreSQL schema, environment variable names, and Prometheus
  metric names as stable 1.0 contracts.
- Add contributor and security guidance, integration CI, third-party notices,
  signed release archives and Linux packages, and signature verification.
