package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// Config value types (the "type" field in ConfigKeyInfo / AnvilKeyInfo). The
// frontend renders a control per type: bool→switch, int/float→number input,
// enum→dropdown, string→text input, string_list→string-array editor,
// provider_map→per-stage provider editor, duration→duration input. The
// string_list/provider_map/duration names are a contract shared verbatim with
// the frontend (Forge-vo5a), which dispatches its rendering on these strings.
const (
	typeBool        = "bool"
	typeInt         = "int"
	typeFloat       = "float"
	typeEnum        = "enum"
	typeString      = "string"
	typeStringList  = "string_list"
	typeProviderMap = "provider_map"
	typeDuration    = "duration"
)

// providerStages is the allowed key set for provider_map values: the pipeline
// stages that accept their own provider chain. It doubles as the Options hint
// in the schema metadata so the frontend can offer exactly these stage keys.
var providerStages = []string{"smith", "warden", "schematic", "cifix", "reviewfix"}

// providerStageSet indexes providerStages for O(1) membership checks during
// provider_map validation.
var providerStageSet = func() map[string]bool {
	m := make(map[string]bool, len(providerStages))
	for _, s := range providerStages {
		m[s] = true
	}
	return m
}()

// ConfigKeyInfo is the per-key metadata returned by GET /api/forge/config and
// consumed by the SettingsPage frontend. It is the documented data contract.
// Value is typed per Type: a JSON boolean, number, or string. Options is the
// allowed set for enum keys; Min/Max bound numeric keys; Unit is an optional
// display suffix (e.g. "USD"). HotReloadable is true only for keys the running
// daemon applies without a restart (see internal/hotreload); the frontend shows
// an "applies on next run" note for the rest.
type ConfigKeyInfo struct {
	Key           string   `json:"key"`
	Type          string   `json:"type"`
	Value         any      `json:"value"`
	IsDefault     bool     `json:"isDefault"`
	HotReloadable bool     `json:"hotReloadable"`
	Area          string   `json:"area"`
	Label         string   `json:"label"`
	Description   string   `json:"description"`
	Options       []string `json:"options,omitempty"`
	Min           *float64 `json:"min,omitempty"`
	Max           *float64 `json:"max,omitempty"`
	Unit          string   `json:"unit,omitempty"`
}

// ConfigResponse is the GET /api/forge/config body. Keys is ordered (stable)
// so the frontend can render them deterministically. Anvils maps each
// configured anvil name to its per-anvil settings (the anvils.<name>.<key>
// contract); it is always present and serializes to "{}" when no anvils are
// configured. Tri-state *bool settings serialize to null when unset, meaning
// the anvil inherits the corresponding global setting or built-in default.
type ConfigResponse struct {
	Keys      []ConfigKeyInfo                 `json:"keys"`
	AnvilKeys []AnvilKeyInfo                  `json:"anvilKeys"`
	Anvils    map[string]config.AnvilSettings `json:"anvils"`
}

// AnvilKeyInfo is the per-anvil key schema returned by GET /api/forge/config so
// the frontend renders per-anvil controls from metadata rather than a hardcoded
// list. TriState marks *bool keys where a null value clears the override (the
// anvil inherits the global/default). Type/Options/Min/Max mirror ConfigKeyInfo.
type AnvilKeyInfo struct {
	Key           string   `json:"key"`
	Type          string   `json:"type"`
	TriState      bool     `json:"triState"`
	HotReloadable bool     `json:"hotReloadable"`
	Label         string   `json:"label"`
	Description   string   `json:"description"`
	Options       []string `json:"options,omitempty"`
	Min           *float64 `json:"min,omitempty"`
	Max           *float64 `json:"max,omitempty"`
}

// fptr returns a pointer to f, for the optional Min/Max numeric bounds.
func fptr(f float64) *float64 { return &f }

// configKeyDef describes one managed settings key: its type, how to read its
// current value, its documented default, its metadata, numeric bounds / enum
// options, and whether the daemon hot-reloads it. This is the single source of
// truth shared by the GET (serialisation) and PATCH (validation) handlers.
type configKeyDef struct {
	Key           string
	Type          string
	Area          string
	Label         string
	Description   string
	HotReloadable bool
	Options       []string
	Min           *float64
	Max           *float64
	Unit          string
	// Default is the documented default value (typed: bool/int/float/string),
	// used to compute IsDefault.
	Default any
	// value resolves the effective typed value from settings, applying the
	// documented default for tri-state *bool keys that are nil/unset.
	value func(s config.SettingsConfig) any
}

// durationKey builds a configKeyDef for a duration setting. The current value
// is exposed as a Go duration string (e.g. "5m0s") so the frontend renders it
// in the duration control, and def is the documented default in the same
// canonical string form (used to compute IsDefault).
func durationKey(key, area, label, desc, def string, read func(config.SettingsConfig) time.Duration) configKeyDef {
	return configKeyDef{
		Key:         key,
		Type:        typeDuration,
		Area:        area,
		Label:       label,
		Description: desc,
		Default:     def,
		value:       func(s config.SettingsConfig) any { return read(s).String() },
	}
}

