package vulncheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Robin831/Forge/internal/executil"
)

// vulnBeadKind names this scanner's rows in state.anvil_beads.
const vulnBeadKind = "vulncheck"

// vulnBeadTitlePrefix opens the title of every bead a vulnerability produces. It
// narrows a listing to plausible candidates; it never decides whose bead one is.
const vulnBeadTitlePrefix = "Security: "

// The two description lines that attribute a vulnerability bead. Both are
// already written by buildBeadDescription and so are carried by every bead
// created before the pin existed, which is what makes those adoptable.
const (
	vulnBeadIDPrefix    = "## Vulnerability: "
	vulnBeadAnvilPrefix = "**Anvil**: "
)

// bdBead is a minimal struct for parsing bd list/show --json output.
type bdBead struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

// vulnBeadOwner reads the anvil and vulnerability id a bead's description
// attributes it to. Either is "" when the description does not carry that line,
// and an unattributed bead is adopted by nobody.
func vulnBeadOwner(desc string) (anvil, vulnID string) {
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, vulnBeadIDPrefix); ok && vulnID == "" {
			vulnID = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(line, vulnBeadAnvilPrefix); ok && anvil == "" {
			anvil = strings.TrimSpace(rest)
		}
	}
	return anvil, vulnID
}

// beadPinStore records which bead holds an anvil's report of one recurring
// condition. It is the subset of state.DB the lookup needs, so the resolution
// can be driven in a test without a SQLite file.
type beadPinStore interface {
	AnvilBead(anvil, kind, key string) (string, error)
	SetAnvilBead(anvil, kind, key, beadID string) error
	ClearAnvilBead(anvil, kind, key string) error
}

// vulnBeadLookup resolves the bead already reporting one anvil's exposure to one
// vulnerability, or reports that there is none.
//
// The check it replaces was `strings.Contains(bdListJSON, vulnID)` over every
// open bead in the pool, which is wrong three ways and only one of them is about
// anvils:
//
//   - A beads pool can hold two anvils (Munin and Explorer share one on
//     purpose). Both depend on the same library, both hit the same CVE, and the
//     second anvil to scan found the first anvil's bead in the raw JSON and
//     created nothing — so one of the two exposures was never reported.
//   - An OSV id is a prefix of a longer one. GO-2026-1234 is a substring of
//     GO-2026-12345, so a bead for the latter suppressed the former.
//   - A bead that merely mentions an id in prose — "we should also check
//     GO-2026-1234" — suppressed the real one.
//
// Ownership is pinned by bead id instead. The listing scan is the adoption path
// and runs only until the anvil has a pin: for a bead created before ownership
// was recorded, or after the state database is lost.
type vulnBeadLookup struct {
	anvil  string
	vulnID string
	pins   beadPinStore
	// showBead returns one bead by id. (nil, nil) means bd answered that the
	// bead does not exist; an error means no answer was obtained at all.
	showBead func(id string) (*bdBead, error)
	// openBeads returns the open beads that could be this anvil's.
	openBeads func() ([]bdBead, error)
}

// resolve returns the anvil's open bead for this vulnerability, or (nil, nil)
// when it has none.
//
// The two failure directions are not symmetric, and neither is the handling. A
// PINNED bead that cannot be read is an error: Forge recorded that it created
// this bead, so an unreachable bd is no reason to believe it is gone, and
// creating on that belief files a duplicate every scan. A LISTING that cannot be
// read leaves resolve returning (nil, nil): with no pin there is no evidence a
// bead exists, and the caller's rule for a security finding is to report it and
// tolerate a duplicate rather than sit on it. The old code applied the second
// rule to both cases because it had no first case.
func (l vulnBeadLookup) resolve() (*bdBead, error) {
	pin, err := l.pinned()
	if err != nil {
		return nil, err
	}
	if pin != nil {
		return pin, nil
	}

	beads, err := l.openBeads()
	if err != nil {
		slog.Warn("could not list beads to check for an existing vulnerability bead; creating rather than suppressing",
			"anvil", l.anvil, "vuln", l.vulnID, "error", err)
		return nil, nil
	}
	found := selectVulnBead(beads, l.anvil, l.vulnID)
	if found == nil {
		return nil, nil
	}
	slog.Info("adopting existing vulnerability bead",
		"anvil", l.anvil, "vuln", l.vulnID, "bead", found.ID)
	l.Remember(found.ID)
	return found, nil
}

// pinned returns the recorded bead when it still exists and is still open,
// clearing the record when bd answers that it does not. The record is
// deliberately not checked against the title, so a retitled bead stays this
// anvil's bead.
func (l vulnBeadLookup) pinned() (*bdBead, error) {
	if l.pins == nil || l.showBead == nil {
		return nil, nil
	}
	id, err := l.pins.AnvilBead(l.anvil, vulnBeadKind, l.vulnID)
	if err != nil {
		// A Forge-local read failure: fall through to the listing scan rather
		// than hold up the anvil's vulnerability reporting on it.
		slog.Warn("could not read the recorded vulnerability bead",
			"anvil", l.anvil, "vuln", l.vulnID, "error", err)
		return nil, nil
	}
	if id == "" {
		return nil, nil
	}
	b, err := l.showBead(id)
	if err != nil {
		return nil, fmt.Errorf("reading recorded vulnerability bead %s: %w", id, err)
	}
	if b == nil || b.ID == "" || !strings.EqualFold(strings.TrimSpace(b.Status), "open") {
		l.forget()
		return nil, nil
	}
	return b, nil
}

// Remember pins a bead as this anvil's report of this vulnerability.
func (l vulnBeadLookup) Remember(beadID string) {
	if l.pins == nil || beadID == "" {
		return
	}
	if err := l.pins.SetAnvilBead(l.anvil, vulnBeadKind, l.vulnID, beadID); err != nil {
		slog.Warn("could not record vulnerability bead",
			"anvil", l.anvil, "vuln", l.vulnID, "bead", beadID, "error", err)
	}
}

func (l vulnBeadLookup) forget() {
	if l.pins == nil {
		return
	}
	if err := l.pins.ClearAnvilBead(l.anvil, vulnBeadKind, l.vulnID); err != nil {
		slog.Warn("could not clear the recorded vulnerability bead",
			"anvil", l.anvil, "vuln", l.vulnID, "error", err)
	}
}

// selectVulnBead picks this anvil's bead for this vulnerability out of a set of
// open beads. Both the id and the anvil are compared as whole parsed values, so
// GO-2026-1234 no longer matches GO-2026-12345's bead.
func selectVulnBead(beads []bdBead, anvil, vulnID string) *bdBead {
	if anvil == "" || vulnID == "" {
		return nil
	}
	for i := range beads {
		if !strings.HasPrefix(beads[i].Title, vulnBeadTitlePrefix) {
			continue
		}
		beadAnvil, beadVuln := vulnBeadOwner(beads[i].Description)
		if !strings.EqualFold(beadAnvil, anvil) || beadVuln != vulnID {
			continue
		}
		return &beads[i]
	}
	return nil
}

// vulnBeadLookupFor builds the lookup CreateBeads uses against a real beads
// workspace.
func (s *Scanner) vulnBeadLookupFor(ctx context.Context, anvilName, anvilPath, vulnID string) vulnBeadLookup {
	return vulnBeadLookup{
		anvil:  anvilName,
		vulnID: vulnID,
		pins:   s.pinStore(),
		showBead: func(id string) (*bdBead, error) {
			return showBead(ctx, anvilPath, id)
		},
		openBeads: func() ([]bdBead, error) {
			return fetchOpenBeads(ctx, anvilPath)
		},
	}
}

// pinStore returns the Scanner's pin store, or nil when it has no state
// database — a typed nil *state.DB in the interface would panic on use.
func (s *Scanner) pinStore() beadPinStore {
	if s.db == nil {
		return nil
	}
	return s.db
}

// fetchOpenBeads lists the open beads in the anvil's pool.
func fetchOpenBeads(ctx context.Context, anvilPath string) ([]bdBead, error) {
	cmd, cancel := executil.BdCommand(ctx, "list", "--status=open", "--limit", "0", "--json")
	defer cancel()
	cmd.Dir = anvilPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd list --status=open in %s: %w", anvilPath, err)
	}
	var beads []bdBead
	if err := json.Unmarshal(out, &beads); err != nil {
		return nil, fmt.Errorf("parse bd list output: %w", err)
	}
	return beads, nil
}

// showBead reads one bead by id, separating "bd says it does not exist" from
// "bd did not answer". The pinned lookup cannot conflate them: forgetting a pin
// on a timeout is how an anvil ends up with a second bead for one CVE.
func showBead(ctx context.Context, anvilPath, beadID string) (*bdBead, error) {
	cmd, cancel := executil.BdCommand(ctx, "show", beadID, "--json")
	defer cancel()
	cmd.Dir = anvilPath
	out, _ := cmd.Output()

	if b := executil.DecodeOneBead(out, func(b bdBead) string { return b.ID }); b != nil {
		return b, nil
	}
	if executil.BdReportsNoSuchBead(out) {
		return nil, nil
	}
	return nil, fmt.Errorf("bd show %s in %s returned no bead", beadID, anvilPath)
}
