#!/bin/sh
# Create Epistula's service identity; retain it on removal.
set -eu
service=epistula-llm-worker
getent group epistula >/dev/null 2>&1 || groupadd --system epistula
getent group "$service" >/dev/null 2>&1 || groupadd --system "$service"
if ! getent passwd "$service" >/dev/null 2>&1; then
    useradd --system --gid "$service" --home-dir "/var/lib/$service" --no-create-home --shell /usr/sbin/nologin "$service"
fi
install -d -m 0750 -o "$service" -g "$service" "/var/lib/$service"