// managedConfigKeys is the allowlist of settings exposed by the config API.
// Includes booleans, scalars (int, float, string), composite types
// (string_list, provider_map), and durations. Order is preserved in the GET
// response. HotReloadable is true only for keys the daemon applies live
// (cross-checked against internal/hotreload/hotreload.go). The tri-state
// *bool keys (auto_merge_crucible_children, vulncheck_enabled,
// smelter_enabled) default to true and resolve nil via their config helpers.
var managedConfigKeys = []configKeyDef{
	{
		Key:         "schematic_enabled",
		Area:        "Pipeline",
		Label:       "Schematic pre-analysis",
		Description: "Enable the Schematic pre-worker globally. Beads exceeding the word threshold or carrying the \"decompose\" tag are analysed before Smith starts.",
		Default:     false,
		Type:        typeBool,
		value:       func(s config.SettingsConfig) any { return s.SchematicEnabled },
	},
	{
		Key:         "go_race_detection",
		Area:        "Temper",
		Label:       "Go race detection",
		Description: "Run the Go race detector (-race) as a separate temper step globally. Per-anvil settings override this.",
		Default:     false,
		Type:        typeBool,
		value:       func(s config.SettingsConfig) any { return s.GoRaceDetection },
	},
	{
		Key:         "auto_learn_rules",
		Area:        "Warden",
		Label:       "Auto-learn Warden rules",
		Description: "Automatically learn Warden review rules from Copilot comments when a PR is merged.",
		Default:     false,
		Type:        typeBool,
		value:       func(s config.SettingsConfig) any { return s.AutoLearnRules },
	},
	{
		Key:         "crucible_enabled",
		Area:        "Crucible",
		Label:       "Crucible orchestration",
		Description: "Enable automatic Crucible orchestration for parent beads that have children (blocks other beads).",
		Default:     false,
		Type:        typeBool,
		value:       func(s config.SettingsConfig) any { return s.CrucibleEnabled },
	},
	{
		Key:         "auto_merge_crucible_children",
		Area:        "Crucible",
		Label:       "Auto-merge Crucible children",
		Description: "Automatically merge (squash) child PRs targeting a Crucible feature branch after the pipeline succeeds. Defaults to true.",
		Default:     true,
		Type:        typeBool,
		value:       func(s config.SettingsConfig) any { return s.IsAutoMergeCrucibleChildren() },
	},
	{
		Key:         "copilot_skip_warden_small_diffs",
		Area:        "Copilot",
		Label:       "Skip Warden on small diffs",
		Description: "Automatically skip Warden for small, low-risk diffs when the primary provider is Copilot. Saves one premium request for trivial changes.",
		Default:     false,
		Type:        typeBool,
		value:       func(s config.SettingsConfig) any { return s.CopilotSkipWardenSmallDiffs },
	},
	{
		Key:         "copilot_batch_ci_fixes",
		Area:        "Copilot",
		Label:       "Batch CI fixes",
		Description: "Batch multiple CI failures into a single Smith invocation when the provider is Copilot. Saves premium requests on PRs with multiple failing checks.",
		Default:     false,
		Type:        typeBool,
		value:       func(s config.SettingsConfig) any { return s.CopilotBatchCIFixes },
	},
	{
		Key:         "copilot_batch_review_fixes",
		Area:        "Copilot",
		Label:       "Batch review fixes",
		Description: "Batch multiple review comments into a single Smith invocation when the provider is Copilot. Saves premium requests on PRs with multiple review comments.",
		Default:     false,
		Type:        typeBool,
		value:       func(s config.SettingsConfig) any { return s.CopilotBatchReviewFixes },
	},
	{
		Key:         "warden_full_rereview",
		Area:        "Warden",
		Label:       "Full Warden re-review",
		Description: "Force the Warden to do a full independent review on every iteration instead of a focused re-review that only checks whether prior feedback was addressed.",
		Default:     false,
		Type:        typeBool,
		value:       func(s config.SettingsConfig) any { return s.WardenFullRereview },
	},
	{
		Key:           "copilot_combined_smith_warden",
		Area:          "Copilot",
		Label:         "Combined Smith+Warden",
		Description:   "Embed Warden review criteria into the Smith prompt so Smith self-reviews its own diff, eliminating the separate Warden request (Copilot only, high risk).",
		Default:       false,
		HotReloadable: true,
		Type:          typeBool,
		value:         func(s config.SettingsConfig) any { return s.CopilotCombinedSmithWarden },
	},
	{
		Key:         "vulncheck_enabled",
		Area:        "Vulncheck",
		Label:       "Vulnerability scanning",
		Description: "Enable vulnerability scanning with govulncheck. When false, scheduled scanning and \"forge scan\" are disabled. Defaults to true.",
		Default:     true,
		Type:        typeBool,
		value:       func(s config.SettingsConfig) any { return s.IsVulncheckEnabled() },
	},
	{
		Key:           "smelter_enabled",
		Area:          "Smelter",
		Label:         "Smelter background process",
		Description:   "Enable the Smelter background process, which batches pending Warden rules into PRs on a schedule. Defaults to true.",
		Default:       true,
		HotReloadable: true,
		Type:          typeBool,
		value:         func(s config.SettingsConfig) any { return s.IsSmelterEnabled() },
	},

	// --- Non-boolean scalar settings (Forge-85wn) ---
	{
		Key:         "max_total_smiths",
		Type:        typeInt,
		Area:        "Concurrency",
		Label:       "Max total smiths",
		Description: "Maximum number of Smith workers running concurrently across all anvils.",
		Default:     4,
		Min:         fptr(1),
		Max:         fptr(64),
		value:       func(s config.SettingsConfig) any { return s.MaxTotalSmiths },
	},
	{
		Key:         "max_lifecycle_workers",
		Type:        typeInt,
		Area:        "Concurrency",
		Label:       "Max lifecycle workers",
		Description: "Maximum concurrent lifecycle/bellows workers (CI-fix, review-fix, rebase). 0 disables the cap.",
		Default:     config.DefaultMaxLifecycleWorkers,
		Min:         fptr(0),
		Max:         fptr(64),
		value:       func(s config.SettingsConfig) any { return s.MaxLifecycleWorkers },
	},
	{
		Key:         "daily_cost_limit",
		Type:        typeFloat,
		Area:        "Cost",
		Label:       "Daily cost limit",
		Description: "Maximum estimated spend per calendar day. 0 means unlimited. Dispatch pauses once the day's estimate exceeds this.",
		Default:     float64(0),
		Min:         fptr(0),
		Unit:        "USD",
		value:       func(s config.SettingsConfig) any { return s.DailyCostLimit },
	},
	{
		Key:         "merge_strategy",
		Type:        typeEnum,
		Area:        "Pipeline",
		Label:       "Merge strategy",
		Description: "How PRs are merged when Forge merges them (squash, merge commit, or rebase).",
		Default:     "squash",
		Options:     []string{"squash", "merge", "rebase"},
		value: func(s config.SettingsConfig) any {
			if strings.TrimSpace(s.MergeStrategy) == "" {
				return "squash"
			}
			return s.MergeStrategy
		},
	},
	{
		Key:         "max_pipeline_iterations",
		Type:        typeInt,
		Area:        "Retry limits",
		Label:       "Max pipeline iterations",
		Description: "Maximum Smith↔Warden loop iterations per bead before giving up.",
		Default:     5,
		Min:         fptr(1),
		Max:         fptr(20),
		value:       func(s config.SettingsConfig) any { return s.MaxPipelineIterations },
	},
	{
		Key:         "max_review_attempts",
		Type:        typeInt,
		Area:        "Retry limits",
		Label:       "Max review attempts",
		Description: "Maximum Warden review cycles per bead.",
		Default:     2,
		Min:         fptr(1),
		Max:         fptr(20),
		value:       func(s config.SettingsConfig) any { return s.MaxReviewAttempts },
	},
	{
		Key:         "max_ci_fix_attempts",
		Type:        typeInt,
		Area:        "Retry limits",
		Label:       "Max CI-fix attempts",
		Description: "Maximum CI-fix (Quench) cycles per PR before it needs a human.",
		Default:     5,
		Min:         fptr(1),
		Max:         fptr(20),
		value:       func(s config.SettingsConfig) any { return s.MaxCIFixAttempts },
	},
	{
		Key:         "max_review_fix_attempts",
		Type:        typeInt,
		Area:        "Retry limits",
		Label:       "Max review-fix attempts",
		Description: "Maximum review-fix (Burnish) cycles per PR before it needs a human.",
		Default:     5,
		Min:         fptr(1),
		Max:         fptr(20),
		value:       func(s config.SettingsConfig) any { return s.MaxReviewFixAttempts },
	},
	{
		Key:         "max_rebase_attempts",
		Type:        typeInt,
		Area:        "Retry limits",
		Label:       "Max rebase attempts",
		Description: "Maximum rebase retry cycles for a conflicting PR before it needs a human.",
		Default:     3,
		Min:         fptr(1),
		Max:         fptr(20),
		value:       func(s config.SettingsConfig) any { return s.MaxRebaseAttempts },
	},
	{
		Key:         "copilot_warden_sample_rate",
		Type:        typeFloat,
		Area:        "Copilot",
		Label:       "Warden sample rate",
		Description: "Probability (0.0–1.0) of running a real Warden review when Smith self-review already approved (Copilot combined mode).",
		Default:     0.1,
		Min:         fptr(0),
		Max:         fptr(1),
		value:       func(s config.SettingsConfig) any { return s.CopilotWardenSampleRate },
	},
	{
		Key:         "wicket_batch_size",
		Type:        typeInt,
		Area:        "Wicket",
		Label:       "Wicket batch size",
		Description: "Number of GitHub issues Wicket processes per scan per repository.",
		Default:     20,
		Min:         fptr(1),
		Max:         fptr(200),
		value:       func(s config.SettingsConfig) any { return s.WicketBatchSize },
	},

	// --- Composite & duration settings (Forge-vo5a) ---
	{
		Key:         "providers",
		Type:        typeStringList,
		Area:        "Providers",
		Label:       "Provider chain",
		Description: "Ordered list of AI providers to try (e.g. \"claude\", \"gemini\"). When a provider signals a rate limit the next one in the list is used.",
		Default:     []string(nil),
		value:       func(s config.SettingsConfig) any { return s.Providers },
	},
	{
		Key:         "stage_providers",
		Type:        typeProviderMap,
		Area:        "Providers",
		Label:       "Per-stage providers",
		Description: "Per-stage provider overrides keyed by pipeline stage (smith, warden, schematic, cifix, reviewfix). Each value is an ordered provider chain.",
		Options:     providerStages,
		Default:     map[string][]string(nil),
		value:       func(s config.SettingsConfig) any { return s.StageProviders },
	},
	durationKey("poll_interval", "Scheduling", "Poll interval",
		"How often the poller checks each anvil for ready beads.", "5m0s",
		func(s config.SettingsConfig) time.Duration { return s.PollInterval }),
	durationKey("smith_timeout", "Scheduling", "Smith timeout",
		"Maximum time a single Smith worker may run before it is terminated.", "30m0s",
		func(s config.SettingsConfig) time.Duration { return s.SmithTimeout }),
	durationKey("bellows_interval", "Bellows", "Bellows interval",
		"How often Bellows polls open PRs for CI status, reviews, and conflicts.", "2m0s",
		func(s config.SettingsConfig) time.Duration { return s.BellowsInterval }),
	durationKey("stale_interval", "Watchdog", "Stale interval",
		"How long a worker log may go idle before the worker is marked stalled. 0 disables stale detection.", "5m0s",
		func(s config.SettingsConfig) time.Duration { return s.StaleInterval }),
	durationKey("depcheck_interval", "Depcheck", "Depcheck interval",
		"How often the dependency-update scanner runs. 0 disables it.", "168h0m0s",
		func(s config.SettingsConfig) time.Duration { return s.DepcheckInterval }),
	durationKey("depcheck_timeout", "Depcheck", "Depcheck timeout",
		"Maximum time for a single dependency scan per anvil.", "5m0s",
		func(s config.SettingsConfig) time.Duration { return s.DepcheckTimeout }),
	durationKey("vulncheck_interval", "Vulncheck", "Vulncheck interval",
		"How often govulncheck runs on Go anvils. 0 disables scheduled scanning.", "24h0m0s",
		func(s config.SettingsConfig) time.Duration { return s.VulncheckInterval }),
	durationKey("vulncheck_timeout", "Vulncheck", "Vulncheck timeout",
		"Maximum time for a single govulncheck invocation per anvil.", "10m0s",
		func(s config.SettingsConfig) time.Duration { return s.VulncheckTimeout }),
	durationKey("smelter_interval", "Smelter", "Smelter interval",
		"How often the Smelter batches pending Warden rules into PRs.", "8h0m0s",
		func(s config.SettingsConfig) time.Duration { return s.SmelterInterval }),
	durationKey("questgiver_interval", "QuestGiver", "QuestGiver interval",
		"How often the QuestGiver monitor polls anvils for E2E quests.", "24h0m0s",
		func(s config.SettingsConfig) time.Duration { return s.QuestgiverInterval }),
	durationKey("wicket_interval", "Wicket", "Wicket interval",
		"How often Wicket polls repositories for new GitHub issues.", "15m0s",
		func(s config.SettingsConfig) time.Duration { return s.WicketInterval }),
	durationKey("rate_limit_backoff", "Scheduling", "Rate-limit backoff",
		"How long dispatch waits after releasing a bead when all providers are rate limited.", "5m0s",
		func(s config.SettingsConfig) time.Duration { return s.RateLimitBackoff }),
	durationKey("crucible_poll_interval", "Crucible", "Crucible poll interval",
		"Interval for the slow unfiltered poll that rebuilds the Crucible parent-child graph. 0 disables two-tier polling.", "3m0s",
		func(s config.SettingsConfig) time.Duration { return s.CruciblePollInterval }),
	durationKey("burnish_verify_timeout", "Bellows", "Burnish verify timeout",
		"Maximum time for the post-Smith temper step in a single Burnish (review-fix) attempt.", "5m0s",
		func(s config.SettingsConfig) time.Duration { return s.BurnishVerifyTimeout }),
	durationKey("adventurer_timeout", "QuestGiver", "Adventurer timeout",
		"Maximum time for a single quest execution by the headless-browser adventurer.", "5m0s",
		func(s config.SettingsConfig) time.Duration { return s.AdventurerTimeout }),

	// --- Kiln preview environments (Forge-9epl) ---
	{
		Key:         "preview_enabled",
		Type:        typeBool,
		Area:        "Previews",
		Label:       "Preview environments",
		Description: "Master gate for Kiln preview environments. When off, no preview can be started regardless of per-anvil settings.",
		Default:     false,
		value:       func(s config.SettingsConfig) any { return s.PreviewEnabled },
	},
	{
		Key:         "preview_max_concurrent",
		Type:        typeInt,
		Area:        "Previews",
		Label:       "Max concurrent previews",
		Description: "How many preview environments may run at once. Each one costs real memory (database, API, dev server), so keep this low.",
		Default:     config.DefaultPreviewMaxConcurrent,
		Min:         fptr(1),
		Max:         fptr(16),
		value:       func(s config.SettingsConfig) any { return s.ResolvedPreviewMaxConcurrent() },
	},
	{
		Key:         "preview_port_range",
		Type:        typeString,
		Area:        "Previews",
		Label:       "Preview port range",
		Description: "Inclusive \"min-max\" TCP port range preview services are allocated from (e.g. 42000-42999).",
		Default:     config.DefaultPreviewPortRange,
		value:       func(s config.SettingsConfig) any { return s.PreviewPortRange },
	},
	{
		Key:         "preview_bind_host",
		Type:        typeString,
		Area:        "Previews",
		Label:       "Preview bind host",
		Description: "Address preview services bind to, available to manifests as {{.BindHost}}. 127.0.0.1 keeps them on loopback; 0.0.0.0 exposes them to the LAN/VPN. Preview URLs bypass the Hearth login.",
		Default:     config.DefaultPreviewBindHost,
		value:       func(s config.SettingsConfig) any { return s.PreviewBindHost },
	},
	{
		Key:         "preview_public_host",
		Type:        typeString,
		Area:        "Previews",
		Label:       "Preview public host",
		Description: "Hostname used in displayed preview links (e.g. the box's LAN or WireGuard name). Empty falls back to the bind host.",
		Default:     "",
		value:       func(s config.SettingsConfig) any { return s.PreviewPublicHost },
	},
	durationKey("preview_idle_timeout", "Previews", "Preview idle timeout",
		"How long a preview may go unused before it is torn down. 0 disables the idle reaper.", "30m0s",
		func(s config.SettingsConfig) time.Duration { return s.PreviewIdleTimeout }),
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
		Keys:      make([]ConfigKeyInfo, 0, len(managedConfigKeys)),
		AnvilKeys: anvilKeySchema(),
		Anvils:    cfg.AnvilSettingsMap(),
	}
	for _, d := range managedConfigKeys {
		val := d.value(cfg.Settings)
		resp.Keys = append(resp.Keys, ConfigKeyInfo{
			Key:           d.Key,
			Type:          d.Type,
			Value:         val,
			IsDefault:     isDefault(val, d.Default),
			HotReloadable: d.HotReloadable,
			Area:          d.Area,
			Label:         d.Label,
			Description:   d.Description,
			Options:       d.Options,
			Min:           d.Min,
			Max:           d.Max,
			Unit:          d.Unit,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// isDefault reports whether val equals def, normalizing nil and empty
// slices/maps as equal so that e.g. providers: [] is treated as default.
func isDefault(val, def any) bool {
	if reflect.DeepEqual(val, def) {
		return true
	}
	rv := reflect.ValueOf(val)
	rd := reflect.ValueOf(def)
	if rv.IsValid() && rd.IsValid() && rv.Type() == rd.Type() {
		switch rv.Kind() {
		case reflect.Slice, reflect.Map:
			if rv.Len() == 0 && rd.Len() == 0 {
				return true
			}
		}
	}
	if rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map) && rv.Len() == 0 && def == nil {
		return true
	}
	if rd.IsValid() && (rd.Kind() == reflect.Slice || rd.Kind() == reflect.Map) && rd.Len() == 0 && val == nil {
		return true
	}
	return false
}

// scalarYAML builds a scalar YAML node with the given tag and rendered value.
func scalarYAML(tag, value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

// isEmptyStringNode reports whether n is an empty string scalar. An empty
// optional string clears a per-anvil key so the anvil inherits the default.
func isEmptyStringNode(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.Value == "" && (n.Tag == "!!str" || n.Tag == "")
}

// validateValue validates a JSON value against a typed key definition and
// returns the YAML node to persist. Scalar types (bool/int/float/enum/string/
// duration) produce a scalar node; string_list produces a sequence node of
// scalar strings; provider_map produces a mapping node (stage → sequence of
// provider strings). It enforces enum membership, numeric Min/Max bounds,
// non-integer rejection for int keys, non-empty list elements, the allowed
// provider_map stage set, and Go duration parseability.
func validateValue(key, typ string, options []string, min, max *float64, raw json.RawMessage) (*yaml.Node, error) {
	switch typ {
	case typeBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("key %q expects a boolean", key)
		}
		v := "false"
		if b {
			v = "true"
		}
		return scalarYAML("!!bool", v), nil
	case typeInt:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("key %q expects an integer", key)
		}
		if f != float64(int64(f)) {
			return nil, fmt.Errorf("key %q expects a whole number", key)
		}
		if err := checkBounds(key, f, min, max); err != nil {
			return nil, err
		}
		return scalarYAML("!!int", strconv.FormatInt(int64(f), 10)), nil
	case typeFloat:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("key %q expects a number", key)
		}
		if err := checkBounds(key, f, min, max); err != nil {
			return nil, err
		}
		return scalarYAML("!!float", strconv.FormatFloat(f, 'g', -1, 64)), nil
	case typeEnum:
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return nil, fmt.Errorf("key %q expects a string", key)
		}
		for _, opt := range options {
			if opt == str {
				return scalarYAML("!!str", str), nil
			}
		}
		return nil, fmt.Errorf("invalid value %q for key %q: expected one of %s", str, key, strings.Join(options, ", "))
	case typeString:
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return nil, fmt.Errorf("key %q expects a string", key)
		}
		return scalarYAML("!!str", str), nil
	case typeDuration:
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return nil, fmt.Errorf("key %q expects a duration string", key)
		}
		str = strings.TrimSpace(str)
		if _, err := time.ParseDuration(str); err != nil {
			return nil, fmt.Errorf("key %q expects a Go duration string like \"5m\" or \"24h\"", key)
		}
		return scalarYAML("!!str", str), nil
	case typeStringList:
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("key %q expects a list of strings", key)
		}
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for i, el := range list {
			if strings.TrimSpace(el) == "" {
				return nil, fmt.Errorf("key %q: element %d must be a non-empty string", key, i)
			}
			seq.Content = append(seq.Content, scalarYAML("!!str", el))
		}
		return seq, nil
	case typeProviderMap:
		var m map[string][]string
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("key %q expects a map of stage to string list", key)
		}
		// Emit stages in a stable order so the persisted diff is deterministic.
		stages := make([]string, 0, len(m))
		for k := range m {
			stages = append(stages, k)
		}
		sort.Strings(stages)
		node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, stage := range stages {
			if !providerStageSet[stage] {
				return nil, fmt.Errorf("key %q: unknown stage %q (allowed: %s)", key, stage, strings.Join(providerStages, ", "))
			}
			if len(m[stage]) == 0 {
				return nil, fmt.Errorf("key %q: stage %q must have at least one provider", key, stage)
			}
			seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for i, el := range m[stage] {
				if strings.TrimSpace(el) == "" {
					return nil, fmt.Errorf("key %q: stage %q element %d must be a non-empty string", key, stage, i)
				}
				seq.Content = append(seq.Content, scalarYAML("!!str", el))
			}
			node.Content = append(node.Content, scalarYAML("!!str", stage), seq)
		}
		return node, nil
	default:
		return nil, fmt.Errorf("key %q has unsupported type %q", key, typ)
	}
}

