#!/bin/sh
# Check dist/ against one public key and nothing else: the detached signature on
# checksums.txt, the manifest's hashes, and every DEB's embedded origin
# signature. RPM signatures are checked by rpm itself in package-container-test.sh.
# Usage: verify-signatures.sh PUBLIC_KEY
set -eu
if [ $# -ne 1 ]; then
    echo "usage: $0 PUBLIC_KEY" >&2
    exit 2
fi
project_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
public_key=$(CDPATH='' cd -- "$(dirname -- "$1")" && pwd)/$(basename -- "$1")
test -f "$public_key"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM

# gpgv accepts only signatures made by a key in this keyring.
GNUPGHOME=$work gpg --batch --quiet --dearmor --output "$work/keyring.gpg" "$public_key"

cd "$project_dir/dist"
gpgv --keyring "$work/keyring.gpg" checksums.txt.sig checksums.txt
if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c checksums.txt
else
    shasum -a 256 -c checksums.txt
fi

for package in *.deb; do
    test -f "$package"
    # The origin signature covers these members, concatenated in this order.
    ar p "$package" debian-binary > "$work/signed-members"
    ar p "$package" control.tar.gz >> "$work/signed-members"
    ar p "$package" data.tar.gz >> "$work/signed-members"
    ar p "$package" _gpgorigin > "$work/origin.asc"
    gpgv --keyring "$work/keyring.gpg" "$work/origin.asc" "$work/signed-members"
    printf '%s: origin signature OK\n' "$package"
done
