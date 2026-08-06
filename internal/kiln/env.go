package kiln

import (
	"os"
	"sort"
	"strconv"
	"strings"
)

// PortEnvPrefix is the prefix of the per-service port variables every preview
// command receives (e.g. FORGE_PREVIEW_PORT_API).
const PortEnvPrefix = "FORGE_PREVIEW_PORT_"

// PreviewEnv is the context Kiln injects into every preview command (setup,
// teardown and each service). It uses the same FORGE_* vocabulary as pipeline
// hooks so a project's scripts can be written once for both.
type PreviewEnv struct {
	// PreviewID is the sanitized bead id — safe as a database name, an
	// identifier and an env value. See SanitizePreviewID.
	PreviewID string
	// BeadID is the raw bead id.
	BeadID string
	// Branch is the branch the preview was checked out from.
	Branch string
	// WorktreePath is the preview's detached checkout under <anvil>/.previews/.
	WorktreePath string
	// AnvilName / AnvilPath identify the repository this preview belongs to.
	AnvilName string
	AnvilPath string
	// Ports maps every service name in the manifest to its allocated port.
	// All of them are exposed to all services, so a client can be told where
	// its api listens without either side knowing the allocation in advance.
	Ports map[string]int
}

// Vars returns the FORGE_* variables as a map.
func (e PreviewEnv) Vars() map[string]string {
	vars := map[string]string{
		"FORGE_PREVIEW_ID":    e.PreviewID,
		"FORGE_BEAD_ID":       e.BeadID,
		"FORGE_BRANCH":        e.Branch,
		"FORGE_WORKTREE_PATH": e.WorktreePath,
		"FORGE_ANVIL_NAME":    e.AnvilName,
		"FORGE_ANVIL_PATH":    e.AnvilPath,
	}
	for name, port := range e.Ports {
		vars[PortEnvVar(name)] = strconv.Itoa(port)
	}
	return vars
}

// Environ returns the FORGE_* variables as sorted KEY=VALUE strings.
func (e PreviewEnv) Environ() []string {
	return flattenEnv(e.Vars())
}

// PortEnvVar returns the environment variable carrying a service's port, e.g.
// "api-gateway" → FORGE_PREVIEW_PORT_API_GATEWAY. Manifest validation rejects
// service names that would collide here.
func PortEnvVar(service string) string {
	return PortEnvPrefix + strings.ToUpper(strings.ReplaceAll(service, "-", "_"))
}

// BuildEnv returns the environment for one preview service: the daemon's own
// environment, then the service's expanded manifest env, then the FORGE_*
// context.
//
// The layering is deliberate. Manifest values override inherited ones (that is
// how a project points a service at its preview database), while the FORGE_*
// context is applied last and wins over both: those variables describe which
// preview this is, and a manifest must not be able to lie about that. Any
// FORGE_* variable inherited from the daemon's own environment is dropped for
// the same reason — a Forge daemon started from inside a worker shell would
// otherwise leak that worker's bead id into every preview command.
//
// base is the environment to inherit; nil means os.Environ().
func BuildEnv(base []string, env PreviewEnv, serviceEnv map[string]string) []string {
	if base == nil {
		base = os.Environ()
	}
	forgeVars := env.Vars()

	// Everything set below is removed from the inherited environment first, so
	// the result has one entry per key regardless of how os/exec dedupes.
	overridden := make(map[string]bool, len(forgeVars)+len(serviceEnv))
	for key := range serviceEnv {
		overridden[key] = true
	}
	for key := range forgeVars {
		overridden[key] = true
	}

	out := make([]string, 0, len(base)+len(forgeVars)+len(serviceEnv))
	for _, entry := range base {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if overridden[key] || strings.HasPrefix(key, "FORGE_") {
			continue
		}
		out = append(out, entry)
	}
	for _, key := range sortedKeys(serviceEnv) {
		if _, injected := forgeVars[key]; injected {
			continue // the context below is authoritative for this key
		}
		out = append(out, key+"="+serviceEnv[key])
	}
	return append(out, flattenEnv(forgeVars)...)
}

// SanitizePreviewID reduces a bead id to an identifier that is safe wherever a
// preview needs a name: a database name (`app_preview_<id>`), an env value, a
// URL path segment. Letters are lowercased, every other character folds to '_',
// runs of '_' collapse, and the result is guaranteed to start with a letter
// (SQL Server, PostgreSQL and MySQL all refuse identifiers starting with a
// digit unless they are quoted).
//
// This is intentionally *not* worktree.PreviewPath's sanitizer: that one
// preserves '.', '-' and case because it produces a directory name, which has
// the opposite constraints.
func SanitizePreviewID(beadID string) string {
	var b strings.Builder
	b.Grow(len(beadID) + 1)
	underscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(beadID)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			underscore = false
			continue
		}
		// Collapse any run of unsafe characters into a single separator, and
		// never open with one.
		if !underscore && b.Len() > 0 {
			b.WriteByte('_')
			underscore = true
		}
	}
	id := strings.TrimRight(b.String(), "_")
	if id == "" {
		return "preview"
	}
	if id[0] >= '0' && id[0] <= '9' {
		return "p_" + id
	}
	return id
}

// flattenEnv renders a map as sorted KEY=VALUE strings so a spawned command's
// environment is deterministic (and diffable in a test).
func flattenEnv(vars map[string]string) []string {
	out := make([]string, 0, len(vars))
	for key, value := range vars {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}
