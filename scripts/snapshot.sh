#!/bin/sh
# Build snapshot archives and packages signed with a throwaway key, so the
# release signing path is exercised without the release key. The secret half
# stays in a temporary directory and is destroyed on exit; the public half is
# left in dist/ for verify-signatures.sh and test-packages.sh.
# Usage: snapshot.sh [GORELEASER COMMAND...]
set -eu
project_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
[ $# -gt 0 ] || set -- goreleaser

work=$(mktemp -d)
cleanup() {
    gpgconf --homedir "$work/gnupg" --kill all >/dev/null 2>&1 || true
    rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

build_umask=$(umask)
umask 077
mkdir "$work/gnupg"
GNUPGHOME=$work/gnupg
NFPM_PASSPHRASE=$(openssl rand 24 | base64 | sed 's/[/10lO#+=]//g')
RELEASE_GPG_KEY_FILE=$work/signing-key.asc
export GNUPGHOME NFPM_PASSPHRASE RELEASE_GPG_KEY_FILE

# Same algorithm and passphrase protection as the release key.
printf '%s' "$NFPM_PASSPHRASE" | gpg --batch --quiet --pinentry-mode loopback --passphrase-fd 0 \
    --quick-generate-key 'Snapshot throwaway key (not a release key) <snapshot@throwaway.invalid>' rsa4096 sign 1d
RELEASE_GPG_FINGERPRINT=$(gpg --batch --list-secret-keys --with-colons | awk -F: '$1 == "fpr" { print $10; exit }')
export RELEASE_GPG_FINGERPRINT
printf '%s' "$NFPM_PASSPHRASE" | gpg --batch --quiet --pinentry-mode loopback --passphrase-fd 0 \
    --armor --output "$RELEASE_GPG_KEY_FILE" --export-secret-keys "$RELEASE_GPG_FINGERPRINT"
umask "$build_umask"

cd "$project_dir"
"$@" release --snapshot --clean
gpg --batch --quiet --armor --output dist/snapshot-signing-key.asc --export "$RELEASE_GPG_FINGERPRINT"
sh scripts/verify-signatures.sh dist/snapshot-signing-key.asc
