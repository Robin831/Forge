package daemon

import (
	"github.com/Robin831/Forge/internal/cost"
)

// recordStageCost persists one completed provider session's token usage into
// the three cost tables, for a stage the daemon runs itself rather than through
// the pipeline: the Crucible's schematic check and an Assay review. It is the
// daemon-side twin of the pipeline's own recordStageCost, and both go through
// cost.Record, so every stage that spends money lands in the same three tables
// with the same prompt-cache columns.
//
// stage names the caller in the failure log. A zero usage writes nothing.
func (d *Daemon) recordStageCost(stage, provName, beadID, anvil string, u cost.Usage) {
	if d.db == nil {
		return
	}
	if err := cost.Record(d.db, provName, beadID, anvil, u); err != nil {
		d.logger.Warn("cost write failed", "stage", stage, "bead", beadID, "anvil", anvil, "error", err)
	}
}
