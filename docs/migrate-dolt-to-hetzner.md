# Migrating Dolt from AKS to Hetzner

This document covers moving the Dolt database (used by `bd`/beads) from the FHI AKS cluster (`tn-heimdall/dolt-beads`) to the Hetzner server that already hosts Hytte.

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [WireGuard vs SSH Tunnel](#wireguard-vs-ssh-tunnel)
3. [Prerequisites](#prerequisites)
4. [Part 1: Install Dolt on Hetzner](#part-1-install-dolt-on-hetzner)
5. [Part 2: Create the Dolt systemd Service](#part-2-create-the-dolt-systemd-service)
6. [Part 3: Initialize and Secure the Database](#part-3-initialize-and-secure-the-database)
7. [Part 4: Migrate Data from AKS](#part-4-migrate-data-from-aks)
8. [Part 5: Lock Down the Firewall](#part-5-lock-down-the-firewall)
9. [Part 6: Set Up the SSH Tunnel on Your Laptop](#part-6-set-up-the-ssh-tunnel-on-your-laptop)
10. [Part 7: Update Beads Config](#part-7-update-beads-config)
11. [Part 8: Set Up Automated Backups](#part-8-set-up-automated-backups)
12. [Part 9: Verify Everything Works](#part-9-verify-everything-works)
13. [Part 10: Decommission the AKS Dolt Pod](#part-10-decommission-the-aks-dolt-pod)
14. [Troubleshooting](#troubleshooting)
15. [Rollback Plan](#rollback-plan)

---

## Architecture Overview

### Before (current)

```
┌─────────────┐    kubectl port-forward    ┌─────────────────────┐
│  Your laptop │ ────────────────────────► │  AKS (tn-heimdall)  │
│  bd commands │    localhost:3306          │  dolt-beads pod      │
└─────────────┘                            │  port 3306           │
                                           └─────────────────────┘
```

### After (target)

```
┌─────────────┐    SSH tunnel (-L 3306)    ┌─────────────────────┐
│  Your laptop │ ────────────────────────► │  Hetzner server      │
│  bd commands │    localhost:3306          │  dolt (systemd)      │
│              │                            │  port 3306 (local)   │
│              │                            │  hytte (systemd)     │
└─────────────┘                            └─────────────────────┘
```

The key change: instead of `kubectl port-forward` tunneling to AKS, you use an **SSH tunnel** to Hetzner. From `bd`'s perspective, nothing changes — it still connects to `127.0.0.1:3306`.

---

## WireGuard vs SSH Tunnel

### What is WireGuard?

WireGuard is a VPN protocol. It creates a private encrypted network between two machines, giving them direct access to each other's services as if they were on the same LAN. It requires **installing WireGuard software on both machines** (a kernel module + userspace tool on the server, and a client app on your laptop).

### Why you can't use it

Your work laptop doesn't allow installing additional VPN software. WireGuard requires a client application (`wireguard-tools` or the WireGuard app) to be installed — so it's off the table.

### SSH tunnel: the better fit

An SSH tunnel does essentially the same thing for a single port, but **using software you already have** (the `ssh` command built into Windows). It's the exact same pattern as `kubectl port-forward` — it maps a remote port to localhost.

| | WireGuard | SSH Tunnel |
|---|-----------|-----------|
| Install required on laptop? | **Yes** (WireGuard client) | **No** (SSH is built-in) |
| Install required on server? | Yes (WireGuard) | No (sshd already running) |
| Scope | Full network access (all ports) | Single port per tunnel |
| Encryption | ChaCha20-Poly1305 | AES-256-GCM (or similar) |
| Performance | Faster (kernel-level) | Slightly slower (userspace) |
| Reconnection | Automatic | Needs autossh or similar |
| Good for your use case? | No (can't install) | **Yes (already works)** |

**Bottom line**: SSH tunnel is the right choice. It's what you're already doing conceptually with `kubectl port-forward`, just without kubectl.

---

## Prerequisites

Before starting, make sure you have:

- [ ] SSH access to the Hetzner server (`ssh robin@<hetzner-ip>`)
- [ ] sudo access on the Hetzner server
- [ ] The current `BEADS_DOLT_PASSWORD` env var value (from your PowerShell profile)
- [ ] `kubectl port-forward` to the current AKS Dolt still working (for the data migration)
- [ ] `bd` and `dolt` CLI tools available locally

---

## Part 1: Install Dolt on Hetzner

SSH into the Hetzner server:

```bash
ssh robin@<hetzner-ip>
```

Install Dolt:

```bash
# Download and install Dolt
sudo bash -c 'curl -L https://github.com/dolthub/dolt/releases/latest/download/install.sh | bash'

# Verify installation
dolt version
```

This installs the `dolt` binary to `/usr/local/bin/dolt`.

Create a dedicated directory for the beads database:

```bash
sudo mkdir -p /var/lib/dolt/forge
sudo chown robin:robin /var/lib/dolt/forge
```

---

## Part 2: Create the Dolt systemd Service

Create the service file:

```bash
sudo nano /etc/systemd/system/dolt-beads.service
```

Paste this content:

```ini
[Unit]
Description=Dolt SQL Server (beads issue tracking)
After=network.target
Wants=network-online.target

[Service]
Type=simple
User=robin
Group=robin
WorkingDirectory=/var/lib/dolt/forge
ExecStart=/usr/local/bin/dolt sql-server \
    --host 127.0.0.1 \
    --port 3306 \
    --user forge \
    --config /etc/dolt/config.yaml
Restart=always
RestartSec=5

# Security hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/dolt/forge
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

**Key details:**
- `--host 127.0.0.1` — Dolt only listens on localhost, **not** the public interface. This is critical for security. The only way to reach it is via SSH tunnel.
- `--port 3306` — Standard MySQL port. No conflict with Hytte (which uses 8080).
- `--user forge` — A dedicated Dolt user (not root).
- `ProtectSystem=strict` / `ProtectHome=yes` — systemd sandboxing. The process can only write to `/var/lib/dolt/forge`.

Create the Dolt server config:

```bash
sudo mkdir -p /etc/dolt
sudo nano /etc/dolt/config.yaml
```

```yaml
log_level: info

listener:
  host: 127.0.0.1
  port: 3306
  max_connections: 10

behavior:
  autocommit: true
```

Enable the service (but don't start it yet — we need to initialize the database first):

```bash
sudo systemctl daemon-reload
sudo systemctl enable dolt-beads
```

---

## Part 3: Initialize and Secure the Database

Initialize the Dolt database:

```bash
cd /var/lib/dolt/forge

# Initialize a new Dolt repo (this creates the database)
dolt init --name "forge-beads" --email "robin@example.com"

# Create the 'forge' database
dolt sql -q "CREATE DATABASE IF NOT EXISTS forge;"

# Create a dedicated user with a strong password
# Replace <STRONG_PASSWORD> with a real password (store it securely)
dolt sql -q "CREATE USER 'forge'@'127.0.0.1' IDENTIFIED BY '<STRONG_PASSWORD>';"
dolt sql -q "GRANT ALL PRIVILEGES ON forge.* TO 'forge'@'127.0.0.1';"
dolt sql -q "FLUSH PRIVILEGES;"
```

**Password management**: Store the password somewhere safe. You'll set it as `BEADS_DOLT_PASSWORD` on your laptop later.

> **Note**: Dolt's user management uses the same MySQL `CREATE USER` / `GRANT` syntax. The user `forge@127.0.0.1` can only connect from localhost (which includes SSH tunnel connections).

---

## Part 4: Migrate Data from AKS

You have two options for migrating the beads data. Choose one:

### Option A: Export/Import via `bd` (Recommended)

This is the simplest and safest approach since `bd` already has JSONL export built in.

**On your laptop** (with kubectl port-forward to AKS still active):

```bash
# Make sure JSONL export is up to date
cd C:\source\fhigit\Forge
bd sync
bd export  # Exports to .beads/issues.jsonl
```

The file `.beads/issues.jsonl` now contains all your beads data. This file is already in git, so it's the source of truth.

**On the Hetzner server**, after starting the Dolt service:

```bash
sudo systemctl start dolt-beads
```

**On your laptop**, switch to the SSH tunnel (see [Part 6](#part-6-set-up-the-ssh-tunnel-on-your-laptop) first), then:

```bash
# bd will auto-import from issues.jsonl into the new Dolt server
bd sync
```

### Option B: Dolt dump/restore (if you need full history)

**On your laptop** (with kubectl port-forward to AKS still active):

```bash
# Dump all tables from the current Dolt server
dolt dump --host 127.0.0.1 --port 3306 --user root --result-format sql > forge-dump.sql
```

**Copy to Hetzner:**

```bash
scp forge-dump.sql robin@<hetzner-ip>:/var/lib/dolt/forge/
```

**On Hetzner:**

```bash
sudo systemctl start dolt-beads
cd /var/lib/dolt/forge
dolt sql < forge-dump.sql
dolt add .
dolt commit -m "Import from AKS"
```

---

## Part 5: Lock Down the Firewall

Dolt is bound to `127.0.0.1`, so it's already not reachable from the internet. But let's make doubly sure with `ufw`:

```bash
# Check current firewall status
sudo ufw status

# If ufw is not enabled, enable it (MAKE SURE SSH IS ALLOWED FIRST):
sudo ufw allow OpenSSH
sudo ufw allow 80/tcp    # HTTP (for Hytte/Caddy)
sudo ufw allow 443/tcp   # HTTPS (for Hytte/Caddy)
sudo ufw enable

# Verify: port 3306 should NOT be listed as allowed
sudo ufw status verbose
```

**Expected output** — only these ports are open to the internet:

```
To                         Action      From
--                         ------      ----
OpenSSH                    ALLOW       Anywhere
80/tcp                     ALLOW       Anywhere
443/tcp                    ALLOW       Anywhere
```

Port 3306 is **not** exposed. The only way to reach Dolt is through an SSH tunnel.

---

## Part 6: Set Up the SSH Tunnel on Your Laptop

This is the replacement for `kubectl port-forward`. It maps the remote Dolt port to your localhost:3306.

### Basic command

```powershell
ssh -N -L 3306:127.0.0.1:3306 robin@<hetzner-ip>
```

**What this does:**
- `-N` — Don't open a shell, just forward the port
- `-L 3306:127.0.0.1:3306` — Forward local port 3306 to the remote server's 127.0.0.1:3306
- The connection is encrypted end-to-end via SSH

From `bd`'s perspective, this is identical to the old `kubectl port-forward` — it sees Dolt on `127.0.0.1:3306`.

### Auto-start in your PowerShell profile

Replace the `kubectl port-forward` in your PowerShell `$PROFILE` with the SSH tunnel.

**Remove/comment out the old line:**

```powershell
# OLD — remove this:
# kubectl port-forward -n tn-heimdall svc/dolt-beads 3306:3306
```

**Add the SSH tunnel instead:**

```powershell
# Start Dolt SSH tunnel in background (replaces kubectl port-forward)
function Start-DoltTunnel {
    $existingTunnel = Get-Process ssh -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -match "3306:127.0.0.1:3306" }
    
    if (-not $existingTunnel) {
        Write-Host "Starting Dolt SSH tunnel to Hetzner..." -ForegroundColor Cyan
        Start-Process ssh -ArgumentList "-N", "-L", "3306:127.0.0.1:3306", "robin@<hetzner-ip>" `
            -WindowStyle Hidden
    }
}

Start-DoltTunnel
```

> **Tip**: Make sure your SSH key is loaded in the ssh-agent so the tunnel doesn't prompt for a password. If you use `ssh-agent`:
> ```powershell
> # Add your key once (will persist across sessions if ssh-agent service is running)
> ssh-add ~/.ssh/id_ed25519
> ```

### Keeping the tunnel alive

SSH tunnels can drop on network changes (sleep, Wi-Fi switch). Two options:

**Option 1: SSH config with keepalive** (simplest)

Add to `~/.ssh/config`:

```
Host hetzner-dolt
    HostName <hetzner-ip>
    User robin
    LocalForward 3306 127.0.0.1:3306
    ServerAliveInterval 30
    ServerAliveCountMax 3
    ExitOnForwardFailure yes
```

Then your tunnel command becomes just:

```powershell
ssh -N hetzner-dolt
```

And `ServerAliveInterval 30` sends a keepalive every 30 seconds, so the tunnel doesn't go stale.

**Option 2: Auto-reconnect wrapper in your profile**

```powershell
function Start-DoltTunnel {
    $jobName = "DoltTunnel"
    $existing = Get-Job -Name $jobName -ErrorAction SilentlyContinue | 
        Where-Object { $_.State -eq 'Running' }
    
    if (-not $existing) {
        Start-Job -Name $jobName -ScriptBlock {
            while ($true) {
                ssh -N -o ServerAliveInterval=30 -o ServerAliveCountMax=3 `
                    -o ExitOnForwardFailure=yes `
                    -L 3306:127.0.0.1:3306 robin@<hetzner-ip>
                # If SSH exits, wait 5 seconds and retry
                Start-Sleep -Seconds 5
            }
        } | Out-Null
        Write-Host "Dolt tunnel started (background job: $jobName)" -ForegroundColor Cyan
    }
}
```

---

## Part 7: Update Beads Config

### Update `.beads/metadata.json` in both repos

The server connection details stay the same (localhost:3306), but update the user:

**Forge/.beads/metadata.json** — change the user from `root` to `forge`:

```json
{
  "database": "forge",
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_server_host": "127.0.0.1",
  "dolt_server_port": 3306,
  "dolt_server_user": "forge",
  "dolt_database": "forge",
  "jsonl_export": "issues.jsonl"
}
```

**Hytte/.beads/metadata.json** — same change (update user to `forge`).

### Update the `BEADS_DOLT_PASSWORD` environment variable

In your PowerShell `$PROFILE` (or however you set env vars), update the password to the one you created in Part 3:

```powershell
$env:BEADS_DOLT_PASSWORD = "<STRONG_PASSWORD>"
```

### Update `.beads/config.yaml`

No changes needed! The `dolt.auto-start: false` setting is still correct — you don't want `bd` spawning a local Dolt when the SSH tunnel drops.

### Update documentation references

In both repos' `AGENTS.md` and `CLAUDE.md`, update the connection instructions:

- Replace references to `kubectl port-forward -n tn-heimdall svc/dolt-beads 3306:3306` with the SSH tunnel command
- Update "AKS pod" references to "Hetzner server"

---

## Part 8: Set Up Automated Backups

Dolt has built-in backup support. Set up a cron job on Hetzner:

```bash
sudo nano /etc/cron.d/dolt-backup
```

```cron
# Daily Dolt backup at 3 AM
0 3 * * * robin cd /var/lib/dolt/forge && /usr/local/bin/dolt backup sync local-backup /var/backups/dolt/forge 2>&1 | logger -t dolt-backup
```

Create the backup directory:

```bash
sudo mkdir -p /var/backups/dolt/forge
sudo chown robin:robin /var/backups/dolt/forge
```

Initialize the backup remote:

```bash
cd /var/lib/dolt/forge
dolt backup add local-backup file:///var/backups/dolt/forge
```

### Optional: Off-server backup

For extra safety, sync backups to Hetzner Object Storage (S3-compatible) or a second Hetzner volume:

```bash
# If you set up Hetzner Object Storage:
# Install s3cmd or use rclone
sudo apt install rclone

# Add a weekly off-server backup cron
# 0 4 * * 0  robin rclone sync /var/backups/dolt/ hetzner-s3:dolt-backups/
```

---

## Part 9: Verify Everything Works

### On the Hetzner server

```bash
# Check Dolt service is running
sudo systemctl status dolt-beads

# Check it's listening on localhost only
sudo ss -tlnp | grep 3306
# Expected: 127.0.0.1:3306   (NOT 0.0.0.0:3306)

# Check logs
sudo journalctl -u dolt-beads -f
```

### On your laptop

```powershell
# 1. Start the SSH tunnel
ssh -N -L 3306:127.0.0.1:3306 robin@<hetzner-ip>

# 2. In another terminal, test bd
cd C:\source\fhigit\Forge
bd ready
bd list

# 3. Verify you can create and close a test issue
bd create "Test: Hetzner migration" --description "Delete after testing" -t task -p 4 --json
# Note the ID, then:
bd close <id> --reason "Migration test successful"
```

### Verify data integrity

```powershell
# Compare issue counts
bd list --json | ConvertFrom-Json | Measure-Object
# Should match the count from before migration
```

---

## Part 10: Decommission the AKS Dolt Pod

Once you've verified everything works on Hetzner for a few days:

1. **Stop the kubectl port-forward** (remove from your profile)
2. **Remove the AKS pod/service** when you're confident the migration is solid
3. **Keep the JSONL export in git** as a safety net — it's always there for disaster recovery

---

## Troubleshooting

### "Connection refused" on localhost:3306

```powershell
# Check if the SSH tunnel is running
Get-Process ssh | Where-Object { $_.CommandLine -match "3306" }

# If not, restart it:
ssh -N -L 3306:127.0.0.1:3306 robin@<hetzner-ip>
```

### "Access denied" from bd

```powershell
# Verify the password is set
echo $env:BEADS_DOLT_PASSWORD

# Test direct MySQL connection through the tunnel
dolt sql-client --host 127.0.0.1 --port 3306 --user forge --password $env:BEADS_DOLT_PASSWORD
```

### Dolt service won't start on Hetzner

```bash
# Check logs
sudo journalctl -u dolt-beads --no-pager -n 50

# Common issues:
# - Port 3306 already in use (MySQL installed?): sudo ss -tlnp | grep 3306
# - Permission denied on /var/lib/dolt/forge: ls -la /var/lib/dolt/
# - Dolt binary missing: which dolt
```

### Tunnel drops frequently

Add these to your `~/.ssh/config`:

```
Host hetzner-dolt
    ServerAliveInterval 15
    ServerAliveCountMax 5
    TCPKeepAlive yes
    Compression yes
```

### Port 3306 conflict with local MySQL

If you have MySQL installed locally, it's already using 3306. Use a different local port:

```powershell
ssh -N -L 3307:127.0.0.1:3306 robin@<hetzner-ip>
```

Then update `.beads/metadata.json` to `"dolt_server_port": 3307` (don't commit this change).

---

## Rollback Plan

If anything goes wrong, you can revert to AKS instantly:

1. **Stop the SSH tunnel**
2. **Restart kubectl port-forward**:
   ```powershell
   kubectl port-forward -n tn-heimdall svc/dolt-beads 3306:3306
   ```
3. **Revert `.beads/metadata.json`** to `"dolt_server_user": "root"`
4. **Revert `BEADS_DOLT_PASSWORD`** to the old value

The AKS pod is still running and has all the original data. No data loss possible during the migration window.

---

## Summary Checklist

- [ ] Install Dolt on Hetzner
- [ ] Create `/var/lib/dolt/forge` database directory
- [ ] Create and enable `dolt-beads.service`
- [ ] Initialize database and create `forge` user with password
- [ ] Migrate data (JSONL import or SQL dump)
- [ ] Verify firewall blocks port 3306 from internet
- [ ] Set up SSH tunnel on laptop
- [ ] Update `.beads/metadata.json` (user: `forge`)
- [ ] Update `BEADS_DOLT_PASSWORD` env var
- [ ] Set up daily backup cron
- [ ] Verify `bd ready` / `bd list` works through tunnel
- [ ] Update `AGENTS.md` / `CLAUDE.md` connection docs
- [ ] Run on both setups in parallel for a few days
- [ ] Decommission AKS Dolt pod
