# Epistula's 1.0 baseline

Epistula begins from a complete, reviewed implementation snapshot, with fresh
Git history. The introductory sequence adds each imported file in its own
commit. Commit subjects explain the file's responsibility; bodies identify its
entry points, test contracts, or document sections. These are introductions to
the 1.0 implementation, rather than a reconstruction of its development.

The imported snapshot was taken from source commit
`795798f4d5bfaa126e3d8e62cb4a9b590967a4ee`. Only tracked files were copied;
local configuration, build output, development metadata, and the source Git
object database were excluded. The original repository remains the development
archive for the period before Epistula's public baseline.

The introduction commits collectively assemble the baseline. Intermediate
commits in that sequence can reference files introduced later and are not
individual buildable releases. Use the complete baseline or a subsequent release
tag for builds. Changes after the introductory sequence use normal focused
commits describing what changed, why, and how it was verified.

Publication adaptations shorten the module directories to `database`, `imap`,
`api`, `llm-worker`, and `mcp`; point module imports at
`github.com/ptudor/epistula-mail/<component>`; and name executables and configuration
examples for Epistula. Existing SQL migrations, API routes, IMAP behavior,
environment variable names, and Prometheus metric names carry forward.

`CLAUDE.md`, component design notes, and current component roadmaps describe the
implementation. Regression-test names retain finding identifiers where they
help identify the contract under test. Historical reviews, deployment-specific
checks, and private migration instructions are not part of this repository or
its history. Current operational instructions begin in [README.md](../README.md)
and the component deployment guides.
