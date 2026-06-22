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
//   - anvils.* adding or removing anvil entries (updates bellows and depcheck)
package hotreload

import (
	"fmt"
	"log/slog"
	"path/filepath"
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
		return
	}

	w.current = newCfg
	w.mu.Unlock()

	for _, change := range changes {
		w.logger.Info("config updated", "field", change)
	}

	// Notify callbacks
	for _, cb := range w.callbacks {
		cb(oldCfg, newCfg)
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
