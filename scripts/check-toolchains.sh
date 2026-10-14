#!/bin/sh
# Keep local auto-selected toolchains aligned with CI and release binaries.
set -eu
cd "$(dirname "$0")/.."
expected="go$(cat .go-version)"
for module in database imap api llm-worker mcp; do
    actual=$(awk '$1 == "toolchain" { print $2 }' "$module/go.mod")
    if [ "$actual" != "$expected" ]; then
        echo "$module/go.mod: toolchain $actual differs from $expected (.go-version)" >&2
        exit 1
    fi
done
