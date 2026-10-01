#!/bin/sh
# Preserve operator configuration; never start or enable a daemon.
set -eu
service=epistula-mcp
config=/etc/epistula/$service.toml
chown "root:$service" "$config"
chmod 0640 "$config"
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi
