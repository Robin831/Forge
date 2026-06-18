package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Robin831/Forge/internal/config"
	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// ConfigKeyInfo is the per-key metadata returned by GET /api/forge/config and
// consumed by the SettingsPage frontend. It is the documented data contract:
//
//   - Key           the forge.yaml settings key (e.g. "schematic_enabled")
//   - Value         the current effective boolean value (defaults applied for
//                   tri-state *bool keys that are unset)
//   - IsDefault     true when Value equals the documented default
//   - HotReloadable true only for keys the running daemon applies without a
//                   restart (see internal/hotreload)
//   - Area          UI grouping for the settings page
//   - Label         short human-readable name
//   - Description    one-line explanation sourced from config.go doc comments
type ConfigKeyInfo struct {
	Key           string `json:"key"`
	Value         bool   `json:"value"`
	IsDefault     bool   `json:"isDefault"`
	HotReloadable bool   `json:"hotReloadable"`
	Area          string `json:"area"`
	Label         string `json:"label"`
	Description   string `json:"description"`
}

// ConfigResponse is the GET /api/forge/config body. Keys is ordered (stable)
// so the frontend can render them deterministically. Anvils maps each
// configured anvil name to its per-anvil settings (the anvils.<name>.<key>
// contract); it is always present and serializes to "{}" when no anvils are
// configured. Tri-state *bool settings serialize to null when unset, meaning
// the anvil inherits the corresponding global setting or built-in default.
type ConfigResponse struct {
	Keys   []ConfigKeyInfo                 `json:"keys"`
	Anvils map[string]config.AnvilSettings `json:"anvils"`
}

// configKeyDef describes one managed boolean settings key: how to read its
// current value, its documented default, its metadata, and whether the daemon
// hot-reloads it. This is the single source of truth shared by the GET
// (serialisation) and PATCH (allowlist validation) handlers.
type configKeyDef struct {
	Key           string
	Area          string
	Label         string
	Description   string
	HotReloadable bool
	Default       bool
	// value resolves the effective boolean from settings, applying the
	// documented default for tri-state *bool keys that are nil/unset.
	value func(s config.SettingsConfig) bool
}

