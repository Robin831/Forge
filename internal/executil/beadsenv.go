package executil

import "strings"

// beadsCredentialPrefixes are the environment variables that let a process reach the beads database: bd's own
// (BEADS_DOLT_*, host, user and password among them) and Dolt's remote password. Agent sessions and the code they
// write never get them: a bead write goes through Varde, never straight to the database (DevInfra-gbx.8).
var beadsCredentialPrefixes = []string{"BEADS_DOLT_", "BEADS_PROXIED_SERVER_EXTERNAL_PASSWORD=", "DOLT_REMOTE_PASSWORD="}

// IsBeadsCredentialEnv reports whether one KEY=VALUE entry carries beads database access.
func IsBeadsCredentialEnv(entry string) bool {
	for _, prefix := range beadsCredentialPrefixes {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

// WithoutBeadsCredentials returns env without the entries IsBeadsCredentialEnv matches.
func WithoutBeadsCredentials(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if !IsBeadsCredentialEnv(entry) {
			out = append(out, entry)
		}
	}
	return out
}
