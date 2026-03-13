#!/bin/sh
set -e

# Create forge system user if it doesn't exist
# Use portable check (id -u) and prefer adduser (Alpine) over useradd (glibc distros)
if ! id -u forge >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --user-group --home-dir /var/lib/forge --shell /sbin/nologin forge
    else
        # Alpine/BusyBox: adduser is available by default
        adduser -S -D -H -h /var/lib/forge -s /sbin/nologin forge
    fi
fi

# Reload systemd, then enable and start the service
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    systemctl enable --now forge.service
fi
