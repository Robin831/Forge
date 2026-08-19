// Package hotreload watches forge.yaml for changes and applies safe updates
// to the running daemon without a restart.
//
// Hot-reloadable settings:
//   - settings.poll_interval
//   - settings.smith_timeout
//   - settings.max_total_smiths
//   - settings.claude_flags
//   - settings.providers
//   - settings.smith_providers
//   - settings.stage_providers
//   - settings.max_ci_fix_attempts (applied immediately to lifecycle manager)
//   - settings.max_review_fix_attempts (applied immediately to lifecycle manager)
//   - settings.max_rebase_attempts (applied immediately to lifecycle manager)
//   - settings.max_lifecycle_workers (gates concurrent lifecycle fix workers; read per-dispatch)
//   - settings.crucible_poll_interval (change the slow-path Crucible poll cadence)
//   - settings.copilot_combined_smith_warden (toggle combined mode at runtime)
//   - settings.copilot_warden_sample_rate (adjust sampling rate at runtime)
//   - settings.smelter_enabled (enable/disable Smelter at runtime)
//   - settings.smelter_interval (change Smelter schedule at runtime)
//   - notifications.* (all notification settings)
//   - anvils.<name>.auto_merge (takes effect on next ready-to-merge transition)
//   - anvils.<name>.max_smiths (changes to existing anvils' concurrency limit)
//   - anvils.<name>.path (changes to existing anvils' path; updates bellows and depcheck)
//   - anvils.<name>.preview_enabled (read per preview_start, so the next start obeys it)
//   - anvils.<name>.preview_auto (read on the next ready-to-merge transition)
//   - anvils.<name>.preview_quests (read per quest run)
//   - anvils.* adding or removing anvil entries (updates bellows and depcheck)
//
// Everything else is read once, at startup. A reload that touches such a
// setting is reported — see restartOnlyKeys and reportIgnored — rather than
// silently dropped: the whole point of the warning is that the file on disk and
// the running daemon otherwise disagree with nothing to say so.
package hotreload

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/fsnotify/fsnotify"
)

// Callback is called when config changes are detected and applied.
// oldCfg is the previous config, newCfg is the updated config.
type Callback func(oldCfg, newCfg *config.Config)

// Watcher monitors the config file and applies safe changes.
type Watcher struct {
	configFile string
	logger     *slog.Logger
	mu         sync.RWMutex
	current    *config.Config
	callbacks  []Callback
	stop       chan struct{}
	debounce   time.Duration
}

// NewWatcher creates a config file watcher.
func NewWatcher(configFile string, current *config.Config, logger *slog.Logger) *Watcher {
	return &Watcher{
		configFile: configFile,
		logger:     logger,
		current:    current,
		stop:       make(chan struct{}),
		debounce:   500 * time.Millisecond,
	}
}

// OnChange registers a callback for config changes.
func (w *Watcher) OnChange(cb Callback) {
	w.callbacks = append(w.callbacks, cb)
}

// Current returns the current config (thread-safe).
func (w *Watcher) Current() *config.Config {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.current
}

// Start begins watching the config file. Blocks until Stop() or error.
//
// We watch the parent directory instead of the file itself because many editors
// (and tools like Viper) save files via write-to-temp + rename. On Windows this
// can cause fsnotify to stop delivering events for the original file after the
// rename. Watching the directory catches Create/Rename events for the config
// filename regardless of how the file is written.
func (w *Watcher) Start() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer watcher.Close()

	absPath, err := filepath.Abs(w.configFile)
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}
	w.configFile = absPath // normalize once so reload() uses the same resolved path
	configDir := filepath.Dir(absPath)
	configBase := filepath.Base(absPath)

	if err := watcher.Add(configDir); err != nil {
		return fmt.Errorf("watching directory %s: %w", configDir, err)
	}

	w.logger.Info("config hot-reload started", "file", absPath, "dir", configDir)

	var debounceTimer *time.Timer
	for {
		select {
		case <-w.stop:
			w.logger.Info("config watcher stopped")
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// Only react to events for our config file (or the ConfigMap
			// atomic-update marker — see configEventIsRelevant).
			if !configEventIsRelevant(event.Name, configBase) {
				continue
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				// Debounce rapid write events (editors write multiple times)
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(w.debounce, func() {
					w.reload()
				})
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			w.logger.Error("config watcher error", "error", err)
		}
	}
}

