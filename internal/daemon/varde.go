package daemon

import (
	"sort"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/vardebeads"
)

// untaggedUnderVarde lists the anvils whose beads the Forge would claim without a Forge label of its own. Through Varde
// (DevInfra-gbx.8) its key writes only beads carrying one, so such claims are refused on every poll.
func untaggedUnderVarde(cfg *config.Config) []string {
	if cfg == nil || !vardebeads.Enabled() {
		return nil
	}
	var names []string
	for name, anvil := range cfg.Anvils {
		if anvil.AutoDispatch != "tagged" || anvil.AutoDispatchTag == "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (d *Daemon) warnVardeDispatch(cfg *config.Config) {
	for _, name := range untaggedUnderVarde(cfg) {
		d.logger.Warn("bead writes go through Varde, which lets the Forge claim only beads with its own Forge label; use auto_dispatch: tagged", "anvil", name)
	}
}
