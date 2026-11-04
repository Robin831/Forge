package executil

import (
	"strings"

	"github.com/Robin831/Forge/internal/vardebeads"
)

// beadsCredentialKeys are the environment variables that point a process at the beads database: bd's own
// (BEADS_DOLT_*, host, user and password among them), its credentials file, and Dolt's remote password. Agent sessions
// and the code they run do not inherit them (DevInfra-gbx.8). This is hygiene, not a boundary: a process with the
// daemon's uid can still read the daemon's environment. Under Varde there is no database user at all (DevInfra-rjb.6):
// FORGE_VARDE_* name the Forge's own Varde key, which reads and writes beads for it, and that key is the boundary.
var beadsCredentialKeys = []string{"BEADS_DOLT_*", "BEADS_CREDENTIALS_FILE", "BEADS_PROXIED_SERVER_EXTERNAL_PASSWORD", "DOLT_REMOTE_PASSWORD", "FORGE_VARDE_*"}

// IsBeadsCredentialEnv reports whether one KEY=VALUE entry is one of them. Names compare without case, as on Windows.
func IsBeadsCredentialEnv(entry string) bool {
	key, _, _ := strings.Cut(entry, "=")
	key = strings.ToUpper(key)
	for _, want := range beadsCredentialKeys {
		if prefix, ok := strings.CutSuffix(want, "*"); ok {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		} else if key == want {
			return true
		}
	}
	return false
}

// KeepsBeadsCredentialsOut reports whether agent sessions are kept from the beads database: only when the Forge's own
// bead writes go through Varde. A forge on plain bd keeps 0.37.2's behaviour, its smiths filing beads with bd.
func KeepsBeadsCredentialsOut() bool { return vardebeads.Enabled() }

// WithoutBeadsCredentials returns env without the entries IsBeadsCredentialEnv matches, through Varde; else env as is.
func WithoutBeadsCredentials(env []string) []string {
	if !KeepsBeadsCredentialsOut() {
		return append([]string(nil), env...)
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if !IsBeadsCredentialEnv(entry) {
			out = append(out, entry)
		}
	}
	return out
}
