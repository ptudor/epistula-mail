# Epistula security

Report suspected vulnerabilities privately through the repository's Security →
Report a vulnerability page when enabled. If that option is unavailable, email
ptudor@ptudor.net with “Epistula security” in the subject. Include the affected
version or commit, platform, reproduction, impact, and any verified workaround.
Redact credentials, message bodies, private keys, and unrelated operator data.

Security fixes target the latest release and main. Older releases have no
guaranteed backport schedule or response time.

Epistula releases use the same OpenPGP release key as the companion daemons:
`8C4F 58EF B945 4902 267D 9B3E 426C 4AA2 1A40 0728`.
The public key is [packaging/release-signing-key.asc](packaging/release-signing-key.asc).
The release workflow signs DEBs, RPMs, and the SHA-256 manifest, verifies
signatures, and generates GitHub build attestations before publication.
[docs/releases.md](docs/releases.md) describes verification and setup.

CI retains the complete module advisory report and gates on vulnerabilities in
imported packages and reachable code, for Linux, FreeBSD, and macOS. This
distinguishes unused packages in a module from code shipped in Epistula. CI also scans source
history. Tests, signatures, and build provenance
provide different evidence; they do not establish independent protocol certification.
