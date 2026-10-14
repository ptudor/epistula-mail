#!/bin/sh
# Executed only inside the disposable containers in test-packages.sh.
set -eu
export DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq
    apt-get install -y --no-install-recommends ca-certificates systemd passwd procps util-linux
    set -- /packages/*_amd64.deb
    test -f "$1"
    # apt ignores a DEB's embedded signature; verify-signatures.sh checks it on the host.
    apt-get install -y --no-install-recommends "$@"
    format=deb
else
    rpm --import /keys/signing-key.asc
    for package in /packages/*.rpm; do
        test -f "$package"
        # An unsigned package also exits 0, reporting only "digests OK".
        result=$(rpm -K "$package")
        printf '%s\n' "$result"
        case $result in
            *'signatures OK') ;;
            *) echo "No verified signature on $package" >&2; exit 1 ;;
        esac
    done
    set -- /packages/*.x86_64.rpm
    test -f "$1"
    dnf install -y --setopt=install_weak_deps=False --setopt=localpkg_gpgcheck=1 "$@" procps-ng util-linux
    format=rpm
fi

services='epistula-database epistula-imap epistula-api epistula-llm-worker epistula-mcp'
for service in $services; do
    getent passwd "$service"
    test -x "/usr/bin/$service"
    test -f "/usr/lib/systemd/system/$service.service"
    grep -qx 'MIT License' "/usr/share/doc/$service/copyright"
    # Debian slim deliberately excludes other /usr/share/doc files.
    grep -q '^## Go runtime and standard library' "/usr/share/doc/$service/copyright"
    grep -q '^## github.com/' "/usr/share/doc/$service/copyright"
    # The package must not create an enablement symlink or start a process.
    test ! -e "/etc/systemd/system/multi-user.target.wants/$service.service"
    # Linux comm names stop at 15 bytes.
    if pgrep -x "$(printf '%.15s' "$service")"; then
        echo "Package unexpectedly started $service" >&2
        exit 1
    fi
    systemd-analyze verify "/usr/lib/systemd/system/$service.service"
    config="/etc/epistula/$service.toml"
    test "$(stat -c %a "$config")" = 640
    test "$(stat -c %U "$config")" = root
    test "$(stat -c %G "$config")" = "$service"
    runuser -u "$service" -- test -r "$config"
    printf '\n# package-upgrade-sentinel\n' >> "$config"
    printf 'preserve state\n' > "/var/lib/$service/package-test-sentinel"
done

for service in epistula-database epistula-imap epistula-api epistula-llm-worker; do
    "$service" version | grep "^$service "
done
epistula-mcp -version | grep '^epistula-mcp '
test "$(stat -c %a /var/spool/epistula-database)" = 2770
test "$(stat -c %G /var/spool/epistula-database)" = epistula
test "$(stat -c %U /var/spool/epistula-database)" = epistula-database
# Match pipe(8), which discards the shared supplementary storage group.
setpriv --reuid=epistula-database --regid=epistula-database --clear-groups -- \
    sh -c 'mkdir /var/spool/epistula-database/.probe && rmdir /var/spool/epistula-database/.probe'
for service in epistula-database epistula-imap epistula-api; do
    runuser -u "$service" -- test -r /var/spool/epistula-database
done
for service in epistula-database epistula-imap; do
    runuser -u "$service" -- test -w /var/spool/epistula-database
done

# Exercise durable delivery with pipe(8)'s exact identity in this disposable
# container. Keep the packaged operator configuration unchanged.
if [ "$format" = deb ]; then
    apt-get install -y --no-install-recommends postgresql
    for bindir in /usr/lib/postgresql/*/bin; do
        PATH="$bindir:$PATH"
    done
    export PATH
else
    dnf install -y --setopt=install_weak_deps=False postgresql-server
fi
install -d -m 0700 -o postgres -g postgres /tmp/epistula-package-pg
runuser -u postgres -- initdb -D /tmp/epistula-package-pg/data -A trust
runuser -u postgres -- pg_ctl -D /tmp/epistula-package-pg/data \
    -l /tmp/epistula-package-pg/server.log -o '-h 127.0.0.1 -p 55432' -w start
trap 'runuser -u postgres -- pg_ctl -D /tmp/epistula-package-pg/data -m immediate -w stop' EXIT
cat > /tmp/epistula-package-deliver.toml <<'EOF'
production = false
[postgres]
dsn = "postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable"
[storage]
root = "/var/spool/epistula-database"
group_writable = true
EOF
chown root:epistula-database /tmp/epistula-package-deliver.toml
chmod 0640 /tmp/epistula-package-deliver.toml
pipe_database() {
    setpriv --reuid=epistula-database --regid=epistula-database --clear-groups -- \
        epistula-database "$@" -config /tmp/epistula-package-deliver.toml
}
pipe_database migrate up
pipe_database admin domain-add -name example.invalid
printf 'package-test-password\n' | pipe_database admin mailbox-add -name package-test -password-stdin
pipe_database admin alias-add -domain example.invalid -localpart package-test -mailbox package-test
printf 'From: sender@example.invalid\r\nTo: package-test@example.invalid\r\nSubject: Package test\r\n\r\nbody\r\n' \
    | pipe_database deliver -recipient package-test@example.invalid -sender sender@example.invalid
test -n "$(find /var/spool/epistula-database/package-test/raw -name '*.eml' -print -quit)"
runuser -u postgres -- pg_ctl -D /tmp/epistula-package-pg/data -m fast -w stop
trap - EXIT

# Reinstall through the package manager with a locally edited configuration.
# conffile retention uses the same package-manager mechanism on version upgrades.
if [ "$format" = deb ]; then
    apt-get install -y --reinstall -o Dpkg::Options::=--force-confold "$@"
else
    dnf reinstall -y --setopt=localpkg_gpgcheck=1 "$@"
fi
for service in $services; do
    config="/etc/epistula/$service.toml"
    grep -q '^# package-upgrade-sentinel$' "$config"
    test -f "/var/lib/$service/package-test-sentinel"
    test ! -e "/etc/systemd/system/multi-user.target.wants/$service.service"
    if pgrep -x "$(printf '%.15s' "$service")"; then
        echo "Package reinstall unexpectedly started $service" >&2
        exit 1
    fi
done
if [ "$format" = deb ]; then
    # Deliberate word splitting: this is a fixed list of package names.
    # shellcheck disable=SC2086
    apt-get remove -y $services
else
    # shellcheck disable=SC2086
    dnf remove -y $services
fi
for service in $services; do
    test ! -e "/usr/bin/$service"
    test -f "/var/lib/$service/package-test-sentinel"
    getent passwd "$service" >/dev/null
done
printf 'Package install, configuration retention, and removal passed (%s).\n' "$format"
