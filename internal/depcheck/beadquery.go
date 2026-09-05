package depcheck

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// bdBead is a minimal struct for parsing bd list/show --json output.
type bdBead struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Labels      []string `json:"labels"`
	ClosedAt    string   `json:"closed_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// fetchBeadList runs bd sql (fast) or falls back to bd list for the given status.
func fetchBeadList(ctx context.Context, anvilPath, status string) ([]byte, error) {
	// Try bd sql first (~6x faster than bd list on Dolt).
	query := fmt.Sprintf(`SELECT * FROM issues WHERE status = '%s'`, status)
	cmd, cancel := executil.BdCommand(ctx, "sql", "--json", query)
	defer cancel()
	cmd.Dir = anvilPath
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}

	// Fall back to bd list.
	cmd2, cancel2 := executil.BdCommand(ctx,
		"list", fmt.Sprintf("--status=%s", status), "--limit", "0", "--json")
	defer cancel2()
	cmd2.Dir = anvilPath
	out, err = cmd2.Output()
	if err != nil {
		log.Printf("[depcheck] bd list --status=%s failed in %s: %v", status, anvilPath, err)
		return nil, err
	}
	return out, nil
}

// fetchBeadShow runs bd show for a single bead ID and returns raw output. It
// discards the exec error: showBead is what separates "bd says no such bead"
// from "bd did not answer", and it does so on bd's own message.
func fetchBeadShow(ctx context.Context, anvilPath, beadID string) []byte {
	cmd, cancel := executil.BdCommandTimeout(ctx, 3*time.Minute,
		"show", beadID, "--json")
	defer cancel()
	cmd.Dir = anvilPath
	out, _ := cmd.Output()
	return out
}

// parseBeadTime attempts to parse a timestamp from bd's JSON output.
// bd uses RFC3339 or similar formats.
func parseBeadTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unable to parse time: %s", s)
}
