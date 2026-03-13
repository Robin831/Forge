# Remote Access via VS Code Remote Tunnels

Access the Forge Hearth TUI and Claude Code CLI from any browser using [VS Code Remote Tunnels](https://code.visualstudio.com/docs/remote/tunnels). This avoids running an SSH server — tunnels use Microsoft's relay infrastructure, require no open ports, and are typically policy-compliant on corporate laptops.

## How It Works

```
┌─────────────────────────────────────┐
│  Your Machine (Host)                │
│                                     │
│  forge up          ← daemon         │
│  code tunnel       ← VS Code relay  │
│                                     │
└──────────┬──────────────────────────┘
           │ Microsoft relay (no open ports)
           ▼
┌─────────────────────────────────────┐
│  Any Browser / VS Code Client       │
│                                     │
│  vscode.dev → terminal → forge hearth│
│  claude --cli  (headless mode)      │
│                                     │
└─────────────────────────────────────┘
```

1. **Host** runs `code tunnel` as a background service alongside Forge.
2. **Client** opens [vscode.dev](https://vscode.dev), connects to the tunnel, and opens a terminal.
3. Run `forge hearth` in the terminal for the full TUI experience.
4. Claude Code CLI also supports remote/headless mode over the tunnel.

## Setup

### 1. Install VS Code CLI (if not already)

The `code` CLI is typically installed with VS Code. Verify:

```powershell
code --version
```

If not available, download the [VS Code CLI](https://code.visualstudio.com/docs/editor/command-line) standalone binary.

### 2. Authenticate the Tunnel (one-time)

```powershell
code tunnel --accept-server-license-terms
```

This prompts you to sign in with your GitHub or Microsoft account. Follow the device code flow in your browser.

### 3. Start the Tunnel

```powershell
# Start in the foreground (for testing)
code tunnel --name forge-host

# Or as a Windows service (persistent)
code tunnel service install
```

The `--name` flag gives your machine a friendly identifier visible in vscode.dev.

### 4. Connect from a Browser

1. Open [vscode.dev](https://vscode.dev)
2. Press `F1` → **Remote Tunnels: Connect to Tunnel...**
3. Select your machine name (`forge-host`)
4. Open a terminal (`Ctrl+``)
5. Run `forge hearth`

## Running Alongside Forge

### Option A: Windows Task Scheduler (Recommended)

Create a second scheduled task alongside ForgeAutoStart:

```powershell
# Register the tunnel as a Windows service
code tunnel service install
```

This registers `code tunnel` as a Windows service that starts at logon, similar to `forge autostart install`.

### Option B: PowerShell Background Job

Add to your PowerShell profile or a startup script:

```powershell
# Start tunnel in background
Start-Job -Name "CodeTunnel" -ScriptBlock { code tunnel --name forge-host }

# Start Forge daemon
forge up
```

### Option C: Combined Startup Script

Create a script that starts both:

```powershell
# start-forge-remote.ps1
Write-Host "Starting VS Code tunnel..."
$tunnel = Start-Process -FilePath "code" -ArgumentList "tunnel","--name","forge-host" -PassThru -WindowStyle Hidden

Write-Host "Starting Forge daemon..."
forge up

# Tunnel runs until manually stopped or system shutdown
```

## Claude Code Remote Sessions

Claude Code CLI can run in headless/remote mode through the tunnel:

1. Connect to your tunnel from vscode.dev
2. Open a terminal
3. Run `claude` — the CLI works normally in the remote terminal
4. Claude has access to all host files and tools

This enables remote AI-assisted development sessions that execute on the host machine while you work from any browser.

## Troubleshooting

### Tunnel won't start
```powershell
# Check if another tunnel is already running
code tunnel status

# Kill existing tunnel and restart
code tunnel kill
code tunnel --name forge-host
```

### Connection drops
The tunnel automatically reconnects. If it doesn't:
```powershell
code tunnel service restart    # If installed as service
# Or manually restart:
code tunnel kill
code tunnel --name forge-host
```

### Hearth rendering issues in vscode.dev
The vscode.dev browser terminal supports full ANSI rendering. If the TUI looks wrong:
- Ensure your browser window is wide enough (minimum ~120 columns)
- Try a different browser (Chrome/Edge work best)
- Check the terminal font — monospace fonts are required for proper alignment

### Corporate policy blocks tunnels
VS Code tunnels use `*.tunnels.api.visualstudio.com` and `*.devtunnels.ms`. If these are blocked by your network policy, you'll need to request an exception from IT. The traffic is TLS-encrypted and routed through Microsoft infrastructure.

## Security Considerations

- **Authentication**: Tunnels require GitHub/Microsoft sign-in. Only your authenticated account can connect.
- **No open ports**: Unlike SSH, tunnels don't listen on any local port. All traffic is outbound to Microsoft's relay.
- **Encryption**: All tunnel traffic is TLS-encrypted end-to-end.
- **Access scope**: The remote session has the same permissions as the user running `code tunnel` on the host.
