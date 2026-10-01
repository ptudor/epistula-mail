#!/bin/sh
# Create Epistula's service identity; retain it on removal.
set -eu
service=epistula-api
getent group epistula >/dev/null 2>&1 || groupadd --system epistula
getent group "$service" >/dev/null 2>&1 || groupadd --system "$service"
if ! getent passwd "$service" >/dev/null 2>&1; then
    useradd --system --gid "$service" --home-dir "/var/lib/$service" --no-create-home --shell /usr/sbin/nologin "$service"
fi
usermod --append --groups epistula "$service"
install -d -m 0750 -o "$service" -g "$service" "/var/lib/$service"
if [ ! -e /var/spool/epistula-database ]; then
    install -d -m 2770 -o root -g epistula /var/spool/epistula-database
fi
