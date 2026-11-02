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
		// rootCtx carries SIGINT/SIGTERM: an interrupted daemon stops this write and its retries too.
		ctx := rootCtx
		if ctx == nil {
			ctx = cmd.Context()
		}
		return vardebeads.Run(ctx, client, args, session, anvilParentForCwd(vardeBase), cmd.OutOrStdout())
	},
}

var vardeBase string

func init() {
	bdVardeCmd.Flags().StringVar(&vardeBase, "base", "", "The daemon's working directory, which relative anvil paths are read against")
}

// anvilParentForCwd is the beads_parent of the anvil whose path is the working directory (or holds it). A relative
// anvil path is read against base, the daemon's working directory, not this process's.
func anvilParentForCwd(base string) string {
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
		path := anvil.Path
		if !filepath.IsAbs(path) && base != "" {
			path = filepath.Join(base, path)
		}
		path = filepath.Clean(path)
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
