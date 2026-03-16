package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Robin831/Forge/internal/autostart"
	"github.com/Robin831/Forge/internal/changelog"
	"github.com/Robin831/Forge/internal/daemon"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/spf13/cobra"
)

// commandTimeout is the maximum time allowed for external CLI checks in doctor.
const commandTimeout = 5 * time.Second

// execLookPath and execRunCommand are package-level variables so tests can
// inject mock implementations without relying on real binaries in PATH.
var (
	execLookPath   = exec.LookPath
	execRunCommand = func(name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
)

func init() {
	rootCmd.AddCommand(doctorCmd)
}

// checkResult is a single health check result.
type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "ok", "warn", "fail"
	Detail string `json:"detail"`
}

var doctorCmd = &cobra.Command{
	Use:     "doctor",
	Short:   "Run health checks on Forge installation",
	GroupID: "daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		var checks []checkResult

		// 1. Check git installed
		checks = append(checks, checkGit())

		// 2. Check bd (beads) installed
		checks = append(checks, checkBinary("bd", "beads issue tracker"))

		// 3. Check VCS CLI tools — platform-aware based on configured anvils
		checks = append(checks, checkVCSTools()...)

		// 4. Check claude installed
		checks = append(checks, checkBinary("claude", "Claude CLI"))

		// 5. Check provider chain CLIs and auth
		checks = append(checks, checkProviderChain()...)

		// 6. Check state.db accessible
		checks = append(checks, checkStateDB())

		// 7. Check daemon running
		checks = append(checks, checkDaemon())

		// 8. Check IPC socket
		checks = append(checks, checkIPC())

		// 9. Check forge dir
		checks = append(checks, checkForgeDir())

		// 10. Check anvils configured
		checks = append(checks, checkAnvils())

		// 11. Check govulncheck (optional — needed for vulnerability scanning)
		checks = append(checks, checkGovulncheck())

		// 12. Check Dolt/beads database connectivity
		checks = append(checks, checkDoltConnectivity())

		// 13. Check depcheck tooling for configured anvils
		checks = append(checks, checkDepcheckTooling()...)

		// 14. Check changelog fragment validity
		checks = append(checks, checkChangelogFragments())

		// 15. Check autostart registration (Windows only)
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

func checkGit() checkResult {
	gitPath, err := execLookPath("git")
	if err != nil {
		return checkResult{
			Name:   "Git",
			Status: "fail",
			Detail: "git not found in PATH — required for worktree operations",
		}
	}
	// Get version for extra detail.
	out, err := execRunCommand(gitPath, "--version")
	if err != nil {
		return checkResult{
			Name:   "Git",
			Status: "ok",
			Detail: gitPath,
		}
	}
	version := strings.TrimSpace(string(out))
	return checkResult{
		Name:   "Git",
		Status: "ok",
		Detail: version,
	}
}

// checkProviderChain verifies that every provider in the configured chain has
// its CLI binary available in PATH and that basic authentication is in place.
func checkProviderChain() []checkResult {
	if cfg == nil {
		return []checkResult{{
			Name:   "Provider chain",
			Status: "warn",
			Detail: "no config loaded — provider chain checks skipped",
		}}
	}

	specs := cfg.Settings.SmithProviders
	if len(specs) == 0 {
		specs = cfg.Settings.Providers
	}
	providers := provider.FromConfig(specs)

	if len(providers) == 0 {
		return []checkResult{{
			Name:   "Provider chain",
			Status: "warn",
			Detail: "no providers configured — set smith_providers in forge.yaml",
		}}
	}

	var results []checkResult
	for _, p := range providers {
		results = append(results, checkProviderCLI(p))
		results = append(results, checkProviderAuth(p))
	}
	return results
}

// checkProviderCLI verifies that a provider's CLI binary is available in PATH.
func checkProviderCLI(p provider.Provider) checkResult {
	name := "Provider CLI (" + p.Label() + ")"
	bin := p.Cmd()
	path, err := execLookPath(bin)
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "fail",
			Detail: fmt.Sprintf("%s not found in PATH", bin),
		}
	}
	return checkResult{
		Name:   name,
		Status: "ok",
		Detail: path,
	}
}

// checkProviderAuth verifies authentication for a provider.
// Each provider kind has different auth mechanisms:
//   - claude: OAuth-based login stored locally
//   - gemini: GOOGLE_API_KEY or gcloud auth
//   - copilot: GitHub auth via gh CLI
//   - openai: OPENAI_API_KEY environment variable
func checkProviderAuth(p provider.Provider) checkResult {
	name := "Provider auth (" + p.Label() + ")"

	// If the provider has a known backend with injected env vars (e.g. ollama),
	// auth is handled by those env vars — skip external checks.
	if p.Backend != "" {
		return checkResult{
			Name:   name,
			Status: "ok",
			Detail: fmt.Sprintf("backend %q provides auth via environment", p.Backend),
		}
	}

	switch p.Kind {
	case provider.Claude:
		return checkClaudeAuth(name)
	case provider.Gemini:
		return checkGeminiAuth(name)
	case provider.Copilot:
		return checkCopilotAuth(name)
	case provider.OpenAI:
		return checkOpenAIAuth(name)
	default:
		return checkResult{
			Name:   name,
			Status: "warn",
			Detail: fmt.Sprintf("unknown provider kind %q — cannot verify auth", p.Kind),
		}
	}
}