// configEventIsRelevant reports whether a directory event should trigger a
// reload. It matches two cases:
//
//   - A direct edit of the config file (basename == configBase). This covers
//     local `forge.yaml` edits and the write-to-temp+rename that editors and
//     the web config PATCH use.
//   - A Kubernetes ConfigMap/Secret atomic update. A mounted ConfigMap is a
//     symlink tree: `forge.yaml -> ..data/forge.yaml` and `..data -> ..<ts>/`.
//     kubelet updates it by writing a new timestamped directory and atomically
//     renaming `..data` to point at it — the mounted `forge.yaml` symlink is
//     never touched. So a filter keyed only on basename == "forge.yaml" never
//     fires for ConfigMap-delivered changes, which is exactly how the daemon
//     runs in production (config mounted read-only from a ConfigMap). Reacting
//     to the `..data` rename closes that gap; reload()'s applyChanges() no-op
//     guard keeps a spurious swap cheap.
func configEventIsRelevant(eventName, configBase string) bool {
	base := filepath.Base(eventName)
	return base == configBase || base == "..data"
}

// Stop terminates the watcher.
func (w *Watcher) Stop() {
	close(w.stop)
}

// reload reads the config file and applies safe changes.
func (w *Watcher) reload() {
	newCfg, err := config.Load(w.configFile)
	if err != nil {
		w.logger.Error("failed to reload config", "error", err)
		return
	}

	w.mu.Lock()
	oldCfg := w.current

	// Apply only hot-reloadable fields
	changes := applyChanges(oldCfg, newCfg)
	if len(changes) == 0 {
		w.mu.Unlock()
		// Nothing hot-reloadable moved, so the running config stands — but the
		// operator still edited something, and saying so is the difference
		// between "I changed it and it did not work" and silence.
		w.reportIgnored(oldCfg, newCfg, 0)
		return
	}

	w.current = newCfg
	w.mu.Unlock()

	for _, change := range changes {
		w.logger.Info("config updated", "field", change)
	}

	w.reportIgnored(oldCfg, newCfg, len(changes))

	// Notify callbacks
	for _, cb := range w.callbacks {
		cb(oldCfg, newCfg)
	}
}

// restartOnlyKey is one setting whose value the daemon captures at startup, so
// that a later edit cannot take effect until the process restarts. value
// renders it for the log line.
type restartOnlyKey struct {
	key   string
	value func(*config.Config) string
}

// restartOnlyKeys are the settings known to be read once, at construction time,
// and therefore not reachable by a reload. They are named individually because
// a warning that names the key the operator just edited is actionable, while a
// generic "something needs a restart" is not.
//
// The list is the Kiln/preview surface, which is where the boundary bites in
// practice: the preview manager, its port allocator and its idle reaper are all
// built once from these values (see internal/daemon/preview.go
// buildPreviewManager). Extending it elsewhere is a matter of adding an entry —
// anything not listed still produces the generic warning in reportIgnored when
// it changes alone.
//
// Deliberately absent: preview_proxy_base and preview_proxy_auth, which the web
// layer reads live through a closure over the current config (see
// internal/daemon/web.go), and the per-anvil tri-states, which are read per
// request and so are genuinely hot-reloadable.
var restartOnlyKeys = []restartOnlyKey{
	{"settings.preview_enabled", func(c *config.Config) string {
		return strconv.FormatBool(c.Settings.PreviewEnabled)
	}},
	{"settings.preview_port_range", func(c *config.Config) string {
		return c.Settings.PreviewPortRange
	}},
	{"settings.preview_bind_host", func(c *config.Config) string {
		return c.Settings.ResolvedPreviewBindHost()
	}},
	{"settings.preview_public_host", func(c *config.Config) string {
		return c.Settings.ResolvedPreviewPublicHost()
	}},
	{"settings.preview_max_concurrent", func(c *config.Config) string {
		return strconv.Itoa(c.Settings.ResolvedPreviewMaxConcurrent())
	}},
	{"settings.preview_evict_lru", func(c *config.Config) string {
		return strconv.FormatBool(c.Settings.PreviewEvictLRU)
	}},
	{"settings.preview_idle_timeout", func(c *config.Config) string {
		return c.Settings.PreviewIdleTimeout.String()
	}},
}

