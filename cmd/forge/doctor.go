package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"text/tabwriter"

	"github.com/Robin831/Forge/internal/autostart"
	"github.com/Robin831/Forge/internal/daemon"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(doctorCmd)
}

// checkResult is a single health check result.
type checkResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"`  // "ok", "warn", "fail"
	Detail  string `json:"detail"`
}

var doctorCmd = &cobra.Command{
	Use:     "doctor",
	Short:   "Run health checks on Forge installation",
	GroupID: "daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		var checks []checkResult

		// 1. Check bd (beads) installed
		checks = append(checks, checkBinary("bd", "beads issue tracker"))

		// 2. Check gh (GitHub CLI) installed and authenticated
		checks = append(checks, checkGitHub())

		// 3. Check claude installed
		checks = append(checks, checkBinary("claude", "Claude CLI"))

		// 4. Check state.db accessible
		checks = append(checks, checkStateDB())

		// 5. Check daemon running
		checks = append(checks, checkDaemon())

		// 6. Check IPC socket
		checks = append(checks, checkIPC())

		// 7. Check forge dir
		checks = append(checks, checkForgeDir())

		// 8. Check anvils configured
		checks = append(checks, checkAnvils())

		// 9. Check govulncheck (optional — needed for vulnerability scanning)
		checks = append(checks, checkGovulncheck())

		// 10. Check autostart registration (Windows only)
		if runtime.GOOS == "windows" {
			checks = append(checks, checkAutostart())
		}

		if jsonOutput {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(checks)
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "CHECK\tSTATUS\tDETAIL\n")

		okCount, warnCount, failCount := 0, 0, 0
		for _, c := range checks {
			icon := "✓"
			switch c.Status {
			case "warn":
				icon = "⚠"
				warnCount++
			case "fail":
				icon = "✗"
				failCount++
			default:
				okCount++
			}
			fmt.Fprintf(tw, "%s %s\t%s\t%s\n", icon, c.Name, c.Status, c.Detail)
		}
		tw.Flush()

		fmt.Printf("\n%d ok, %d warnings, %d failures\n", okCount, warnCount, failCount)

		if failCount > 0 {
			return fmt.Errorf("%d health checks failed", failCount)
		}
		return nil
	},
}

func checkBinary(name, description string) checkResult {
	path, err := exec.LookPath(name)
	if err != nil {
		return checkResult{
			Name:   description,
			Status: "fail",
			Detail: fmt.Sprintf("%s not found in PATH", name),
		}
	}
	return checkResult{
		Name:   description,
		Status: "ok",
		Detail: path,
	}
}

func checkGitHub() checkResult {
	// Check gh exists
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return checkResult{
			Name:   "GitHub CLI",
			Status: "fail",
			Detail: "gh not found in PATH",
		}
	}

	// Check gh auth status
	cmd := exec.Command(ghPath, "auth", "status")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return checkResult{
			Name:   "GitHub CLI",
			Status: "warn",
			Detail: "gh installed but not authenticated: " + string(output),
		}
	}
	return checkResult{
		Name:   "GitHub CLI",
		Status: "ok",
		Detail: "authenticated",
	}
}

func checkStateDB() checkResult {
	db, err := state.Open("")
	if err != nil {
		return checkResult{
			Name:   "State database",
			Status: "fail",
			Detail: fmt.Sprintf("cannot open: %v", err),
		}
	}
	defer db.Close()

	// Quick query to verify it works
	_, err = db.RecentEvents(1)
	if err != nil {
		return checkResult{
			Name:   "State database",
			Status: "warn",
			Detail: fmt.Sprintf("open but query failed: %v", err),
		}
	}

	return checkResult{
		Name:   "State database",
		Status: "ok",
		Detail: db.Path(),
	}
}

func checkDaemon() checkResult {
	pid, running := daemon.IsRunning()
	if !running {
		return checkResult{
			Name:   "Daemon",
			Status: "warn",
			Detail: "not running (use 'forge up' to start)",
		}
	}
	return checkResult{
		Name:   "Daemon",
		Status: "ok",
		Detail: fmt.Sprintf("running (PID %d)", pid),
	}
}

func checkIPC() checkResult {
	if !ipc.SocketExists() {
		return checkResult{
			Name:   "IPC socket",
			Status: "warn",
			Detail: "not available (daemon may not be running)",
		}
	}
	return checkResult{
		Name:   "IPC socket",
		Status: "ok",
		Detail: socketDescription(),
	}
}

func socketDescription() string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\forge`
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".forge", "forge.sock")
}

func checkForgeDir() checkResult {
	home, _ := os.UserHomeDir()
	forgeDir := filepath.Join(home, ".forge")
	info, err := os.Stat(forgeDir)
	if err != nil {
		return checkResult{
			Name:   "Forge directory",
			Status: "fail",
			Detail: fmt.Sprintf("%s does not exist", forgeDir),
		}
	}
	if !info.IsDir() {
		return checkResult{
			Name:   "Forge directory",
			Status: "fail",
			Detail: fmt.Sprintf("%s is not a directory", forgeDir),
		}
	}
	return checkResult{
		Name:   "Forge directory",
		Status: "ok",
		Detail: forgeDir,
	}
}

func checkAnvils() checkResult {
	if cfg == nil {
		return checkResult{
			Name:   "Anvils configured",
			Status: "warn",
			Detail: "no config loaded (run 'forge anvil add' first)",
		}
	}
	count := len(cfg.Anvils)
	if count == 0 {
		return checkResult{
			Name:   "Anvils configured",
			Status: "warn",
			Detail: "no anvils registered",
		}
	}

	// Verify each anvil path exists
	missing := 0
	for name, a := range cfg.Anvils {
		if _, err := os.Stat(a.Path); err != nil {
			missing++
			_ = name // used in loop
		}
	}

	if missing > 0 {
		return checkResult{
			Name:   "Anvils configured",
			Status: "warn",
			Detail: fmt.Sprintf("%d anvils, %d with invalid paths", count, missing),
		}
	}

	return checkResult{
		Name:   "Anvils configured",
		Status: "ok",
		Detail: fmt.Sprintf("%d anvils registered", count),
	}
}

func checkGovulncheck() checkResult {
	path, err := exec.LookPath("govulncheck")
	if err != nil {
		return checkResult{
			Name:   "govulncheck",
			Status: "warn",
			Detail: "not installed — vulnerability scanning disabled. Install with: go install golang.org/x/vuln/cmd/govulncheck@latest",
		}
	}
	return checkResult{
		Name:   "govulncheck",
		Status: "ok",
		Detail: path,
	}
}

func checkAutostart() checkResult {
	registered, nextRun, err := autostart.Status()
	if err != nil {
		return checkResult{
			Name:   "Autostart",
			Status: "warn",
			Detail: fmt.Sprintf("check failed: %v", err),
		}
	}
	if !registered {
		return checkResult{
			Name:   "Autostart",
			Status: "warn",
			Detail: "not configured (run 'forge autostart install')",
		}
	}
	detail := "registered"
	if nextRun != "" {
		detail += " (next: " + nextRun + ")"
	}
	return checkResult{
		Name:   "Autostart",
		Status: "ok",
		Detail: detail,
	}
}
