#!/bin/sh
# Preserve operator configuration; never start or enable a daemon.
set -eu
service=epistula-database
config=/etc/epistula/$service.toml
chown "root:$service" "$config"
chmod 0640 "$config"
# Postfix pipe(8) clears supplementary groups. The delivery uid must own
# the packaged spool; IMAP continues to write via its shared group.
# Never follow an operator's replacement symlink or adopt another owner's tree.
spool=/var/spool/epistula-database
if [ -d "$spool" ] && [ ! -L "$spool" ] && [ "$(stat -c %U "$spool")" = root ]; then
    foreign=$(find "$spool" -mindepth 1 -maxdepth 1 ! -user epistula-database ! -user epistula-imap -print -quit)
    if [ -z "$foreign" ]; then
        chown epistula-database:epistula "$spool"
        chmod 2770 "$spool"
    fi
fi
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi
