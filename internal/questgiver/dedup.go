package questgiver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Robin831/Forge/internal/executil"
)

// questBeadKind names this scanner's rows in state.anvil_beads.
const questBeadKind = "questgiver"

// questBeadTitlePrefix opens the title of every bead a failed quest produces. It
// narrows a listing to plausible candidates; it never decides whose bead one is.
// The quest name is deliberately not part of it: as a prefix, "login" also
// matches "login-admin"'s bead.
const questBeadTitlePrefix = "E2E failure: "

// The two description lines that attribute a quest bead. They are the only
// markers a bead created before the pin existed can carry, so they are what the
// adoption path in questBeadLookup.resolve matches on — by whole-line equality,
// never as a substring.
const (
	questBeadAnvilPrefix = "Anvil: "
	questBeadQuestPrefix = "Quest: "
)

// bdBead is a minimal struct for parsing bd list --json output.
type bdBead struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

// questBeadDescription renders the description of a quest-failure bead. The
// anvil and quest lines come first because they are the bead's attribution.
func questBeadDescription(anvil string, quest *Quest, result *QuestResult, stepAction string) string {
	return fmt.Sprintf(
		"%s%s\n%s%s\nFailed step: %d (action: %s)\nError: %s\nQuest file: %s\nReproduce: forge quest run %s",
		questBeadAnvilPrefix, anvil,
		questBeadQuestPrefix, quest.Name,
		result.FailedStep, stepAction, result.ErrorMessage, quest.FilePath, quest.Name,
	)
}

// questBeadOwner reads the anvil and quest a bead's description attributes it
// to. Either is "" when the description does not carry that line, and an
// unattributed bead is adopted by nobody: a duplicate bead is a nuisance,
// while suppressing one anvil's E2E failure because another anvil's quest of
// the same name already failed loses the report entirely.
func questBeadOwner(desc string) (anvil, quest string) {
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, questBeadAnvilPrefix); ok && anvil == "" {
			anvil = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(line, questBeadQuestPrefix); ok && quest == "" {
			quest = strings.TrimSpace(rest)
		}
	}
	return anvil, quest
}

// beadPinStore records which bead holds an anvil's report of one recurring
// condition. It is the subset of state.DB the lookup needs, so the resolution
// can be driven in a test without a SQLite file.
type beadPinStore interface {
	AnvilBead(anvil, kind, key string) (string, error)
	SetAnvilBead(anvil, kind, key, beadID string) error
	ClearAnvilBead(anvil, kind, key string) error
}

// questBeadLookup resolves the bead already reporting one anvil's failure of one
// quest, or reports that there is none.
//
// Ownership is pinned by bead id, not matched by title: a beads pool can hold
// two anvils (Munin and Explorer share one deliberately), and a quest name is a
// filename in each repository — nothing stops both from having a "login" quest.
// Matched on the title, the second anvil's failure found the first anvil's bead
// and created nothing at all, so the failure was never reported. A title is also
// the field a human tidies.
//
// The listing scan is the adoption path and runs only until the anvil has a pin:
// for a bead created before ownership was recorded, or after the state database
// is lost.
type questBeadLookup struct {
	anvil string
	quest string
	pins  beadPinStore
	// showBead returns one bead by id. (nil, nil) means bd answered that the
	// bead does not exist; an error means no answer was obtained at all.
	showBead func(id string) (*bdBead, error)
	// activeBeads returns the open and in-progress beads that could be this
	// anvil's.
	activeBeads func() ([]bdBead, error)
}

// resolve returns the anvil's open bead for this quest, or (nil, nil) when it
// has none. An error means the answer is unknown, and the caller must not
// create: an unknown answer that creates is how a scheduled scan files the same
// bead every cycle.
func (l questBeadLookup) resolve() (*bdBead, error) {
	pin, err := l.pinned()
	if err != nil {
		return nil, err
	}
	if pin != nil {
		return pin, nil
	}

	beads, err := l.activeBeads()
	if err != nil {
		return nil, err
	}
	found := selectQuestBead(beads, l.anvil, l.quest)
	if found == nil {
		return nil, nil
	}
	slog.Info("adopting existing quest-failure bead",
		"anvil", l.anvil, "quest", l.quest, "bead", found.ID)
	l.Remember(found.ID)
	return found, nil
}

