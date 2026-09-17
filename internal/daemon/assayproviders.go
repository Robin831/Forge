package daemon

import (
	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/config"
)

// warnAssayProviderConflicts logs, once per distinct condition, every anvil
// that sets both a legacy assay provider/model key and an assay stage_providers
// key, naming which one decides each pass. It runs at startup and on every
// config reload; the rendered message is the dedupe key, so an unchanged
// condition stays quiet across reloads while an edit that changes what wins is
// reported again.
func (d *Daemon) warnAssayProviderConflicts(cfg *config.Config) {
	if d.logger == nil {
		return
	}
	for _, c := range assay.LegacyStageConflicts(cfg) {
		msg := c.String()
		if _, seen := d.assayProviderConflictsLogged.LoadOrStore(msg, struct{}{}); seen {
			continue
		}
		d.logger.Warn("Assay provider keys overlap", "anvil", c.Anvil, "detail", msg)
	}
}
