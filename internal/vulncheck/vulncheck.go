// Package vulncheck runs govulncheck on registered anvils and creates beads
// for discovered vulnerabilities.
//
// It can run as a background daemon goroutine (scheduled scanning) or be
// invoked on-demand via the "forge scan" CLI subcommand.
package vulncheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// Finding groups a vulnerability with its call stacks for govulncheck v1 JSON.
type Finding struct {
	OSV   string `json:"osv"`
	Trace []struct {
		Module  string `json:"module"`
		Version string `json:"version"`
		Package string `json:"package"`
	} `json:"trace"`
}

// govulncheckMessage is a single line of govulncheck -json output.
// Each line is a JSON object with exactly one field set.
type govulncheckMessage struct {
	OSV     *osvEntry `json:"osv,omitempty"`
	Finding *Finding  `json:"finding,omitempty"`
}

// osvEntry represents the osv field in govulncheck JSON output.
type osvEntry struct {
	ID       string   `json:"id"`
	Aliases  []string `json:"aliases"`
	Summary  string   `json:"summary"`
	Details  string   `json:"details"`
	Affected []struct {
		Package struct {
			Name      string `json:"name"`
			Ecosystem string `json:"ecosystem"`
		} `json:"package"`
		Ranges []struct {
			Events []struct {
				Introduced string `json:"introduced,omitempty"`
				Fixed      string `json:"fixed,omitempty"`
			} `json:"events"`
		} `json:"ranges"`
		EcosystemSpecific *struct {
			Imports []struct {
				Path    string   `json:"path"`
				Symbols []string `json:"symbols"`
			} `json:"imports"`
		} `json:"ecosystem_specific,omitempty"`
	} `json:"affected"`
	DatabaseSpecific *struct {
		Severity string `json:"severity"`
	} `json:"database_specific,omitempty"`
}

// ScanResult holds the outcome of scanning a single anvil.
type ScanResult struct {
	Anvil   string
	Path    string
	Vulns   []ParsedVuln
	Err     error
	Scanned time.Time
}

// MarshalJSON provides a custom JSON encoding for ScanResult that renders Err
// as a string field, so that JSON output includes a meaningful error message.
func (sr ScanResult) MarshalJSON() ([]byte, error) {
	type scanResultJSON struct {
		Anvil   string       `json:"anvil"`
		Path    string       `json:"path"`
		Vulns   []ParsedVuln `json:"vulns"`
		Err     string       `json:"err,omitempty"`
		Scanned time.Time    `json:"scanned"`
	}

	out := scanResultJSON{
		Anvil:   sr.Anvil,
		Path:    sr.Path,
		Vulns:   sr.Vulns,
		Scanned: sr.Scanned,
	}

	if sr.Err != nil {
		out.Err = sr.Err.Error()
	}

	return json.Marshal(out)
}

// ParsedVuln is a processed vulnerability ready for bead creation.
type ParsedVuln struct {
	ID          string   // e.g. "GO-2024-1234"
	CVEs        []string // extracted CVE IDs
	Summary     string
	Details     string
	Severity    string // "CRITICAL", "HIGH", "MEDIUM", "LOW"
	AffectedPkg string // module path
	FixedIn     string // version that fixes it
	Symbols     []string
}

// Scanner runs govulncheck on anvils.
type Scanner struct {
	db     *state.DB
	logger *slog.Logger
	anvils map[string]config.AnvilConfig
}

// New creates a Scanner.
func New(db *state.DB, logger *slog.Logger, anvils map[string]config.AnvilConfig) *Scanner {
	return &Scanner{
		db:     db,
		logger: logger,
		anvils: anvils,
	}
}

// ScanAll runs govulncheck on all Go-based anvils and returns results.
func (s *Scanner) ScanAll(ctx context.Context) []ScanResult {
	var results []ScanResult

	for name, anvil := range s.anvils {
		if ctx.Err() != nil {
			break
		}

		// Only scan Go projects (must have go.mod)
		goMod := filepath.Join(anvil.Path, "go.mod")
		if _, err := os.Stat(goMod); err != nil {
			s.logger.Debug("skipping non-Go anvil", "anvil", name)
			continue
		}

		s.logger.Info("scanning anvil for vulnerabilities", "anvil", name)
		s.db.LogEvent(state.EventVulnScanStarted, fmt.Sprintf("scanning %s", name), "", name)

		result := s.scanAnvil(ctx, name, anvil.Path)
		results = append(results, result)

		if result.Err != nil {
			s.logger.Error("vulncheck failed", "anvil", name, "error", result.Err)
			s.db.LogEvent(state.EventVulnScanFailed, fmt.Sprintf("scan failed for %s: %v", name, result.Err), "", name)
		} else {
			s.logger.Info("vulncheck complete", "anvil", name, "vulns", len(result.Vulns))
			s.db.LogEvent(state.EventVulnScanDone,
				fmt.Sprintf("found %d vulnerabilities in %s", len(result.Vulns), name), "", name)
		}
	}

	return results
}

