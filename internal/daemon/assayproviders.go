package daemon

import (
	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/config"
)

// warnAssayProviderConflicts logs every anvil that sets both a legacy assay
// provider/model key and an assay stage_providers key, naming which one decides
// each pass. It runs at startup and on every config reload. (A multi-entry
// assay chain is no longer warned about: each pass fails over down its own
// chain on a rate limit, so the entries after the head do run.)
//
// A message is skipped only when the PREVIOUS call's config produced it too:
// the seen-set is rebuilt from the current conditions on every call, so an
// unchanged reload stays quiet, while a condition that goes away and later
// comes back (A -> B -> A) is in force again and is logged again.
func (d *Daemon) warnAssayProviderConflicts(cfg *config.Config) {
	if d.logger == nil {
		return
	}
	d.assayProviderWarnMu.Lock()
	defer d.assayProviderWarnMu.Unlock()

	prev := d.assayProviderWarned
	current := make(map[string]struct{})
	for _, c := range assay.LegacyStageConflicts(cfg) {
		msg := c.String()
		current[msg] = struct{}{}
		if _, seen := prev[msg]; seen {
			continue
		}
		d.logger.Warn("Assay provider keys overlap", "anvil", c.Anvil, "detail", msg)
	}
	d.assayProviderWarned = current
}