// managedConfigKeys is the allowlist of boolean settings exposed by the
// config API. Order is preserved in the GET response. HotReloadable is true
// only for keys the daemon applies live (cross-checked against
// internal/hotreload/hotreload.go: copilot_combined_smith_warden and
// smelter_enabled). The tri-state *bool keys (auto_merge_crucible_children,
// vulncheck_enabled, smelter_enabled) default to true and resolve nil via
// their config helpers.
var managedConfigKeys = []configKeyDef{
	{
		Key:         "schematic_enabled",
		Area:        "Pipeline",
		Label:       "Schematic pre-analysis",
		Description: "Enable the Schematic pre-worker globally. Beads exceeding the word threshold or carrying the \"decompose\" tag are analysed before Smith starts.",
		Default:     false,
		value:       func(s config.SettingsConfig) bool { return s.SchematicEnabled },
	},
	{
		Key:         "go_race_detection",
		Area:        "Temper",
		Label:       "Go race detection",
		Description: "Run the Go race detector (-race) as a separate temper step globally. Per-anvil settings override this.",
		Default:     false,
		value:       func(s config.SettingsConfig) bool { return s.GoRaceDetection },
	},
	{
		Key:         "auto_learn_rules",
		Area:        "Warden",
		Label:       "Auto-learn Warden rules",
		Description: "Automatically learn Warden review rules from Copilot comments when a PR is merged.",
		Default:     false,
		value:       func(s config.SettingsConfig) bool { return s.AutoLearnRules },
	},
	{
		Key:         "crucible_enabled",
		Area:        "Crucible",
		Label:       "Crucible orchestration",
		Description: "Enable automatic Crucible orchestration for parent beads that have children (blocks other beads).",
		Default:     false,
		value:       func(s config.SettingsConfig) bool { return s.CrucibleEnabled },
	},
	{
		Key:         "auto_merge_crucible_children",
		Area:        "Crucible",
		Label:       "Auto-merge Crucible children",
		Description: "Automatically merge (squash) child PRs targeting a Crucible feature branch after the pipeline succeeds. Defaults to true.",
		Default:     true,
		value:       func(s config.SettingsConfig) bool { return s.IsAutoMergeCrucibleChildren() },
	},
	{
		Key:         "copilot_skip_warden_small_diffs",
		Area:        "Copilot",
		Label:       "Skip Warden on small diffs",
		Description: "Automatically skip Warden for small, low-risk diffs when the primary provider is Copilot. Saves one premium request for trivial changes.",
		Default:     false,
		value:       func(s config.SettingsConfig) bool { return s.CopilotSkipWardenSmallDiffs },
	},
	{
		Key:         "copilot_batch_ci_fixes",
		Area:        "Copilot",
		Label:       "Batch CI fixes",
		Description: "Batch multiple CI failures into a single Smith invocation when the provider is Copilot. Saves premium requests on PRs with multiple failing checks.",
		Default:     false,
		value:       func(s config.SettingsConfig) bool { return s.CopilotBatchCIFixes },
	},
	{
		Key:         "copilot_batch_review_fixes",
		Area:        "Copilot",
		Label:       "Batch review fixes",
		Description: "Batch multiple review comments into a single Smith invocation when the provider is Copilot. Saves premium requests on PRs with multiple review comments.",
		Default:     false,
		value:       func(s config.SettingsConfig) bool { return s.CopilotBatchReviewFixes },
	},
	{
		Key:         "warden_full_rereview",
		Area:        "Warden",
		Label:       "Full Warden re-review",
		Description: "Force the Warden to do a full independent review on every iteration instead of a focused re-review that only checks whether prior feedback was addressed.",
		Default:     false,
		value:       func(s config.SettingsConfig) bool { return s.WardenFullRereview },
	},
	{
		Key:           "copilot_combined_smith_warden",
		Area:          "Copilot",
		Label:         "Combined Smith+Warden",
		Description:   "Embed Warden review criteria into the Smith prompt so Smith self-reviews its own diff, eliminating the separate Warden request (Copilot only, high risk).",
		Default:       false,
		HotReloadable: true,
		value:         func(s config.SettingsConfig) bool { return s.CopilotCombinedSmithWarden },
	},
	{
		Key:         "vulncheck_enabled",
		Area:        "Vulncheck",
		Label:       "Vulnerability scanning",
		Description: "Enable vulnerability scanning with govulncheck. When false, scheduled scanning and \"forge scan\" are disabled. Defaults to true.",
		Default:     true,
		value:       func(s config.SettingsConfig) bool { return s.IsVulncheckEnabled() },
	},
	{
		Key:           "smelter_enabled",
		Area:          "Smelter",
		Label:         "Smelter background process",
		Description:   "Enable the Smelter background process, which batches pending Warden rules into PRs on a schedule. Defaults to true.",
		Default:       true,
		HotReloadable: true,
		value:         func(s config.SettingsConfig) bool { return s.IsSmelterEnabled() },
	},
}

// configKeyByName indexes managedConfigKeys for O(1) allowlist validation in
// the PATCH handler.
var configKeyByName = func() map[string]configKeyDef {
	m := make(map[string]configKeyDef, len(managedConfigKeys))
	for _, d := range managedConfigKeys {
		m[d.Key] = d
	}
	return m
}()

// loadConfig reads the current forge config. Tests override s.configLoader to
// point at a fixture; production uses s.configFile (the daemon's --config
// path) so the web layer reads the same file the daemon hot-reloads.
func (s *Server) loadConfig() (*config.Config, error) {
	if s.configLoader != nil {
		return s.configLoader()
	}
	return config.Load(s.configFile)
}

// configWritePath resolves the file the PATCH handler edits. Tests override
// s.configPath; production uses s.configFile (the daemon's --config path) so
// edits land in the same file the daemon hot-reloads.
func (s *Server) configWritePath() string {
	if s.configPath != nil {
		return s.configPath()
	}
	if s.configFile != "" {
		return s.configFile
	}
	return defaultConfigWritePath()
}

// defaultConfigWritePath returns the path the daemon should persist config
// edits to. It prefers an existing resolved config file so edits land where
// the daemon already reads from; when none exists it falls back to the
// documented ~/.forge/config.yaml so a fresh install gets a sensible file.
func defaultConfigWritePath() string {
	if path := config.ConfigFilePath(""); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".forge", "config.yaml")
}

