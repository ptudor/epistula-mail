# Epistula Linux packages

Epistula builds separate DEB and RPM packages for its five executables on Linux
amd64 and arm64. Install only the components a host needs. PostgreSQL, Postfix,
TLS certificates, and a model server are configured separately.

Packages create dedicated service accounts with names matching their executables.
Database, IMAP, and API accounts also join the shared `epistula` storage group.
Configuration is installed as `/etc/epistula/<executable>.toml`, owned by root
and the component's private group with mode 0640. Package upgrades preserve
edited configuration. No package enables or starts a service automatically.

The shared blob root is `/var/spool/epistula-database`. Packages create it with
setgid mode 2770 and group `epistula` when it is absent. Existing mail ownership
is not rewritten. Database and IMAP configurations use `group_writable = true`;
API reads the same root. The blob root must be mounted consistently on all three
components' hosts. Service accounts and state survive package removal.

The database service runs `serve` for health and metrics. Configure Postfix to
invoke `epistula-database deliver` under the database account for delivery.
The worker's service runs on the model host. MCP uses a loopback HTTP listener
in its packaged unit; client-launched stdio use needs only its executable and
the scoped API credentials.

Configure the installed TOML files, PostgreSQL roles and migrations, and IMAP TLS
paths before enabling a daemon. Empty database DSNs and tokens require operator
configuration. Follow the component deployment guides linked from the README.
Use the service account when checking configuration, so permission errors are
visible before startup.

```sh
sudo -u epistula-database epistula-database check-config -config /etc/epistula/epistula-database.toml
sudo -u epistula-database epistula-database migrate up -config /etc/epistula/epistula-database.toml
sudo systemctl enable --now epistula-database.service
```

Enable each additional component after its configuration and credentials are
ready. Units constrain filesystem access and run under dedicated identities.
IMAP retains the capability required to bind port 993; other units have no
ambient capabilities. The API unit mounts the shared blob root read-only.

RPMs have embedded OpenPGP signatures. DEBs have embedded origin signatures;
APT does not check those automatically. Verify the signed checksum manifest
before installing either format, following [releases.md](releases.md).

`make test-packages` exercises installation, signature checks, edited configuration
retention, storage-group permissions, and removal in disposable Debian and Fedora
containers. It does not provide live mail-delivery acceptance.
