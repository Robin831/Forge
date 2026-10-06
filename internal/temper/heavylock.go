package temper

import (
	"path/filepath"
	"strings"
)

// heavyLockEnv names the lock file Temper shares with the deployment's dotnet wrapper. Unset, as
// on a laptop, Temper takes no lock at all.
const heavyLockEnv = "FORGE_DOTNET_LOCK"

// heavyLockHeldEnv tells the wrapper inside a step that Temper already holds the lock, so it must
// not wait on it again.
const heavyLockHeldEnv = "FORGE_DOTNET_LOCK_HELD"

// heavyDotnetSubcommands are the dotnet commands that compile or host tests, the ones two of which
// at once OOM-kill a 6Gi forge container. The skybert chart's dotnet wrapper locks the same set.
var heavyDotnetSubcommands = map[string]bool{
	"build": true, "test": true, "format": true, "publish": true,
	"pack": true, "msbuild": true, "vstest": true,
}

// isHeavyDotnetStep reports whether step runs one of heavyDotnetSubcommands: the command is
// dotnet and its first non-option argument is one of them.
func isHeavyDotnetStep(step Step) bool {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(step.Command)), ".exe")
	if name != "dotnet" {
		return false
	}
	for _, arg := range step.Args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return heavyDotnetSubcommands[strings.ToLower(arg)]
	}
	return false
}