// handleForgeConfigGet returns the managed boolean settings with per-key
// metadata. GET /api/forge/config.
func (s *Server) handleForgeConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load config: "+err.Error())
		return
	}
	resp := ConfigResponse{
		Keys:   make([]ConfigKeyInfo, 0, len(managedConfigKeys)),
		Anvils: cfg.AnvilSettingsMap(),
	}
	for _, d := range managedConfigKeys {
		val := d.value(cfg.Settings)
		resp.Keys = append(resp.Keys, ConfigKeyInfo{
			Key:           d.Key,
			Value:         val,
			IsDefault:     val == d.Default,
			HotReloadable: d.HotReloadable,
			Area:          d.Area,
			Label:         d.Label,
			Description:   d.Description,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleForgeConfigPatch accepts a map of boolean key->value, validates every
// key against the allowlist (all-or-nothing), persists the changes via a YAML
// node-tree edit that preserves comments and unrelated keys, and returns the
// freshly re-read config (same shape as GET). PATCH /api/forge/config.
func (s *Server) handleForgeConfigPatch(w http.ResponseWriter, r *http.Request) {
	var req map[string]bool
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if len(req) == 0 {
		writeError(w, http.StatusBadRequest, "no config keys provided")
		return
	}

	// All-or-nothing: validate every key before writing anything.
	var unknown []string
	for k := range req {
		if _, ok := configKeyByName[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		writeError(w, http.StatusBadRequest, "unknown config key(s): "+strings.Join(unknown, ", "))
		return
	}

	path := s.configWritePath()
	if path == "" {
		writeError(w, http.StatusInternalServerError, "could not resolve config file path")
		return
	}
	if err := applyConfigPatch(path, req); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist config: "+err.Error())
		return
	}

	// Return the re-read config so the caller sees the persisted result. The
	// loader reads from disk, so it reflects the edit we just wrote.
	s.handleForgeConfigGet(w, r)
}

// applies* are the hot-reload coverage values reported per key by the
// per-anvil PATCH handler. "instant" keys are applied by the running daemon
// without a restart (see internal/hotreload); "next_run" keys are read fresh
// on the next dispatch/run so they take effect for the next bead, not for
// work already in flight.
const (
	appliesInstant = "instant"
	appliesNextRun = "next_run"
)

// anvilKeyDef describes one allowlisted per-anvil settings key: whether it is
// tri-state (a *bool that can be cleared to inherit) or a plain bool, and
// whether the daemon hot-reloads it instantly. This is the single source of
// truth for the anvils.<name>.<key> contract shared by validation and the
// hot-reload coverage reported in the response.
type anvilKeyDef struct {
	Key string
	// TriState marks a *bool field where JSON null clears the override
	// (anvil inherits the global/default). Plain bools reject null.
	TriState bool
	// Instant marks keys the daemon applies live via internal/hotreload.
	// Only auto_merge is instant; the rest apply on the next dispatch/run.
	Instant bool
}

// managedAnvilKeys is the allowlist of the eight per-anvil settings exposed by
// the config API, mirroring config.AnvilSettings. Order is preserved in the
// response's per-key coverage list. Only auto_merge is hot-reloadable (see
// internal/hotreload/hotreload.go, which diffs anvil auto_merge live); the
// others are read on the next dispatch/run.
var managedAnvilKeys = []anvilKeyDef{
	{Key: "auto_merge", TriState: false, Instant: true},
	{Key: "schematic_enabled", TriState: true},
	{Key: "golangci_lint", TriState: true},
	{Key: "go_race_detection", TriState: true},
	{Key: "depcheck_enabled", TriState: true},
	{Key: "questgiver_enabled", TriState: true},
	{Key: "wicket_enabled", TriState: true},
	{Key: "wicket_auto_dispatch", TriState: false},
}

// anvilKeyByName indexes managedAnvilKeys for O(1) allowlist validation.
var anvilKeyByName = func() map[string]anvilKeyDef {
	m := make(map[string]anvilKeyDef, len(managedAnvilKeys))
	for _, d := range managedAnvilKeys {
		m[d.Key] = d
	}
	return m
}()

// AnvilKeyApplied reports, per edited key, whether the change takes effect
// instantly (hot-reloaded) or on the next dispatch/run, and whether the edit
// cleared the override (tri-state *bool reset to inherit) rather than setting
// an explicit value. The frontend uses Applies to show the "applies on next
// run" note (consistent with Forge-3hvb).
type AnvilKeyApplied struct {
	Key     string `json:"key"`
	Applies string `json:"applies"`
	Cleared bool   `json:"cleared"`
}

// AnvilConfigPatchResponse is the PATCH /api/forge/config/anvils/{name} body.
// Settings is the re-read projection of the anvil after the edit (same shape
// as the per-anvil entries in GET /api/forge/config), so the caller sees the
// persisted result without a second request. Applied lists per-key hot-reload
// coverage for exactly the keys touched by this request.
type AnvilConfigPatchResponse struct {
	Anvil    string               `json:"anvil"`
	Settings config.AnvilSettings `json:"settings"`
	Applied  []AnvilKeyApplied    `json:"applied"`
}

// handleForgeAnvilConfigPatch accepts a map of per-anvil boolean key->value
// for a single anvil, validates the anvil name (404 if unknown) and every key
// (400 if unknown), distinguishes tri-state *bool clears (JSON null → remove
// the key so the anvil inherits) from explicit true/false, persists the
// changes via a YAML node-tree edit that preserves comments and unrelated
// keys, and returns the re-read anvil settings plus per-key hot-reload
// coverage. PATCH /api/forge/config/anvils/{name}.
func (s *Server) handleForgeAnvilConfigPatch(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "anvil name is required")
		return
	}

	// Decode into raw messages so we can distinguish "key present with null"
	// (clear/inherit) from an explicit true/false, and reject malformed values.
	var req map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if len(req) == 0 {
		writeError(w, http.StatusBadRequest, "no config keys provided")
		return
	}

	cfg, err := s.loadConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load config: "+err.Error())
		return
	}
	if _, ok := cfg.Anvils[name]; !ok {
		writeError(w, http.StatusNotFound, "unknown anvil: "+name)
		return
	}

	// All-or-nothing: validate every key and value before writing anything.
	var unknown []string
	sets := map[string]bool{}
	var clears []string
	applied := make([]AnvilKeyApplied, 0, len(req))
	for k, raw := range req {
		def, ok := anvilKeyByName[k]
		if !ok {
			unknown = append(unknown, k)
			continue
		}
		switch strings.TrimSpace(string(raw)) {
		case "true":
			sets[k] = true
		case "false":
			sets[k] = false
		case "null":
			if !def.TriState {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("key %q cannot be cleared (null); it is a plain boolean, send true or false", k))
				return
			}
			clears = append(clears, k)
		default:
			allowed := "true or false"
			if def.TriState {
				allowed = "true, false, or null"
			}
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("invalid value for key %q: expected %s", k, allowed))
			return
		}
		appliesVal := appliesNextRun
		if def.Instant {
			appliesVal = appliesInstant
		}
		applied = append(applied, AnvilKeyApplied{
			Key:     k,
			Applies: appliesVal,
			Cleared: strings.TrimSpace(string(raw)) == "null",
		})
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		writeError(w, http.StatusBadRequest, "unknown config key(s): "+strings.Join(unknown, ", "))
		return
	}

	path := s.configWritePath()
	if path == "" {
		writeError(w, http.StatusInternalServerError, "could not resolve config file path")
		return
	}
	if err := applyAnvilConfigPatch(path, name, sets, clears); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist config: "+err.Error())
		return
	}

	// Re-read so the caller sees the persisted result (defaults/inherit
	// applied) rather than echoing the request back.
	reread, err := s.loadConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reload config: "+err.Error())
		return
	}
	// Order the per-key coverage to match the allowlist for a stable response.
	sort.SliceStable(applied, func(i, j int) bool {
		return anvilKeyOrder(applied[i].Key) < anvilKeyOrder(applied[j].Key)
	})
	writeJSON(w, http.StatusOK, AnvilConfigPatchResponse{
		Anvil:    name,
		Settings: reread.AnvilSettingsMap()[name],
		Applied:  applied,
	})
}

