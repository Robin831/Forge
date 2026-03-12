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
//   - settings.max_ci_fix_attempts (applied immediately to lifecycle manager)
//   - settings.max_review_fix_attempts (applied immediately to lifecycle manager)
//   - settings.max_rebase_attempts (applied immediately to lifecycle manager)
//   - notifications.* (all notification settings)
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
			// Only react to events for our config file.
			if filepath.Base(event.Name) != configBase {
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

	if old.Notifications.TeamsWebhookURL != new.Notifications.TeamsWebhookURL {
		changes = append(changes, "teams_webhook_url changed")
	}

	if old.Notifications.Enabled != new.Notifications.Enabled {
		changes = append(changes, fmt.Sprintf("notifications.enabled: %v → %v",
			old.Notifications.Enabled, new.Notifications.Enabled))
	}

	if !sliceEqual(old.Notifications.Events, new.Notifications.Events) {
		changes = append(changes, "notifications.events changed")
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
