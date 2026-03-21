package depupdate

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Robin831/Forge/internal/depcheck"
)

// PromptSelection asks the user interactively how to filter UpdateGroups.
// It reads from r and writes the prompt to w (typically os.Stdin / os.Stdout).
//
// The menu displayed is:
//
//	Update all? [a]ll / [p]atch+minor only / [s]elect groups / [n]o
//
// Returns the selected subset of groups (nil means "do nothing").
// Returns (nil, nil) for 'n' or EOF.
func PromptSelection(r io.Reader, w io.Writer, groups []UpdateGroup) ([]UpdateGroup, error) {
	if len(groups) == 0 {
		return nil, nil
	}

	scanner := bufio.NewScanner(r)
	fmt.Fprintf(w, "\nUpdate all? [a]ll / [p]atch+minor only / [s]elect groups / [n]o: ")

	if !scanner.Scan() {
		// EOF or error — treat as "no"
		fmt.Fprintln(w)
		return nil, nil
	}

	choice := strings.TrimSpace(strings.ToLower(scanner.Text()))
	switch choice {
	case "a", "":
		return groups, nil
	case "p":
		return filterGroupsByKind(groups, "patch", "minor"), nil
	case "s":
		return promptSelectGroups(w, groups, scanner)
	case "n":
		fmt.Fprintln(w, "No updates applied.")
		return nil, nil
	default:
		fmt.Fprintf(w, "Unknown choice %q; no updates applied.\n", choice)
		return nil, nil
	}
}

// filterGroupsByKind returns only groups whose Kind is in the allowed set.
func filterGroupsByKind(groups []UpdateGroup, allowed ...string) []UpdateGroup {
	set := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		set[k] = true
	}
	var out []UpdateGroup
	for _, g := range groups {
		if set[g.Kind] {
			out = append(out, g)
		}
	}
	return out
}

// promptSelectGroups renders a numbered group list and parses comma-separated
// input to return the caller-selected subset.
func promptSelectGroups(w io.Writer, groups []UpdateGroup, scanner *bufio.Scanner) ([]UpdateGroup, error) {
	fmt.Fprintln(w, "\nAvailable groups:")
	for i, g := range groups {
		fmt.Fprintf(w, "  %2d. %-40s  %s  (%d package(s))\n", i+1, g.Name, g.Kind, len(g.Updates))
	}
	fmt.Fprint(w, "Enter group numbers (comma-separated, e.g. 1,3): ")

	if !scanner.Scan() {
		fmt.Fprintln(w)
		return nil, nil
	}

	input := strings.TrimSpace(scanner.Text())
	if input == "" {
		fmt.Fprintln(w, "No groups selected; no updates applied.")
		return nil, nil
	}

	seen := make(map[int]bool)
	var selected []UpdateGroup
	for _, part := range strings.Split(input, ",") {
		part = strings.TrimSpace(part)
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > len(groups) {
			fmt.Fprintf(w, "  Ignoring invalid entry %q\n", part)
			continue
		}
		if !seen[n] {
			seen[n] = true
			selected = append(selected, groups[n-1])
		}
	}
	return selected, nil
}

// BuildFilteredGroups is the convenience entry point used by the CLI command.
// It collects all ecosystem results across anvils, applies opts filtering, and
// delegates to GroupUpdates for intelligent grouping of related packages.
func BuildFilteredGroups(ctx context.Context, results []AnvilResult, opts Options) []UpdateGroup {
	var all []*depcheck.CheckResult
	for _, ar := range results {
		all = append(all, applyOptsToCheckResults(ar.Ecosystems, opts)...)
	}
	return GroupUpdates(ctx, all)
}

// applyOptsToCheckResults returns shallow copies of each CheckResult with the
// Patch/Minor/Major slices pre-filtered according to opts. This is used to
// restrict which packages are considered when building UpdateGroups.
func applyOptsToCheckResults(crs []*depcheck.CheckResult, opts Options) []*depcheck.CheckResult {
	out := make([]*depcheck.CheckResult, 0, len(crs))
	for _, cr := range crs {
		if cr == nil || cr.Error != nil {
			out = append(out, cr)
			continue
		}
		filtered := filterUpdates(cr, opts)
		if len(filtered) == 0 {
			continue
		}
		// Reconstruct a CheckResult with only the filtered updates.
		fc := &depcheck.CheckResult{
			Anvil:     cr.Anvil,
			Path:      cr.Path,
			Ecosystem: cr.Ecosystem,
			Checked:   cr.Checked,
		}
		for _, u := range filtered {
			switch u.Kind {
			case "major":
				fc.Major = append(fc.Major, u)
			case "minor":
				fc.Minor = append(fc.Minor, u)
			default:
				fc.Patch = append(fc.Patch, u)
			}
		}
		out = append(out, fc)
	}
	return out
}
