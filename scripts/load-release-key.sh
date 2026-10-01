#!/bin/sh
# Release workflow only. Stage the signing key from the RELEASE_GPG_KEY secret,
# refuse to continue unless it is the key published in packaging/ and the
# passphrase unlocks it, then hand its location to the following steps.
set -eu
: "${RELEASE_GPG_KEY:?}" "${NFPM_PASSPHRASE:?}" "${RUNNER_TEMP:?}" "${GITHUB_ENV:?}"
project_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

umask 077
stage=$RUNNER_TEMP/release-signing
mkdir "$stage" "$stage/gnupg"
GNUPGHOME=$stage/gnupg
export GNUPGHOME
printf '%s\n' "$RELEASE_GPG_KEY" > "$stage/signing-key.asc"
gpg --batch --quiet --import "$stage/signing-key.asc"

fingerprint() {
    awk -F: '$1 == "fpr" { print $10; exit }'
}
published=$(gpg --batch --show-keys --with-colons "$project_dir/packaging/release-signing-key.asc" | fingerprint)
staged=$(gpg --batch --list-secret-keys --with-colons | fingerprint)
if [ -z "$published" ] || [ "$staged" != "$published" ]; then
    echo "RELEASE_GPG_KEY is not the key in packaging/release-signing-key.asc" >&2
    exit 1
fi

# A wrong passphrase must stop the release here, not after the build.
printf '%s' "$NFPM_PASSPHRASE" | gpg --batch --quiet --pinentry-mode loopback --passphrase-fd 0 \
    --local-user "$published" --output "$stage/probe.sig" --detach-sign "$stage/signing-key.asc"
rm -f "$stage/probe.sig"

{
    printf 'GNUPGHOME=%s\n' "$GNUPGHOME"
    printf 'RELEASE_GPG_KEY_FILE=%s\n' "$stage/signing-key.asc"
    printf 'RELEASE_GPG_FINGERPRINT=%s\n' "$published"
} >> "$GITHUB_ENV"