// anvilKeyOrder returns the index of key within managedAnvilKeys, or a large
// sentinel when absent, so the response coverage list renders deterministically.
func anvilKeyOrder(key string) int {
	for i, d := range managedAnvilKeys {
		if d.Key == key {
			return i
		}
	}
	return len(managedAnvilKeys)
}

// applyAnvilConfigPatch edits the YAML document at path in place, setting each
// anvils.<name>.<key> in sets to its boolean and removing each key in clears
// (so the anvil inherits the global/default). Like applyConfigPatch it works
// on a yaml.Node tree to preserve comments and unrelated keys, then writes the
// result back atomically (temp file + rename) so the fsnotify watcher fires
// once on a complete file.
func applyAnvilConfigPatch(path, anvil string, sets map[string]bool, clears []string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}

	var doc yaml.Node
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		root = &yaml.Node{Kind: yaml.MappingNode}
		doc.Content[0] = root
	}

	anvils := mappingValueNode(root, "anvils")
	if anvils == nil || anvils.Kind != yaml.MappingNode {
		anvils = &yaml.Node{Kind: yaml.MappingNode}
		setMappingChild(root, "anvils", anvils)
	}
	anvilNode := mappingValueNode(anvils, anvil)
	if anvilNode == nil || anvilNode.Kind != yaml.MappingNode {
		anvilNode = &yaml.Node{Kind: yaml.MappingNode}
		setMappingChild(anvils, anvil, anvilNode)
	}

	// Apply sets in a stable order so the diff is deterministic.
	keys := make([]string, 0, len(sets))
	for k := range sets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		setBoolNode(anvilNode, key, sets[key])
	}
	sort.Strings(clears)
	for _, key := range clears {
		removeMappingChild(anvilNode, key)
	}

	out, err := marshalNode(&doc)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return writeFileAtomic(path, out)
}