// checkBounds enforces the optional Min/Max numeric bounds.
func checkBounds(key string, f float64, min, max *float64) error {
	if min != nil && f < *min {
		return fmt.Errorf("key %q must be >= %s", key, strconv.FormatFloat(*min, 'g', -1, 64))
	}
	if max != nil && f > *max {
		return fmt.Errorf("key %q must be <= %s", key, strconv.FormatFloat(*max, 'g', -1, 64))
	}
	return nil
}

// handleForgeConfigPatch accepts a map of typed key->value, validates every key
// and value against the allowlist/schema (all-or-nothing), persists the changes
// via a YAML node-tree edit that preserves comments and unrelated keys, and
// returns the freshly re-read config (same shape as GET). PATCH /api/forge/config.
func (s *Server) handleForgeConfigPatch(w http.ResponseWriter, r *http.Request) {
	var req map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if len(req) == 0 {
		writeError(w, http.StatusBadRequest, "no config keys provided")
		return
	}

	// All-or-nothing: validate every key and value before writing anything.
	var unknown []string
	nodes := make(map[string]*yaml.Node, len(req))
	for k, raw := range req {
		def, ok := configKeyByName[k]
		if !ok {
			unknown = append(unknown, k)
			continue
		}
		vn, err := validateValue(def.Key, def.Type, def.Options, def.Min, def.Max, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		nodes[k] = vn
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
	if err := applyConfigPatch(path, nodes); err != nil {
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

// anvilKeyDef describes one allowlisted per-anvil settings key: its type, UI
// metadata, enum options / numeric bounds, whether it is tri-state (a *bool
// that can be cleared to inherit), and whether the daemon hot-reloads it. This
// is the single source of truth for the anvils.<name>.<key> contract shared by
// the GET schema, PATCH validation, and the hot-reload coverage in the response.
type anvilKeyDef struct {
	Key         string
	Type        string
	Label       string
	Description string
	// TriState marks a *bool field where JSON null clears the override
	// (anvil inherits the global/default). Plain bools reject null.
	TriState bool
	// Instant marks keys the daemon applies live via internal/hotreload.
	// Only auto_merge is instant; the rest apply on the next dispatch/run.
	Instant bool
	Options []string
	Min     *float64
	Max     *float64
}

// clearable reports whether a JSON null is accepted for this key (removing it so
// the anvil inherits the default). True for tri-state *bool keys and for every
// non-bool scalar (which can be reset to the anvil/global default).
func (d anvilKeyDef) clearable() bool {
	return d.TriState || d.Type != typeBool
}

// managedAnvilKeys is the allowlist of per-anvil settings exposed by the config
// API, mirroring config.AnvilSettings. Order is preserved in the response's
// schema and per-key coverage. Only auto_merge is hot-reloadable (see
// internal/hotreload/hotreload.go, which diffs anvil auto_merge live); the
// others are read on the next dispatch/run.
var managedAnvilKeys = []anvilKeyDef{
	{Key: "auto_merge", Type: typeBool, TriState: false, Instant: true,
		Label: "Auto-merge PRs", Description: "Automatically merge this anvil's PRs once required checks pass."},
	{Key: "schematic_enabled", Type: typeBool, TriState: true,
		Label: "Schematic pre-analysis", Description: "Override the global Schematic pre-worker for this anvil."},
	{Key: "golangci_lint", Type: typeBool, TriState: true,
		Label: "golangci-lint", Description: "Run golangci-lint as a Temper step for this anvil."},
	{Key: "go_race_detection", Type: typeBool, TriState: true,
		Label: "Go race detection", Description: "Run the Go race detector (-race) as a Temper step for this anvil."},
	{Key: "depcheck_enabled", Type: typeBool, TriState: true,
		Label: "Dependency scanning", Description: "Include this anvil in scheduled dependency-update scans."},
	{Key: "questgiver_enabled", Type: typeBool, TriState: true,
		Label: "QuestGiver E2E quests", Description: "Discover and run E2E quests for this anvil."},
	{Key: "preview_enabled", Type: typeBool, TriState: true,
		Label: "Kiln previews", Description: "Allow on-demand preview environments for this anvil's branches (requires the global preview_enabled and a .forge/preview.yaml manifest)."},
	{Key: "preview_quests", Type: typeBool, TriState: false,
		Label: "Preview E2E quests", Description: "Run this anvil's E2E quests against a running preview environment (requires previews to be enabled for the anvil)."},
	{Key: "wicket_enabled", Type: typeBool, TriState: true,
		Label: "Wicket issue triage", Description: "Poll this anvil's GitHub issues and triage them into beads."},
	{Key: "wicket_auto_dispatch", Type: typeBool, TriState: false,
		Label: "Wicket auto-dispatch", Description: "Auto-dispatch beads created by Wicket triage for this anvil."},

	// --- Non-boolean per-anvil scalars (Forge-85wn). Send null to reset. ---
	{Key: "max_smiths", Type: typeInt, Min: fptr(0), Max: fptr(32),
		Label: "Max smiths", Description: "Maximum concurrent Smith workers for this anvil (0 uses the default)."},
	{Key: "auto_dispatch", Type: typeEnum, Options: []string{"off", "all", "tagged", "priority"},
		Label: "Auto-dispatch mode", Description: "How ready beads are dispatched for this anvil."},
	{Key: "auto_dispatch_tag", Type: typeString,
		Label: "Auto-dispatch tag", Description: "Label a bead must carry when auto-dispatch is \"tagged\" (e.g. forgeReady)."},
	{Key: "auto_dispatch_min_priority", Type: typeInt, Min: fptr(0), Max: fptr(4),
		Label: "Auto-dispatch min priority", Description: "Minimum priority (0=highest) a bead needs to be auto-dispatched."},
	{Key: "preview_auto", Type: typeEnum, Options: config.PreviewAutoModes,
		Label: "Automatic previews", Description: "When to start a preview without being asked. \"ready_to_merge\" starts one as a PR becomes mergeable; it still obeys preview_max_concurrent and the idle timeout, and each preview holds memory for as long as it runs."},
	{Key: "platform", Type: typeEnum, Options: []string{"github", "gitlab", "gitea", "bitbucket", "azuredevops"},
		Label: "VCS platform", Description: "Hosting platform for this anvil's PR operations."},

	// --- Composite per-anvil overrides (Forge-vo5a). Send null to inherit. ---
	{Key: "stage_providers", Type: typeProviderMap, Options: providerStages,
		Label: "Per-stage providers", Description: "Per-anvil override of the global per-stage provider chains (smith, warden, schematic, cifix, reviewfix)."},
	{Key: "wicket_trusted_users", Type: typeStringList,
		Label: "Wicket trusted users", Description: "GitHub logins whose issues are auto-dispatched without extra review for this anvil."},
	{Key: "wicket_ignore_users", Type: typeStringList,
		Label: "Wicket ignored users", Description: "GitHub logins skipped entirely when triaging issues for this anvil."},
	{Key: "wicket_repos", Type: typeStringList,
		Label: "Wicket repositories", Description: "\"owner/repo\" strings Wicket scans for this anvil. Empty derives the repo from the git remote."},
	{Key: "wicket_issue_labels", Type: typeStringList,
		Label: "Wicket issue labels", Description: "GitHub labels an issue must carry for Wicket to triage it in this anvil. Empty means all issues are eligible."},
	{Key: "wicket_triage_prompt", Type: typeString,
		Label: "Wicket triage prompt", Description: "Optional prompt suffix appended to the default Wicket triage system prompt for this anvil."},
}

// anvilKeySchema projects managedAnvilKeys into the GET response schema so the
// frontend renders per-anvil controls from metadata rather than a hardcoded list.
func anvilKeySchema() []AnvilKeyInfo {
	out := make([]AnvilKeyInfo, 0, len(managedAnvilKeys))
	for _, d := range managedAnvilKeys {
		out = append(out, AnvilKeyInfo{
			Key:           d.Key,
			Type:          d.Type,
			TriState:      d.TriState,
			HotReloadable: d.Instant,
			Label:         d.Label,
			Description:   d.Description,
			Options:       d.Options,
			Min:           d.Min,
			Max:           d.Max,
		})
	}
	return out
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
	anvilCfg, ok := cfg.Anvils[name]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown anvil: "+name)
		return
	}

	// All-or-nothing: validate every key and value before writing anything.
	var unknown []string
	sets := map[string]*yaml.Node{}
	var clears []string
	applied := make([]AnvilKeyApplied, 0, len(req))
	for k, raw := range req {
		def, ok := anvilKeyByName[k]
		if !ok {
			unknown = append(unknown, k)
			continue
		}
		cleared := false
		if strings.TrimSpace(string(raw)) == "null" {
			if !def.clearable() {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("key %q cannot be cleared (null); it is a plain boolean, send true or false", k))
				return
			}
			clears = append(clears, k)
			cleared = true
		} else {
			vn, err := validateValue(def.Key, def.Type, def.Options, def.Min, def.Max, raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			// An empty optional string clears the key so the anvil inherits.
			if def.Type == typeString && isEmptyStringNode(vn) {
				clears = append(clears, k)
				cleared = true
			} else {
				sets[k] = vn
			}
		}
		appliesVal := appliesNextRun
		if def.Instant {
			appliesVal = appliesInstant
		}
		applied = append(applied, AnvilKeyApplied{
			Key:     k,
			Applies: appliesVal,
			Cleared: cleared,
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
	if err := applyAnvilConfigPatch(path, name, anvilCfg.Path, sets, clears); err != nil {
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
// once on a complete file. When creating a new anvil block, anvilPath is
// persisted as the "path" key so the file remains a valid config.
func applyAnvilConfigPatch(path, anvil, anvilPath string, sets map[string]*yaml.Node, clears []string) error {
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
		if anvilPath != "" {
			setMappingChild(anvilNode, "path", &yaml.Node{
				Kind: yaml.ScalarNode, Tag: "!!str", Value: anvilPath,
			})
		}
	}

	// Apply sets in a stable order so the diff is deterministic.
	keys := make([]string, 0, len(sets))
	for k := range sets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		setValueNode(anvilNode, key, sets[key])
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
// settings.<key> to the given YAML node (scalar, sequence, or mapping). It
// parses into a yaml.Node tree (rather than marshalling a full Config) so
// comments and unrelated keys are preserved, locates or creates the
// top-level `settings` mapping and each target node, then writes the result
// back atomically (temp file + rename) so the fsnotify watcher fires once
// on a complete file.
func applyConfigPatch(path string, patch map[string]*yaml.Node) error {
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
		setValueNode(settings, key, patch[key])
	}

	out, err := marshalNode(&doc)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return writeFileAtomic(path, out)
}

// setValueNode sets m[key] to vn, editing the existing value node in place
// (preserving its surrounding comments) or appending a new key/value pair. It
// handles scalar, sequence, and mapping value nodes, so it serializes the
// scalar types as well as the string_list (sequence) and provider_map
// (mapping) value types.
func setValueNode(m *yaml.Node, key string, vn *yaml.Node) {
	if existing := mappingValueNode(m, key); existing != nil {
		// Replace the node's content but keep any comments attached to the
		// existing value so a node-tree edit preserves surrounding annotations.
		head, line, foot := existing.HeadComment, existing.LineComment, existing.FootComment
		*existing = *vn
		existing.HeadComment, existing.LineComment, existing.FootComment = head, line, foot
		return
	}
	setMappingChild(m, key, vn)
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
