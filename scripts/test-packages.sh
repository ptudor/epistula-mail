#!/bin/sh
# Run on a Linux amd64 host with Docker. Containers receive no clock/device capabilities.
# Usage: test-packages.sh [PUBLIC_KEY]   (default: the key of the last make snapshot)
set -eu
project_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
public_key=${1:-$project_dir/dist/snapshot-signing-key.asc}
test -f "$public_key"
public_key=$(CDPATH='' cd -- "$(dirname -- "$public_key")" && pwd)/$(basename -- "$public_key")
for distro in debian:13-slim fedora:43; do
    docker run --rm \
        -v "$project_dir/dist:/packages:ro" \
        -v "$project_dir/scripts:/checks:ro" \
        -v "$public_key:/keys/signing-key.asc:ro" \
        "$distro" sh /checks/package-container-test.sh
done