// applyConfigPatch edits the YAML document at path in place, setting each
// settings.<key> to the given boolean. It parses into a yaml.Node tree
// (rather than marshalling a full Config) so comments and unrelated keys are
// preserved, locates or creates the top-level `settings` mapping and each
// target scalar, then writes the result back atomically (temp file + rename)
// so the fsnotify watcher fires once on a complete file.
func applyConfigPatch(path string, patch map[string]bool) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}

	var doc yaml.Node
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	// Normalise to a DocumentNode wrapping a MappingNode root so we always
	// have somewhere to attach keys, even for an empty/new file.
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		// e.g. the file was a bare scalar/null — replace with an empty map.
		root = &yaml.Node{Kind: yaml.MappingNode}
		doc.Content[0] = root
	}

	settings := mappingValueNode(root, "settings")
	if settings == nil || settings.Kind != yaml.MappingNode {
		settings = &yaml.Node{Kind: yaml.MappingNode}
		setMappingChild(root, "settings", settings)
	}

	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		setBoolNode(settings, key, patch[key])
	}

	out, err := marshalNode(&doc)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return writeFileAtomic(path, out)
}

// mappingValueNode returns the value node for key in mapping m, or nil when
// the key is absent. m.Content holds alternating key/value nodes.
func mappingValueNode(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setMappingChild sets key -> valNode in mapping m, replacing an existing
// value node or appending a new key/value pair.
func setMappingChild(m *yaml.Node, key string, valNode *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = valNode
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		valNode)
}

// removeMappingChild deletes the key/value pair for key from mapping m,
// returning true when a pair was removed. Removing a tri-state *bool key
// (rather than writing null) lets the anvil inherit the global/default and
// keeps the persisted file minimal.
func removeMappingChild(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

// setBoolNode sets m[key] to a boolean scalar, editing the existing scalar in
// place (preserving its surrounding comments) or appending a new pair.
func setBoolNode(m *yaml.Node, key string, val bool) {
	valStr := "false"
	if val {
		valStr = "true"
	}
	if vn := mappingValueNode(m, key); vn != nil {
		vn.Kind = yaml.ScalarNode
		vn.Tag = "!!bool"
		vn.Style = 0
		vn.Value = valStr
		// Drop any child content a previous non-scalar value may have held.
		vn.Content = nil
		return
	}
	setMappingChild(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: valStr})
}

// marshalNode renders a yaml.Node tree with 2-space indentation to match the
// project's existing config files.
func marshalNode(n *yaml.Node) ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, so a concurrent fsnotify watcher only ever observes a
// complete file. The parent directory is created when missing.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	// Preserve the existing file mode where possible; default to 0644.
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".forge-config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	cleanup = false
	return nil
}
