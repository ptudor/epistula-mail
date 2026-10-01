# Contributing to Epistula

Describe a reproducible problem, the affected commit or version, platform,
expected behavior, and redacted logs. Report vulnerabilities privately as
described in [SECURITY.md](SECURITY.md).

Use the Go version in [.go-version](.go-version). Build and test the complete
checkout with `make build`, `make test`, `make vet`, and `make verify-modules`.
The five components are separate Go modules. IMAP and API import the database
module through a sibling `replace`; build matching components from one checkout.

Run `make test-integration` with `MAIL_DATABASE_TEST_PG` set to a disposable
PostgreSQL bootstrap URL and a role allowed to create databases. Each test creates
an isolated database and registers its cleanup. The regular test suite skips
database integration when the variable is unset.

Explain the trigger, resulting behavior, compatibility impact, and validation
in each pull request. Keep protocol and storage contracts explicit. Schema changes
belong in numbered, embedded database migrations; maintain schema.sql parity.
Preserve original message bytes and the durability ordering of blobs before rows.
Include a regression test for changes to those contracts.

Release changes should pass `make release-check`, `make snapshot`, and
`make check-notices`. Signed package lifecycle checks run on Linux amd64 with
Docker via `make test-packages`. Snapshots use a temporary signing key.
After changing dependencies or the Go toolchain, run `make update-notices` and
review `THIRD_PARTY_NOTICES.md`. If the Go license is outside GOROOT, set
`GO_LICENSE=/path/to/go/LICENSE`.

Contributions use the [MIT license](LICENSE). Preserve third-party notices.
The file introduction commits document the 1.0 snapshot; new work should use
focused behavioral commits, as explained in [docs/baseline.md](docs/baseline.md).
