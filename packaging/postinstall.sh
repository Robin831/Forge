#!/bin/sh
set -e

# Create forge system user if it doesn't exist
if ! getent passwd forge >/dev/null 2>&1; then
    useradd --system --user-group --home-dir /var/lib/forge --shell /usr/sbin/nologin forge
fi

# Reload systemd to pick up the new service file
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
fi
