package vardebeads

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// KnownPools are Varde's bead pools by name, with the id prefix of each (varde/values.workspace.yaml, beads.pools).
// Only the fallback below reads them; an anvil's varde_pool names its pool outright.
var KnownPools = map[string]string{"munin": "Fhi.Metadata", "heimdall": "Heimdall", "devinfra": "DevInfra"}

// ResolvePool names the Varde pool an anvil's beads live in: explicit (the anvil's varde_pool) when set; otherwise
// read from the anvil's own git-tracked bd files, following .beads/redirect: the issue-prefix in config.yaml, the id
// prefix of issues.jsonl, then the database at the end of config.yaml's sync.remote, matched against KnownPools.
func ResolvePool(explicit, anvilPath string) (string, error) {
	if p := strings.TrimSpace(explicit); p != "" {
		return p, nil
	}
	if anvilPath == "" {
		return "", fmt.Errorf("no anvil path to find a Varde pool for; set varde_pool on the anvil")
	}
	dir := beadsDir(anvilPath)
	config, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if prefix := yamlValue(config, "issue-prefix"); prefix != "" {
		if pool := poolForID(prefix + "-"); pool != "" {
			return pool, nil
		}
	}
	if pool := poolForID(firstJSONLID(filepath.Join(dir, "issues.jsonl"))); pool != "" {
		return pool, nil
	}
	if remote := yamlValue(config, "sync.remote"); remote != "" {
		db := remote[strings.LastIndex(remote, "/")+1:]
		if _, ok := KnownPools[db]; ok {
			return db, nil
		}
	}
	return "", fmt.Errorf("no Varde pool for the anvil at %s: set varde_pool on it in forge.yaml (one of %s); %s has no issue-prefix, issues.jsonl or sync.remote that names one", anvilPath, strings.Join(poolNames(), ", "), dir)
}

// beadsDir is where bd finds an anvil's beads: its .beads, or the one a hand-written .beads/redirect points at
// (relative to the anvil's root), as the explorer anvil shares Munin's.
func beadsDir(anvilPath string) string {
	dir := filepath.Join(anvilPath, ".beads")
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err == nil {
		return dir
	}
	raw, err := os.ReadFile(filepath.Join(dir, "redirect"))
	if err != nil {
		return dir
	}
	target := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if line, _, _ = strings.Cut(line, "#"); strings.TrimSpace(line) != "" {
			target = strings.TrimSpace(line)
		}
	}
	if target == "" {
		return dir
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(anvilPath, target)
	}
	return filepath.Clean(target)
}

var yamlLine = regexp.MustCompile(`^([A-Za-z0-9_.-]+):\s*(.*)$`)

// yamlValue reads one top-level scalar from bd's flat config.yaml; enough for a key bd writes as `key: "value"`.
func yamlValue(raw []byte, key string) string {
	for _, line := range strings.Split(string(raw), "\n") {
		m := yamlLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil || m[1] != key {
			continue
		}
		v := m[2]
		if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, `'`) {
			if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
				return v[1 : end+1]
			}
		}
		v, _, _ = strings.Cut(v, " #")
		return strings.TrimSpace(v)
	}
	return ""
}

func firstJSONLID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for i := 0; i < 20 && scanner.Scan(); i++ {
		var rec struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(scanner.Bytes(), &rec) == nil && rec.ID != "" {
			return rec.ID
		}
	}
	return ""
}

func poolForID(id string) string {
	for name, prefix := range KnownPools {
		if strings.HasPrefix(id, prefix+"-") {
			return name
		}
	}
	return ""
}

func poolNames() []string {
	names := make([]string, 0, len(KnownPools))
	for name := range KnownPools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
