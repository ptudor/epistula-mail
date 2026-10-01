# Epistula delivery through Postfix

Epistula stores messages received by Postfix through a pipe transport. Configure
PostgreSQL, shared blob storage, the `epistula-database` account, and the database
TOML file before configuring Postfix. Run configuration checks as the delivery
account, then apply the schema:

```sh
epistula-database check-config -config /etc/epistula/epistula-database.toml
epistula-database migrate up -config /etc/epistula/epistula-database.toml
```

FreeBSD uses `/usr/local/etc/epistula/epistula-database.toml` instead. Execute the
following commands under an identity authorized to write the database and blobs.

## Provision routing

Create a domain, mailbox, and alias. The mailbox password is supplied interactively
or through `-password-stdin`; choose it before exposing IMAP access.

```sh
epistula-database admin domain-add -name example.invalid
epistula-database admin mailbox-add -name alice -quota-bytes 10737418240
epistula-database admin alias-add -domain example.invalid -localpart alice -mailbox alice
```

Use your configured domain instead of the reserved example name. Optional catchall
and reject rules are managed by the database administrator commands. Confirm
delivery directly before enabling the Postfix route:

```sh
printf 'From: sender@example.invalid\r\nTo: alice@example.invalid\r\nSubject: Epistula delivery check\r\n\r\nbody\r\n' \
  | epistula-database deliver -config /etc/epistula/epistula-database.toml \
      -recipient alice@example.invalid -sender sender@example.invalid
epistula-database admin log-tail -mailbox alice
```

The delivery command must exit zero after durable storage; the delivery log records
the outcome. A temporary failure uses a sysexits code that tells Postfix to retry.

## Configure the pipe transport

Add the supplied [master.cf fragment](master.cf) to Postfix's `master.cf`, adjusting
the executable, configuration path, and delivery identity to the installation.
Linux packages install the executable under `/usr/bin`; the FreeBSD fragment uses
`/usr/local/bin`. Postfix pipe delivery needs the same storage access as the writer.

Set the per-transport recipient limit to one:

```sh
sudo postconf -e 'epistula-database_destination_recipient_limit = 1'
```

The supplied transport sets `flags=D` to prepend `Delivered-To:`. Postfix requires
one recipient per invocation for that flag; the LDA also stores one recipient's
message per invocation. Without this setting, multi-recipient deliveries defer.

Configure a transport lookup map using [transport.example](transport.example),
with a route for each accepted address or domain. Point `transport_maps` at that
map using the lookup type appropriate to your Postfix installation. Rebuild the
map with `postmap` when its source changes. Configure Postfix's recipient/domain
acceptance policy alongside Epistula's routing tables; a transport map alone
does not define which mail Postfix should accept.

Reload Postfix after checking its configuration:

```sh
sudo postfix check
sudo postfix reload
sudo postconf -M epistula-database/unix
sudo postconf epistula-database_destination_recipient_limit
```

Send a test message through Postfix and verify the Postfix queue, delivery log,
and retrieval through IMAP or the API. The database `serve` process exposes health
and metrics; it is separate from the per-message delivery process.
