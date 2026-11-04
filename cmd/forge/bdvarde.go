package main

import (
	"os"
	"path/filepath"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/vardebeads"
	"github.com/spf13/cobra"
)

// bdVardeCmd is what a bd command becomes when the Forge's beads go through Varde: the daemon runs it in place of bd, in
// the anvil's directory, and it answers as bd would. A write goes to Varde's bead tools (DevInfra-gbx.8); with --read,
// a read goes to beads_bd_read in the anvil's pool (DevInfra-rjb.6).
var bdVardeCmd = &cobra.Command{
	Use:          "bd-varde [--read] -- <bd arguments>",
	Short:        "Run one bd command through Varde (used by the daemon)",
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
		anvil, path := anvilForCwd(vardeBase)
		if vardeRead {
			pool, err := poolForCwd(anvil, path)
			if err != nil {
				return err
			}
			// bd's exit code is the answer (1 with the not-found JSON, say), so it leaves as this process's own.
			if code := vardebeads.RunRead(ctx, client, pool, args, cmd.OutOrStdout(), cmd.ErrOrStderr(), vardebeads.ReadBackoff); code != 0 {
				os.Exit(code)
			}
			return nil
		}
		return vardebeads.Run(ctx, client, args, session, anvil.BeadsParent, cmd.OutOrStdout())
	},
}

var (
	vardeBase string
	vardeRead bool
)

func init() {
	bdVardeCmd.Flags().StringVar(&vardeBase, "base", "", "The daemon's working directory, which relative anvil paths are read against")
	bdVardeCmd.Flags().BoolVar(&vardeRead, "read", false, "A bd read, sent to Varde's beads_bd_read in the anvil's pool")
}

// poolForCwd is the Varde pool of the anvil the working directory is in; outside every anvil, the pool of the
// directory itself, read from its .beads.
func poolForCwd(anvil config.AnvilConfig, path string) (string, error) {
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = wd
	}
	return vardebeads.ResolvePool(anvil.VardePool, path)
}

// anvilParentForCwd is the beads_parent of the anvil whose path is the working directory (or holds it).
func anvilParentForCwd(base string) string {
	anvil, _ := anvilForCwd(base)
	return anvil.BeadsParent
}

// anvilForCwd is the anvil whose path is the working directory (or holds it), with that path. A relative anvil path
// is read against base, the daemon's working directory, not this process's.
func anvilForCwd(base string) (config.AnvilConfig, string) {
	wd, err := os.Getwd()
	if err != nil || cfg == nil {
		return config.AnvilConfig{}, ""
	}
	if real, err := filepath.EvalSymlinks(wd); err == nil {
		wd = real
	}
	wd = filepath.Clean(wd)
	best, found, foundPath := -1, config.AnvilConfig{}, ""
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
			best, found, foundPath = len(path), anvil, path
		}
	}
	return found, foundPath
}

func startsDotDot(rel string) bool {
	return len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}
