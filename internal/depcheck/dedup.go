package depcheck

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// createBeads creates bead issues for the outdated dependencies found in an anvil.
// Within each ecosystem, patch+minor updates go into a single auto-dispatch bead.
// Major updates get a separate "needs attention" bead per ecosystem.
// Duplicate beads (same kind already open) are skipped to prevent queue flooding.
func (m *Monitor) createBeads(ctx context.Context, result ScanResult) {
	byEco := result.UpdatesByEcosystem()

	for eco, updates := range byEco {
		var patchMinor, major []DependencyUpdate
		for _, u := range updates {
			if u.Kind == "major" {
				major = append(major, u)
			} else {
				patchMinor = append(patchMinor, u)
			}
		}

		if len(patchMinor) > 0 {
			phrase := fmt.Sprintf("%s dependencies (patch/minor)", eco)
			if !openBeadExists(ctx, result.Path, phrase) {
				m.createUpdateBead(ctx, result.Anvil, result.Path, eco, "auto", patchMinor)
			} else {
				log.Printf("[depcheck] %s: open %s patch/minor update bead already exists, skipping", result.Anvil, eco)
			}
		}

		if len(major) > 0 {
			phrase := fmt.Sprintf("%s major version updates", eco)
			if !openBeadExists(ctx, result.Path, phrase) {
				m.createUpdateBead(ctx, result.Anvil, result.Path, eco, "major", major)
			} else {
				log.Printf("[depcheck] %s: open %s major update bead already exists, skipping", result.Anvil, eco)
			}
		}
	}
}

// openBeadExists returns true if an open bead whose title contains phrase already
// exists in the anvil's bead queue. This prevents duplicate update beads from
// accumulating across check cycles.
func openBeadExists(ctx context.Context, anvilPath, phrase string) bool {
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "list", "--status=open", "--json"))
	cmd.Dir = anvilPath

	output, err := cmd.Output()
	if err != nil {
		// If we can't query, allow creation rather than silently suppressing beads.
		return false
	}

	return strings.Contains(string(output), phrase)
}

// CreateBeads creates beads for all scan results (for CLI use).
// Returns the number of beads created.
func (m *Monitor) CreateBeads(ctx context.Context, results []ScanResult) (int, error) {
	created := 0
	for _, result := range results {
		if result.Error != nil || len(result.Updates) == 0 {
			continue
		}
		created += m.createBeadsCount(ctx, result)
	}
	return created, nil
}

// createBeadsCount is like createBeads but returns the count of beads attempted.
func (m *Monitor) createBeadsCount(ctx context.Context, result ScanResult) int {
	count := 0
	byEco := result.UpdatesByEcosystem()

	for eco, updates := range byEco {
		var patchMinor, major []DependencyUpdate
		for _, u := range updates {
			if u.Kind == "major" {
				major = append(major, u)
			} else {
				patchMinor = append(patchMinor, u)
			}
		}

		if len(patchMinor) > 0 {
			phrase := fmt.Sprintf("%s dependencies (patch/minor)", eco)
			if !openBeadExists(ctx, result.Path, phrase) {
				m.createUpdateBead(ctx, result.Anvil, result.Path, eco, "auto", patchMinor)
				count++
			}
		}

		if len(major) > 0 {
			phrase := fmt.Sprintf("%s major version updates", eco)
			if !openBeadExists(ctx, result.Path, phrase) {
				m.createUpdateBead(ctx, result.Anvil, result.Path, eco, "major", major)
				count++
			}
		}
	}

	return count
}

// createUpdateBead runs 'bd create' to create a bead for dependency updates.
func (m *Monitor) createUpdateBead(ctx context.Context, anvil, anvilPath string, eco Ecosystem, kind string, updates []DependencyUpdate) {
	var title string
	var priority string
	var desc strings.Builder

	switch kind {
	case "auto":
		title = fmt.Sprintf("Update %d %s dependencies (patch/minor)", len(updates), eco)
		priority = "3"
		desc.WriteString(fmt.Sprintf("Automated dependency update for %s patch and minor version bumps.\n\n", eco))
	case "major":
		title = fmt.Sprintf("Review %d %s major version updates", len(updates), eco)
		priority = "2"
		desc.WriteString(fmt.Sprintf("Major %s version updates detected. These may contain breaking changes and require manual review.\n\n", eco))
	}

	desc.WriteString("## Outdated Packages\n\n")
	desc.WriteString("| Package | Current | Latest | Type |\n")
	desc.WriteString("|---------|---------|--------|------|\n")
	for _, u := range updates {
		pkg := u.Package
		if u.Subdir != "" {
			pkg = fmt.Sprintf("%s (%s)", u.Package, u.Subdir)
		}
		desc.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", pkg, u.Current, u.Latest, u.Kind))
	}

	if kind == "auto" {
		desc.WriteString("\n## Instructions\n\n")
		switch eco {
		case EcosystemGo:
			desc.WriteString("```bash\n")
			for _, u := range updates {
				desc.WriteString(fmt.Sprintf("go get %s@%s\n", u.Package, u.Latest))
			}
			desc.WriteString("go mod tidy\n")
			desc.WriteString("```\n")
		case EcosystemDotnet:
			desc.WriteString("```bash\n")
			for _, u := range updates {
				desc.WriteString(fmt.Sprintf("dotnet add package %s --version %s\n", u.Package, u.Latest))
			}
			desc.WriteString("```\n")
		case EcosystemNpm:
			desc.WriteString("```bash\n")
			var pkgs []string
			for _, u := range updates {
				pkgs = append(pkgs, fmt.Sprintf("%s@%s", u.Package, u.Latest))
			}
			desc.WriteString(fmt.Sprintf("npm install %s\n", strings.Join(pkgs, " ")))
			desc.WriteString("```\n")
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	issueType := "chore"
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "create",
		fmt.Sprintf("--title=%s", title),
		fmt.Sprintf("--description=%s", desc.String()),
		fmt.Sprintf("--type=%s", issueType),
		fmt.Sprintf("--priority=%s", priority),
		"--json",
	))
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		log.Printf("[depcheck] Failed to create bead for %s (%s %s): %v: %s", anvil, eco, kind, err, stderr.String())
		_ = m.db.LogEvent(state.EventDepcheckFailed,
			fmt.Sprintf("Failed to create %s %s update bead for %s: %v", eco, kind, anvil, err), "", anvil)
		return
	}

	log.Printf("[depcheck] Created %s %s update bead for %s: %s", eco, kind, anvil, strings.TrimSpace(string(output)))
	_ = m.db.LogEvent(state.EventDepcheckBeadCreated,
		fmt.Sprintf("Created %s %s dependency update bead for %s (%d packages)", eco, kind, anvil, len(updates)),
		"", anvil)
}
