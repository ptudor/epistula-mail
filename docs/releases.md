# Epistula releases

Epistula releases are published on GitHub. Repository tags use `vMAJOR.MINOR.PATCH`
and build all five components together. Release notes are checked in at
`docs/release-notes/<tag>.md`; the 1.0 notes are
[v1.0.0.md](release-notes/v1.0.0.md).

Each Linux, FreeBSD, and macOS archive for amd64 or arm64 contains all five
executables and the configuration/deployment documentation. Linux also has five
separate packages in each architecture and DEB/RPM format. `checksums.txt` covers
the distributable artifacts; `checksums.txt.sig` authenticates that manifest.
The tagged workflow adds GitHub build provenance attestations.

## Maintainer release setup

The release workflow signs with the published release key, which it reads from
two repository Actions secrets:

| Secret | Contents |
| --- | --- |
| `RELEASE_GPG_KEY` | ASCII-armored RSA secret key matching the checked-in public key |
| `RELEASE_GPG_PASSPHRASE` | Passphrase that unlocks that key |

The expected fingerprint is `8C4F 58EF B945 4902 267D 9B3E 426C 4AA2 1A40 0728`.
The key loader uses an isolated temporary GPG home, requires an exact match with
the published key, and proves it can sign before building. Secret keys are not
stored in this repository. GitHub's automatic token publishes release assets;
a separate personal token is unnecessary.

Enable private vulnerability reporting in the repository settings if desired.
Check in the release notes, update the changelog, and let CI pass on `main`,
then create and push the release tag on the intended commit. CI and security
checks gate the release. The workflow builds a draft, signs packages and the
checksum manifest, verifies them, tests package lifecycles, attests assets, and
finally publishes the draft. A failed verification leaves it unpublished. Do not
treat an unverified draft as a release.

## Verify a download

Obtain [the published public key](../packaging/release-signing-key.asc) through a
trusted checkout and independently confirm its fingerprint. Import it into an
isolated keyring and verify the checksum signature before checking downloaded files:

```sh
gpg --show-keys --with-fingerprint packaging/release-signing-key.asc
gpg --import packaging/release-signing-key.asc
gpg --verify checksums.txt.sig checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

On macOS use `shasum -a 256 -c checksums.txt` with the complete set of artifacts,
or check the selected archive's hash against the authenticated manifest.
Then unpack the archive or install the verified package. RPM signatures can also
be checked with `rpm --import` followed by `rpm --checksig`; DEB origin signatures
are checked by [scripts/verify-signatures.sh](../scripts/verify-signatures.sh).

GitHub attestations provide an additional check on the source repository and
workflow that produced a file:

```sh
gh attestation verify checksums.txt --repo ptudor/epistula-mail --signer-workflow ptudor/epistula-mail/.github/workflows/release.yml
```

## Development snapshots

Install GoReleaser OSS 2.18.1 and GPG, then run `make release-check` and
`make snapshot`. Snapshots generate a temporary RSA signing key and delete its
secret material on exit. Its public half is written to
`dist/snapshot-signing-key.asc` for signature and package checks. Development
snapshots are not authenticated by the production release key.

`make update-notices` builds every release target and regenerates the dependency
notices from executable metadata. `make check-notices` checks the checked-in
notices against those binaries. The only permitted local dependency replacement
is Epistula's own sibling database module, covered by the project MIT license.