func checkClaudeAuth(name string) checkResult {
	// Claude CLI uses OAuth login. Check if ANTHROPIC_API_KEY is set (API
	// key mode) or if the CLI has a stored session (OAuth mode). We can't
	// easily verify OAuth tokens, but the API key env var is straightforward.
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return checkResult{
			Name:   name,
			Status: "ok",
			Detail: "ANTHROPIC_API_KEY is set",
		}
	}
	// ANTHROPIC_API_KEY is not set. `claude --version` only confirms the
	// binary is present — it does NOT verify an OAuth session is active.
	// Return warn so users aren't misled into thinking auth is confirmed.
	bin, err := execLookPath("claude")
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "warn",
			Detail: "claude not in PATH — cannot verify auth",
		}
	}
	out, err := execRunCommand(bin, "--version")
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "warn",
			Detail: "claude CLI error: " + strings.TrimSpace(string(out)),
		}
	}
	return checkResult{
		Name:   name,
		Status: "warn",
		Detail: "ANTHROPIC_API_KEY not set — cannot verify OAuth session (run 'claude auth login' if needed)",
	}
}

func checkGeminiAuth(name string) checkResult {
	// Gemini CLI typically uses GOOGLE_API_KEY or GEMINI_API_KEY.
	if os.Getenv("GOOGLE_API_KEY") != "" {
		return checkResult{
			Name:   name,
			Status: "ok",
			Detail: "GOOGLE_API_KEY is set",
		}
	}
	if os.Getenv("GEMINI_API_KEY") != "" {
		return checkResult{
			Name:   name,
			Status: "ok",
			Detail: "GEMINI_API_KEY is set",
		}
	}
	return checkResult{
		Name:   name,
		Status: "warn",
		Detail: "neither GOOGLE_API_KEY nor GEMINI_API_KEY is set — gemini CLI may require authentication",
	}
}

func checkCopilotAuth(name string) checkResult {
	// Copilot CLI relies on GitHub authentication.
	ghPath, err := execLookPath("gh")
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "warn",
			Detail: "gh not in PATH — copilot requires GitHub auth via gh CLI",
		}
	}
	out, err := execRunCommand(ghPath, "auth", "status")
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "warn",
			Detail: "gh not authenticated — copilot requires GitHub auth: " + strings.TrimSpace(string(out)),
		}
	}
	return checkResult{
		Name:   name,
		Status: "ok",
		Detail: "GitHub auth active (via gh CLI)",
	}
}

func checkOpenAIAuth(name string) checkResult {
	if os.Getenv("OPENAI_API_KEY") != "" {
		return checkResult{
			Name:   name,
			Status: "ok",
			Detail: "OPENAI_API_KEY is set",
		}
	}
	return checkResult{
		Name:   name,
		Status: "warn",
		Detail: "OPENAI_API_KEY is not set — OpenAI provider requires authentication",
	}
}