// pinned returns the recorded bead when it still exists and is still active,
// clearing the record when bd answers that it does not. The record is
// deliberately not checked against the title, so a retitled bead stays this
// anvil's bead.
//
// A failure to READ the pinned bead is an error, not a missing bead: bd exits
// non-zero both for a deleted bead and for a timeout, and treating a timeout as
// "no bead" would file the duplicate the pin exists to prevent.
func (l questBeadLookup) pinned() (*bdBead, error) {
	if l.pins == nil || l.showBead == nil {
		return nil, nil
	}
	id, err := l.pins.AnvilBead(l.anvil, questBeadKind, l.quest)
	if err != nil {
		// A Forge-local read failure: fall through to the listing scan rather
		// than hold up the anvil's E2E reporting on it.
		slog.Warn("could not read the recorded quest bead",
			"anvil", l.anvil, "quest", l.quest, "error", err)
		return nil, nil
	}
	if id == "" {
		return nil, nil
	}
	b, err := l.showBead(id)
	if err != nil {
		return nil, fmt.Errorf("reading recorded quest bead %s: %w", id, err)
	}
	if b == nil || b.ID == "" || !isActiveBeadStatus(b.Status) {
		l.forget()
		return nil, nil
	}
	return b, nil
}

// Remember pins a bead as this anvil's report of this quest.
func (l questBeadLookup) Remember(beadID string) {
	if l.pins == nil || beadID == "" {
		return
	}
	if err := l.pins.SetAnvilBead(l.anvil, questBeadKind, l.quest, beadID); err != nil {
		slog.Warn("could not record quest bead",
			"anvil", l.anvil, "quest", l.quest, "bead", beadID, "error", err)
	}
}

func (l questBeadLookup) forget() {
	if l.pins == nil {
		return
	}
	if err := l.pins.ClearAnvilBead(l.anvil, questBeadKind, l.quest); err != nil {
		slog.Warn("could not clear the recorded quest bead",
			"anvil", l.anvil, "quest", l.quest, "error", err)
	}
}

// isActiveBeadStatus reports whether a bead in this status still counts as an
// open report. A bead someone has claimed suppresses a new one; a closed bead
// does not, so a quest that starts failing again is reported again.
func isActiveBeadStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "open", "in_progress":
		return true
	default:
		return false
	}
}

// selectQuestBead picks this anvil's bead for this quest out of a set of active
// beads. A bead the description does not attribute to both is never returned,
// whoever else it belongs to.
func selectQuestBead(beads []bdBead, anvil, quest string) *bdBead {
	if anvil == "" || quest == "" {
		return nil
	}
	for i := range beads {
		if !strings.HasPrefix(beads[i].Title, questBeadTitlePrefix) {
			continue
		}
		if !isActiveBeadStatus(beads[i].Status) {
			continue
		}
		beadAnvil, beadQuest := questBeadOwner(beads[i].Description)
		if !strings.EqualFold(beadAnvil, anvil) || beadQuest != quest {
			continue
		}
		return &beads[i]
	}
	return nil
}

// questBeadLookupFor builds the lookup findQuestBead uses against a real beads
// workspace.
func (m *Monitor) questBeadLookupFor(ctx context.Context, anvilName, anvilPath, questName string) questBeadLookup {
	return questBeadLookup{
		anvil: anvilName,
		quest: questName,
		pins:  m.pinStore(),
		showBead: func(id string) (*bdBead, error) {
			return showBead(ctx, anvilPath, id)
		},
		activeBeads: func() ([]bdBead, error) {
			return fetchActiveBeads(ctx, anvilPath)
		},
	}
}

// pinStore returns the Monitor's pin store, or nil when it has no state
// database — a typed nil *state.DB in the interface would panic on use.
func (m *Monitor) pinStore() beadPinStore {
	if m.db == nil {
		return nil
	}
	return m.db
}

// fetchActiveBeads lists the open and in-progress beads that could be a quest
// bead. The status split is bd's, not ours: it lists one status at a time.
func fetchActiveBeads(ctx context.Context, anvilPath string) ([]bdBead, error) {
	var all []bdBead
	for _, status := range []string{"open", "in_progress"} {
		cmd, cancel := executil.BdCommand(ctx,
			"list", "--status="+status, "--limit", "0", "--json")
		cmd.Dir = anvilPath
		out, err := cmd.Output()
		cancel()
		if err != nil {
			return nil, fmt.Errorf("bd list --status=%s in %s: %w", status, anvilPath, err)
		}
		var beads []bdBead
		if err := json.Unmarshal(out, &beads); err != nil {
			return nil, fmt.Errorf("parse bd list --status=%s output: %w", status, err)
		}
		all = append(all, beads...)
	}
	return all, nil
}

// showBead reads one bead by id, separating "bd says it does not exist" from
// "bd did not answer". The pinned lookup cannot conflate them: forgetting a pin
// on a timeout is how an anvil ends up with a second bead for one quest.
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