// restartRequiredChanges returns the restartOnlyKeys whose value differs
// between the two configs, rendered as "key: old → new".
func restartRequiredChanges(old, new *config.Config) []string {
	if old == nil || new == nil {
		return nil
	}
	var changed []string
	for _, k := range restartOnlyKeys {
		o, n := k.value(old), k.value(new)
		if o != n {
			changed = append(changed, fmt.Sprintf("%s: %s → %s", k.key, o, n))
		}
	}
	return changed
}

// reportIgnored logs what the reload could not apply. applied is the number of
// hot-reloadable changes the same reload did apply.
//
// A known restart-only setting is named. Anything else is caught by comparing
// the two configs whole: an edit that moved nothing hot-reloadable still
// changed the file, and the operator deserves to hear that it will not take
// effect until a restart — even for a key this package has never heard of.
//
// A reload that applies nothing leaves w.current untouched, so the divergence
// (and this warning) persists across later reloads until the daemon restarts.
// That is the honest reading: the file and the running daemon really do still
// disagree.
func (w *Watcher) reportIgnored(old, new *config.Config, applied int) {
	if restart := restartRequiredChanges(old, new); len(restart) > 0 {
		w.logger.Warn("config change requires a daemon restart to take effect",
			"keys", strings.Join(restart, ", "))
		return
	}
	if applied == 0 && !reflect.DeepEqual(old, new) {
		w.logger.Warn("config file changed but no hot-reloadable setting differs; " +
			"a daemon restart is required to apply the edit")
	}
}