func checkBinary(name, description string) checkResult {
	path, err := execLookPath(name)
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

// anyAnvilUsesPlatform reports whether any configured anvil uses the given
// VCS platform. An empty platform string on an anvil defaults to GitHub.
func anyAnvilUsesPlatform(p vcs.Platform) bool {
	if cfg == nil {
		// No config loaded — assume GitHub (the default).
		return p == vcs.GitHub
	}
	for _, anvil := range cfg.Anvils {
		resolved, err := vcs.ParsePlatform(anvil.Platform)
		if err != nil {
			continue
		}
		if resolved == p {
			return true
		}
	}
	return false
}

// invalidPlatformAnvils returns a map of anvil name → raw platform string
// for any anvils whose platform value cannot be parsed.
func invalidPlatformAnvils() map[string]string {
	invalid := make(map[string]string)
	if cfg == nil {
		return invalid
	}
	for name, anvil := range cfg.Anvils {
		if anvil.Platform == "" {
			continue // empty defaults to GitHub — not an error
		}
		if _, err := vcs.ParsePlatform(anvil.Platform); err != nil {
			invalid[name] = anvil.Platform
		}
	}
	return invalid
}

// checkVCSTools returns platform-aware checks for VCS CLI tools.
// Only tools required by the configured anvil platforms are checked.
func checkVCSTools() []checkResult {
	var results []checkResult

	// Warn about any anvils with unrecognised platform values.
	for name, raw := range invalidPlatformAnvils() {
		results = append(results, checkResult{
			Name:   "VCS platform config",
			Status: "warn",
			Detail: fmt.Sprintf("anvil %q has unknown platform %q — check forge.yaml", name, raw),
		})
	}

	if anyAnvilUsesPlatform(vcs.GitHub) {
		results = append(results, checkGitHub())
	} else {
		// Collect configured platform names for context.
		platforms := configuredPlatforms()
		detail := "not required — no GitHub anvils configured"
		if len(platforms) > 0 {
			detail += " (using " + strings.Join(platforms, ", ") + ")"
		}
		results = append(results, checkResult{
			Name:   "GitHub CLI",
			Status: "ok",
			Detail: detail,
		})
	}

	return results
}

// configuredPlatforms returns deduplicated platform names from config.
func configuredPlatforms() []string {
	if cfg == nil {
		return nil
	}
	seen := make(map[string]bool)
	var platforms []string
	for _, anvil := range cfg.Anvils {
		p, err := vcs.ParsePlatform(anvil.Platform)
		if err != nil {
			continue
		}
		name := string(p)
		if !seen[name] {
			seen[name] = true
			platforms = append(platforms, name)
		}
	}
	sort.Strings(platforms)
	return platforms
}

func checkGitHub() checkResult {
	// Check gh exists
	ghPath, err := execLookPath("gh")
	if err != nil {
		return checkResult{
			Name:   "GitHub CLI",
			Status: "fail",
			Detail: "gh not found in PATH",
		}
	}

	// Check gh auth status
	output, err := execRunCommand(ghPath, "auth", "status")
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
	path, err := execLookPath("govulncheck")
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

// checkDoltConnectivity verifies that the beads database is reachable by
// running a lightweight bd list command with a short timeout.
func checkDoltConnectivity() checkResult {
	bdPath, err := execLookPath("bd")
	if err != nil {
		return checkResult{
			Name:   "Beads DB connectivity",
			Status: "warn",
			Detail: "bd not in PATH — cannot verify database connectivity",
		}
	}
	out, err := execRunCommand(bdPath, "list", "--limit", "1", "--json")
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return checkResult{
			Name:   "Beads DB connectivity",
			Status: "fail",
			Detail: "cannot reach beads database: " + detail,
		}
	}
	return checkResult{
		Name:   "Beads DB connectivity",
		Status: "ok",
		Detail: "connected",
	}
}

// checkDepcheckTooling verifies that the CLI tools required by depcheck are
// available for each ecosystem detected across configured anvils.
func checkDepcheckTooling() []checkResult {
	if cfg == nil {
		return nil
	}
	if cfg.Settings.DepcheckInterval == 0 {
		return []checkResult{{
			Name:   "Depcheck tooling",
			Status: "ok",
			Detail: "depcheck disabled (interval = 0)",
		}}
	}

	// Scan anvil paths for ecosystem marker files.
	hasGo, hasDotNet, hasNpm := false, false, false
	for _, anvil := range cfg.Anvils {
		if anvil.DepcheckEnabled != nil && !*anvil.DepcheckEnabled {
			continue
		}
		if !hasGo {
			if _, err := os.Stat(filepath.Join(anvil.Path, "go.mod")); err == nil {
				hasGo = true
			}
		}
		if !hasDotNet {
			// Check for any .sln or .csproj in root.
			if matches, _ := filepath.Glob(filepath.Join(anvil.Path, "*.sln")); len(matches) > 0 {
				hasDotNet = true
			} else if matches, _ := filepath.Glob(filepath.Join(anvil.Path, "*.csproj")); len(matches) > 0 {
				hasDotNet = true
			}
		}
		if !hasNpm {
			if _, err := os.Stat(filepath.Join(anvil.Path, "package.json")); err == nil {
				hasNpm = true
			}
		}
	}

	var results []checkResult
	if hasGo {
		results = append(results, checkBinary("go", "Depcheck: Go"))
	}
	if hasDotNet {
		results = append(results, checkBinary("dotnet", "Depcheck: .NET"))
	}
	if hasNpm {
		results = append(results, checkBinary("npm", "Depcheck: npm"))
	}
	if len(results) == 0 {
		results = append(results, checkResult{
			Name:   "Depcheck tooling",
			Status: "ok",
			Detail: "no ecosystems detected in configured anvils",
		})
	}
	return results
}

// checkChangelogFragments validates all existing changelog fragments in
// changelog.d/ and reports any that have parse errors (bad category, etc.).
func checkChangelogFragments() checkResult {
	dir := "changelog.d"
	valid, errs := changelog.ValidateAllFragments(dir)
	if len(errs) == 0 {
		if valid == 0 {
			return checkResult{
				Name:   "Changelog fragments",
				Status: "ok",
				Detail: "no fragments present",
			}
		}
		return checkResult{
			Name:   "Changelog fragments",
			Status: "ok",
			Detail: fmt.Sprintf("%d fragments, all valid", valid),
		}
	}
	// Build a summary of the first few errors.
	summaries := make([]string, 0, len(errs))
	for i, e := range errs {
		if i >= 3 {
			summaries = append(summaries, fmt.Sprintf("… and %d more", len(errs)-3))
			break
		}
		summaries = append(summaries, e.Error())
	}
	return checkResult{
		Name:   "Changelog fragments",
		Status: "warn",
		Detail: fmt.Sprintf("%d valid, %d invalid: %s", valid, len(errs), strings.Join(summaries, "; ")),
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