// scanAnvil runs govulncheck on a single anvil directory.
func (s *Scanner) scanAnvil(ctx context.Context, name, path string) ScanResult {
	result := ScanResult{
		Anvil:   name,
		Path:    path,
		Scanned: time.Now(),
	}

	// Run govulncheck with JSON output
	cmd := exec.CommandContext(ctx, "govulncheck", "-json", "./...")
	cmd.Dir = path
	executil.HideWindow(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	// govulncheck exits non-zero when vulnerabilities are found — that's expected.
	// Only treat it as an error if we got no stdout at all.
	if err != nil && stdout.Len() == 0 {
		result.Err = fmt.Errorf("govulncheck: %w: %s", err, stderr.String())
		return result
	}

	vulns, parseErr := parseGovulncheckJSON(stdout.Bytes())
	if parseErr != nil {
		result.Err = fmt.Errorf("parsing govulncheck output: %w", parseErr)
		return result
	}

	result.Vulns = vulns
	return result
}

// parseGovulncheckJSON parses the newline-delimited JSON output from govulncheck.
func parseGovulncheckJSON(data []byte) ([]ParsedVuln, error) {
	// govulncheck -json emits one JSON object per line.
	// We collect OSV entries and findings, then merge them.
	osvMap := make(map[string]*osvEntry)
	findingOSVs := make(map[string]bool) // OSV IDs that have actual findings (called vulns)

	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var msg govulncheckMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			// Skip non-JSON lines (govulncheck sometimes emits progress info)
			continue
		}

		if msg.OSV != nil {
			osvMap[msg.OSV.ID] = msg.OSV
		}
		if msg.Finding != nil {
			findingOSVs[msg.Finding.OSV] = true
		}
	}

	// Only report vulnerabilities that have actual findings (called in code)
	var vulns []ParsedVuln
	for id := range findingOSVs {
		osv, ok := osvMap[id]
		if !ok {
			continue
		}

		v := ParsedVuln{
			ID:      osv.ID,
			Summary: osv.Summary,
			Details: osv.Details,
		}

		// Extract severity
		if osv.DatabaseSpecific != nil {
			v.Severity = strings.ToUpper(osv.DatabaseSpecific.Severity)
		}
		if v.Severity == "" {
			v.Severity = "MEDIUM" // default if not specified
		}

		// Extract CVE IDs from OSV aliases (preferred) and ID (fallback)
		for _, alias := range osv.Aliases {
			if strings.HasPrefix(alias, "CVE-") {
				v.CVEs = append(v.CVEs, alias)
			}
		}
		if strings.HasPrefix(osv.ID, "CVE-") {
			seen := false
			for _, c := range v.CVEs {
				if c == osv.ID {
					seen = true
					break
				}
			}
			if !seen {
				v.CVEs = append(v.CVEs, osv.ID)
			}
		}

		// Extract affected packages, fix versions, and symbols
		for _, aff := range osv.Affected {
			if v.AffectedPkg == "" {
				v.AffectedPkg = aff.Package.Name
			}
			for _, r := range aff.Ranges {
				for _, ev := range r.Events {
					if ev.Fixed != "" && v.FixedIn == "" {
						v.FixedIn = ev.Fixed
					}
				}
			}
			if aff.EcosystemSpecific != nil {
				for _, imp := range aff.EcosystemSpecific.Imports {
					v.Symbols = append(v.Symbols, imp.Symbols...)
				}
			}
		}

		vulns = append(vulns, v)
	}

	return vulns, nil
}

// CreateBeads creates bead issues for discovered vulnerabilities via `bd create`.
func (s *Scanner) CreateBeads(ctx context.Context, results []ScanResult) (int, error) {
	created := 0

	for _, result := range results {
		if result.Err != nil || len(result.Vulns) == 0 {
			continue
		}

		for _, vuln := range result.Vulns {
			if ctx.Err() != nil {
				return created, ctx.Err()
			}

			title := fmt.Sprintf("Security: %s — %s", vuln.ID, truncate(vuln.Summary, 60))
			description := buildBeadDescription(vuln, result.Anvil)
			priority := severityToPriority(vuln.Severity)

			// Check if a bead already exists for this vuln (search by OSV ID)
			if exists, _ := s.beadExists(ctx, result.Path, vuln.ID); exists {
				s.logger.Debug("bead already exists for vuln", "vuln", vuln.ID, "anvil", result.Anvil)
				continue
			}

			if err := s.createBead(ctx, result.Path, title, description, priority); err != nil {
				s.logger.Error("failed to create bead for vuln", "vuln", vuln.ID, "anvil", result.Anvil, "error", err)
				continue
			}

			s.logger.Info("created bead for vulnerability", "vuln", vuln.ID, "anvil", result.Anvil, "priority", priority)
			s.db.LogEvent(state.EventVulnBeadCreated,
				fmt.Sprintf("created bead for %s in %s (P%d)", vuln.ID, result.Anvil, priority),
				"", result.Anvil)
			created++
		}
	}

	return created, nil
}

