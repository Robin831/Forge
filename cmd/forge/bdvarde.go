package main

import (
	"os"
	"path/filepath"

	"github.com/Robin831/Forge/internal/vardebeads"
	"github.com/spf13/cobra"
)

// bdVardeCmd is what a bd write becomes when the Forge's bead writes go through Varde (DevInfra-gbx.8): the daemon
// runs it in place of bd, in the anvil's directory, and it answers as bd would.
var bdVardeCmd = &cobra.Command{
	Use:          "bd-varde -- <bd write arguments>",
	Short:        "Run one bd write through Varde (used by the daemon)",
	Hidden:       true,
	SilenceUsage: true,
	Args:         cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := vardebeads.NewClientFromEnv()
		if err != nil {
			return err
		}
		session := os.Getenv(vardebeads.EnvSession)
		if session == "" {
			session = "Forge"
		}
		return vardebeads.Run(cmd.Context(), client, args, session, anvilParentForCwd(), cmd.OutOrStdout())
	},
}

// anvilParentForCwd is the beads_parent of the anvil whose path is the working directory (or holds it).
func anvilParentForCwd() string {
	wd, err := os.Getwd()
	if err != nil || cfg == nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(wd); err == nil {
		wd = real
	}
	wd = filepath.Clean(wd)
	best, parent := -1, ""
	for _, anvil := range cfg.Anvils {
		if anvil.Path == "" {
			continue
		}
		path := filepath.Clean(anvil.Path)
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = real
		}
		if rel, err := filepath.Rel(path, wd); err == nil && rel != ".." && !startsDotDot(rel) && len(path) > best {
			best, parent = len(path), anvil.BeadsParent
		}
	}
	return parent
}

func startsDotDot(rel string) bool {
	return len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}
