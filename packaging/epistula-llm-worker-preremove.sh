#!/bin/sh
# Stop on removal, retaining configuration, identities, and mail data.
set -eu
case "${1:-}" in
    remove|deconfigure|0)
        if command -v systemctl >/dev/null 2>&1; then
            systemctl stop epistula-llm-worker.service >/dev/null 2>&1 || true
            systemctl disable epistula-llm-worker.service >/dev/null 2>&1 || true
        fi
        ;;
esac