// beadExists checks whether a bead already references this vulnerability ID.
func (s *Scanner) beadExists(ctx context.Context, anvilPath, vulnID string) (bool, error) {
	cmd := exec.CommandContext(ctx, "bd", "search", vulnID, "--json")
	cmd.Dir = anvilPath
	executil.HideWindow(cmd)

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// bd search may return non-zero if no results; that's fine
		return false, nil
	}

	// If we got any results, consider the bead as existing
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || string(trimmed) == "[]" || string(trimmed) == "null" {
		return false, nil
	}
	return true, nil
}

// createBead calls `bd create` to make a new issue.
func (s *Scanner) createBead(ctx context.Context, anvilPath, title, description string, priority int) error {
	cmd := exec.CommandContext(ctx, "bd", "create",
		"--title", title,
		"--description", description,
		"--type", "bug",
		fmt.Sprintf("--priority=%d", priority),
		"--json",
	)
	cmd.Dir = anvilPath
	executil.HideWindow(cmd)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd create: %w: %s", err, string(out))
	}
	return nil
}

// buildBeadDescription formats a vulnerability into a detailed bead description.
func buildBeadDescription(v ParsedVuln, anvil string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Vulnerability: %s\n\n", v.ID)

	if len(v.CVEs) > 0 {
		fmt.Fprintf(&b, "**CVEs**: %s\n", strings.Join(v.CVEs, ", "))
	}
	fmt.Fprintf(&b, "**Severity**: %s\n", v.Severity)
	fmt.Fprintf(&b, "**Anvil**: %s\n\n", anvil)

	if v.Summary != "" {
		fmt.Fprintf(&b, "### Summary\n%s\n\n", v.Summary)
	}
	if v.Details != "" {
		fmt.Fprintf(&b, "### Details\n%s\n\n", v.Details)
	}

	fmt.Fprintf(&b, "### Affected Package\n")
	if v.AffectedPkg != "" {
		fmt.Fprintf(&b, "- **Module**: %s\n", v.AffectedPkg)
	}
	if v.FixedIn != "" {
		fmt.Fprintf(&b, "- **Fixed in**: %s\n", v.FixedIn)
	}
	if len(v.Symbols) > 0 {
		fmt.Fprintf(&b, "- **Symbols**: %s\n", strings.Join(v.Symbols, ", "))
	}

	fmt.Fprintf(&b, "\n### Suggested Remediation\n")
	if v.FixedIn != "" {
		fmt.Fprintf(&b, "Upgrade the affected module to version %s or later:\n", v.FixedIn)
		fmt.Fprintf(&b, "```bash\ngo get %s@v%s\ngo mod tidy\n```\n", v.AffectedPkg, v.FixedIn)
	} else {
		fmt.Fprintf(&b, "No fix version is currently available. Consider:\n")
		fmt.Fprintf(&b, "- Checking for alternative packages\n")
		fmt.Fprintf(&b, "- Reviewing if the vulnerable code path is reachable\n")
		fmt.Fprintf(&b, "- Monitoring for upstream fixes\n")
	}

	fmt.Fprintf(&b, "\n*Auto-detected by `forge scan` (govulncheck)*\n")
	return b.String()
}

// severityToPriority maps vulnerability severity to bead priority.
func severityToPriority(severity string) int {
	switch strings.ToUpper(severity) {
	case "CRITICAL":
		return 1
	case "HIGH":
		return 2
	case "MEDIUM":
		return 3
	case "LOW":
		return 4
	default:
		return 3
	}
}

// truncate shortens a string to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// RunScheduled is a blocking loop that runs scans on a configurable interval.
// It should be launched as a goroutine alongside bellows.
func (s *Scanner) RunScheduled(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		s.logger.Info("vulncheck scheduled scanning disabled (interval=0)")
		return
	}

	s.logger.Info("vulncheck scheduled scanning started", "interval", interval)

	// Run an initial scan after a short delay (don't scan on startup to avoid
	// blocking other daemon init work).
	initialDelay := 30 * time.Second
	select {
	case <-ctx.Done():
		return
	case <-time.After(initialDelay):
	}

	s.runOnce(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("vulncheck scheduled scanning stopped")
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

// runOnce performs a single scan-and-create-beads cycle.
func (s *Scanner) runOnce(ctx context.Context) {
	results := s.ScanAll(ctx)
	created, err := s.CreateBeads(ctx, results)
	if err != nil {
		s.logger.Error("vulncheck bead creation error", "error", err)
		return
	}
	if created > 0 {
		s.logger.Info("vulncheck created new beads", "count", created)
	}
}