// applyChanges compares old and new configs and returns a list of changed fields.
func applyChanges(old, new *config.Config) []string {
	var changes []string

	if old.Settings.PollInterval != new.Settings.PollInterval {
		changes = append(changes, fmt.Sprintf("poll_interval: %v → %v",
			old.Settings.PollInterval, new.Settings.PollInterval))
	}

	if old.Settings.SmithTimeout != new.Settings.SmithTimeout {
		changes = append(changes, fmt.Sprintf("smith_timeout: %v → %v",
			old.Settings.SmithTimeout, new.Settings.SmithTimeout))
	}

	if old.Settings.MaxTotalSmiths != new.Settings.MaxTotalSmiths {
		changes = append(changes, fmt.Sprintf("max_total_smiths: %d → %d",
			old.Settings.MaxTotalSmiths, new.Settings.MaxTotalSmiths))
	}

	if !sliceEqual(old.Settings.ClaudeFlags, new.Settings.ClaudeFlags) {
		changes = append(changes, "claude_flags changed")
	}

	if !sliceEqual(old.Settings.Providers, new.Settings.Providers) {
		changes = append(changes, "providers changed")
	}

	if !sliceEqual(old.Settings.SmithProviders, new.Settings.SmithProviders) {
		changes = append(changes, "smith_providers changed")
	}

	if !stageProvidersEqual(old.Settings.StageProviders, new.Settings.StageProviders) {
		changes = append(changes, "stage_providers changed")
	}

	if old.Notifications.ResolvedTeamsURL() != new.Notifications.ResolvedTeamsURL() {
		changes = append(changes, "teams_webhook_url changed")
	}

	if old.Notifications.Enabled != new.Notifications.Enabled {
		changes = append(changes, fmt.Sprintf("notifications.enabled: %v → %v",
			old.Notifications.Enabled, new.Notifications.Enabled))
	}

	if !sliceEqual(old.Notifications.ResolvedTeamsEvents(), new.Notifications.ResolvedTeamsEvents()) {
		changes = append(changes, "notifications.events changed")
	}

	if len(old.Notifications.Webhooks) != len(new.Notifications.Webhooks) {
		changes = append(changes, fmt.Sprintf("notifications.webhooks count: %d → %d",
			len(old.Notifications.Webhooks), len(new.Notifications.Webhooks)))
	} else {
		for i := range old.Notifications.Webhooks {
			if old.Notifications.Webhooks[i].URL != new.Notifications.Webhooks[i].URL ||
				old.Notifications.Webhooks[i].Name != new.Notifications.Webhooks[i].Name ||
				!sliceEqual(old.Notifications.Webhooks[i].Events, new.Notifications.Webhooks[i].Events) {
				changes = append(changes, fmt.Sprintf("webhook %q changed", old.Notifications.Webhooks[i].Name))
				break
			}
		}
	}

	if old.Settings.MaxCIFixAttempts != new.Settings.MaxCIFixAttempts {
		changes = append(changes, fmt.Sprintf("max_ci_fix_attempts: %d → %d",
			old.Settings.MaxCIFixAttempts, new.Settings.MaxCIFixAttempts))
	}

	if old.Settings.MaxReviewFixAttempts != new.Settings.MaxReviewFixAttempts {
		changes = append(changes, fmt.Sprintf("max_review_fix_attempts: %d → %d",
			old.Settings.MaxReviewFixAttempts, new.Settings.MaxReviewFixAttempts))
	}

	// Both of these are read per dispatch (the breaker on every review-fix
	// action, the retry count on every burnish verification), so a reload
	// applies immediately — they only need reporting.
	if old.Settings.MaxSameHeadReviewFixes != new.Settings.MaxSameHeadReviewFixes {
		changes = append(changes, fmt.Sprintf("max_same_head_review_fixes: %d → %d",
			old.Settings.MaxSameHeadReviewFixes, new.Settings.MaxSameHeadReviewFixes))
	}

	if old.Settings.BurnishVerifyRetries != new.Settings.BurnishVerifyRetries {
		changes = append(changes, fmt.Sprintf("burnish_verify_retries: %d → %d",
			old.Settings.BurnishVerifyRetries, new.Settings.BurnishVerifyRetries))
	}

	if old.Settings.MaxRebaseAttempts != new.Settings.MaxRebaseAttempts {
		changes = append(changes, fmt.Sprintf("max_rebase_attempts: %d → %d",
			old.Settings.MaxRebaseAttempts, new.Settings.MaxRebaseAttempts))
	}

	if old.Settings.MaxLifecycleWorkers != new.Settings.MaxLifecycleWorkers {
		changes = append(changes, fmt.Sprintf("max_lifecycle_workers: %d → %d",
			old.Settings.MaxLifecycleWorkers, new.Settings.MaxLifecycleWorkers))
	}

	if old.Settings.CopilotCombinedSmithWarden != new.Settings.CopilotCombinedSmithWarden {
		changes = append(changes, fmt.Sprintf("copilot_combined_smith_warden: %v → %v",
			old.Settings.CopilotCombinedSmithWarden, new.Settings.CopilotCombinedSmithWarden))
	}

	if old.Settings.CopilotWardenSampleRate != new.Settings.CopilotWardenSampleRate {
		changes = append(changes, fmt.Sprintf("copilot_warden_sample_rate: %v → %v",
			old.Settings.CopilotWardenSampleRate, new.Settings.CopilotWardenSampleRate))
	}

	// Read per bead close (parentclose.go), so swapping the config in is all a
	// reload takes — it only needs reporting.
	oldAutoCloseParents := old.Settings.IsAutoCloseParentsEnabled()
	newAutoCloseParents := new.Settings.IsAutoCloseParentsEnabled()
	if oldAutoCloseParents != newAutoCloseParents {
		changes = append(changes, fmt.Sprintf("auto_close_parents: %v → %v",
			oldAutoCloseParents, newAutoCloseParents))
	}

	oldSmelterEnabled := old.Settings.IsSmelterEnabled()
	newSmelterEnabled := new.Settings.IsSmelterEnabled()
	if oldSmelterEnabled != newSmelterEnabled {
		changes = append(changes, fmt.Sprintf("smelter_enabled: %v → %v",
			oldSmelterEnabled, newSmelterEnabled))
	}

	if old.Settings.CruciblePollInterval != new.Settings.CruciblePollInterval {
		changes = append(changes, fmt.Sprintf("crucible_poll_interval: %v → %v",
			old.Settings.CruciblePollInterval, new.Settings.CruciblePollInterval))
	}

	if old.Settings.SmelterInterval != new.Settings.SmelterInterval {
		changes = append(changes, fmt.Sprintf("smelter_interval: %v → %v",
			old.Settings.SmelterInterval, new.Settings.SmelterInterval))
	}

	// Assay config (global block). The daemon reads the resolved Assay config
	// live via SetAssayConfig (d.cfg.Load().ResolvedAssay), but reload() only
	// stores the new config when applyChanges reports a change — so without
	// detecting assay edits here, a ConfigMap change to e.g.
	// daily_cost_limit_usd would be silently ignored until a restart. DeepEqual
	// covers the tri-state *bool / []string (skip_paths) fields cleanly.
	if !reflect.DeepEqual(old.Assay, new.Assay) {
		changes = append(changes, "assay config changed")
	}

	// Detect anvil changes (add, remove, path change, max_smiths)
	for name, newAnvil := range new.Anvils {
		if oldAnvil, ok := old.Anvils[name]; ok {
			if oldAnvil.MaxSmiths != newAnvil.MaxSmiths {
				changes = append(changes, fmt.Sprintf("anvil %s max_smiths: %d → %d",
					name, oldAnvil.MaxSmiths, newAnvil.MaxSmiths))
			}
			if oldAnvil.Path != newAnvil.Path {
				changes = append(changes, fmt.Sprintf("anvil %s path: %q → %q",
					name, oldAnvil.Path, newAnvil.Path))
			}
			if oldAnvil.AutoMerge != newAnvil.AutoMerge {
				changes = append(changes, fmt.Sprintf("anvil %s auto_merge: %v → %v",
					name, oldAnvil.AutoMerge, newAnvil.AutoMerge))
			}
			if !stageProvidersEqual(oldAnvil.StageProviders, newAnvil.StageProviders) {
				changes = append(changes, fmt.Sprintf("anvil %s stage_providers changed", name))
			}
			if !reflect.DeepEqual(oldAnvil.Assay, newAnvil.Assay) {
				changes = append(changes, fmt.Sprintf("anvil %s assay config changed", name))
			}
			// The per-anvil preview tri-states are resolved per request
			// (config.IsPreviewEnabledForAnvil and friends), so swapping the
			// config in is all it takes for the next preview_start, automatic
			// start or quest run to obey the new value.
			if !boolPtrEqual(oldAnvil.PreviewEnabled, newAnvil.PreviewEnabled) {
				changes = append(changes, fmt.Sprintf("anvil %s preview_enabled: %s → %s",
					name, boolPtrString(oldAnvil.PreviewEnabled), boolPtrString(newAnvil.PreviewEnabled)))
			}
			if oldAnvil.PreviewAuto != newAnvil.PreviewAuto {
				changes = append(changes, fmt.Sprintf("anvil %s preview_auto: %q → %q",
					name, oldAnvil.PreviewAuto, newAnvil.PreviewAuto))
			}
			if oldAnvil.PreviewQuests != newAnvil.PreviewQuests {
				changes = append(changes, fmt.Sprintf("anvil %s preview_quests: %v → %v",
					name, oldAnvil.PreviewQuests, newAnvil.PreviewQuests))
			}
		} else {
			changes = append(changes, fmt.Sprintf("anvil %s added", name))
		}
	}
	for name := range old.Anvils {
		if _, ok := new.Anvils[name]; !ok {
			changes = append(changes, fmt.Sprintf("anvil %s removed", name))
		}
	}

	return changes
}

// boolPtrEqual compares two tri-state booleans: unset (nil) is distinct from
// both true and false, since it means "inherit the global setting".
func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// boolPtrString renders a tri-state boolean for a log line, naming the unset
// case rather than printing a pointer address.
func boolPtrString(b *bool) string {
	if b == nil {
		return "unset"
	}
	return strconv.FormatBool(*b)
}

// sliceEqual compares two string slices.
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// stageProvidersEqual compares two stage-provider maps.
func stageProvidersEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !sliceEqual(av, bv) {
			return false
		}
	}
	return true
}
