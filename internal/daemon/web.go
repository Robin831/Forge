package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/forgechat"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/vcs/github"
	"github.com/Robin831/Forge/internal/web"
)

// startWebServer launches the Hearth 2.0 web server when FORGE_WEB_ENABLED
// is truthy. The address comes from FORGE_WEB_ADDR (default :8080) and the
// user list from FORGE_USERS (user:bcrypt-hash,user:bcrypt-hash). The
// server runs in a goroutine and is stopped when ctx is cancelled.
//
// Returning a non-nil error means we tried to start the server but it
// could not be configured (bad FORGE_USERS, etc.) — the daemon continues
// without the web UI in that case but logs the failure.
func (d *Daemon) startWebServer(ctx context.Context) error {
	if !webEnabled() {
		return nil
	}

	addr := strings.TrimSpace(os.Getenv("FORGE_WEB_ADDR"))
	if addr == "" {
		addr = ":8080"
	}

	users, err := web.ParseUsersFromEnv()
	if err != nil {
		return fmt.Errorf("FORGE_USERS: %w", err)
	}
	if len(users) == 0 {
		d.logger.Warn("FORGE_WEB_ENABLED set but FORGE_USERS is empty — login will reject all attempts")
	}

	sessionTTL := d.sessionDurationEnv("FORGE_WEB_SESSION_TTL")
	if sessionTTL < 0 {
		d.logger.Warn("negative session TTL, using default", "var", "FORGE_WEB_SESSION_TTL", "value", sessionTTL)
		sessionTTL = 0
	}

	srv, err := web.New(web.Config{
		Addr:               addr,
		Users:              users,
		CookieSecure:       cookieSecureEnv(),
		SessionTTL:         sessionTTL,
		SessionAbsoluteTTL: d.sessionDurationEnv("FORGE_WEB_SESSION_ABSOLUTE_TTL"),
	}, d.db, d.handleIPC, d.logger)
	if err != nil {
		return fmt.Errorf("constructing web server: %w", err)
	}

	// Tell the web server which config file the daemon is using so
	// GET/PATCH /api/forge/config target the same file the hot-reloader
	// watches (honours --config).
	srv.SetConfigFile(d.configFile)

	// Gate the activity SSE stream onto legacy polling when
	// settings.sse_poll_fallback is set. The closure reads d.cfg each connect
	// so hot-reloads of the flag take effect without restarting the server.
	// This is a one-release safety valve; the polling path is slated for
	// removal next release.
	srv.SetSSEPollFallback(func() bool {
		return d.cfg.Load().Settings.SSEPollFallback
	})

	// Plug in the Beads-Forge AI runner using the daemon's configured
	// providers. We pick the head of the resolved list — fallbacks are not
	// needed here because a turn is interactive and should fail fast.
	if runner := d.buildForgeChatRunner(); runner != nil {
		srv.SetChatRunner(runner)
	}
	// Expose the live anvil registry so the bead-emission handler can
	// translate anvil names into on-disk paths for `bd create`. The closure
	// reads d.cfg via Load() each invocation so hot-reloads pick up edits
	// to forge.yaml without restarting the web server.
	srv.SetAnvilLister(func() map[string]string {
		cfg := d.cfg.Load()
		out := make(map[string]string, len(cfg.Anvils))
		for name, anvil := range cfg.Anvils {
			out[name] = anvil.Path
		}
		return out
	})
	// Build a name -> "https://github.com/owner/repo" map from each anvil's
	// git origin remote, so /api/status can render a worker's pr_number as a
	// clickable PR link. Derived once at startup (git reads .git/config, no
	// network); best-effort — anvils whose remote can't be parsed are omitted
	// and fall back to the ingot URL in the handler.
	{
		cfg := d.cfg.Load()
		repoURLs := make(map[string]string, len(cfg.Anvils))
		for name, anvil := range cfg.Anvils {
			out, gerr := exec.Command("git", "-C", anvil.Path, "remote", "get-url", "origin").Output()
			if gerr != nil {
				continue
			}
			owner, repo, perr := github.ParseRepoURL(strings.TrimSpace(string(out)))
			if perr != nil {
				continue
			}
			repoURLs[name] = fmt.Sprintf("https://github.com/%s/%s", owner, repo)
		}
		srv.SetAnvilRepoURLs(repoURLs)
	}

	// Expose each anvil's auto_dispatch_tag so the Hearth Apply-tag action
	// can resolve the configured label without trusting a frontend constant.
	// Anvils without a tag are omitted so the handler can use a simple
	// "ok" lookup.
	srv.SetAnvilDispatchTagLister(func() map[string]string {
		cfg := d.cfg.Load()
		out := make(map[string]string, len(cfg.Anvils))
		for name, anvil := range cfg.Anvils {
			if anvil.AutoDispatchTag != "" {
				out[name] = anvil.AutoDispatchTag
			}
		}
		return out
	})

	go func() {
		if err := srv.Start(ctx); err != nil && ctx.Err() == nil {
			d.logger.Error("web server stopped with error", "error", err)
		}
	}()
	d.logger.Info("hearth 2.0 web server started", "addr", addr, "users", len(users))
	return nil
}

// buildForgeChatRunner resolves the provider for the "forgechat" stage and
// returns a ClaudeRunner. Returns nil when no provider can be resolved (e.g.
// the daemon shipped without a provider chain) so the web layer falls back
// to a 503 response on /turn.
func (d *Daemon) buildForgeChatRunner() forgechat.Runner {
	cfg := d.cfg.Load()
	specs := config.ProvidersForStageWithAnvil(cfg.Settings, nil, "forgechat")
	if len(specs) == 0 {
		specs = cfg.Settings.Providers
	}
	providers := provider.FromConfig(specs)
	if len(providers) == 0 {
		providers = provider.Defaults()
	}
	if len(providers) == 0 {
		return nil
	}
	runner := forgechat.NewClaudeRunner(providers[0], cfg.Settings.ClaudeFlags)
	runner.Timeout = cfg.Settings.ForgeChat.ResolvedTurnTimeout()
	return runner
}

// webEnabled reports whether FORGE_WEB_ENABLED requests the web UI.
// Accepts the same truthy values as the existing FORGE_FOREGROUND check.
func webEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FORGE_WEB_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// sessionDurationEnv parses a Go duration (e.g. "168h", "7d" is NOT valid —
// use "168h") from the named environment variable. Returns 0 when the
// variable is unset or unparseable, letting web.New apply its default.
// Negative values are preserved so callers like SessionAbsoluteTTL can use
// them as a "disable" sentinel; callers that require positive durations
// (e.g. SessionTTL) must clamp at the call site.
func (d *Daemon) sessionDurationEnv(name string) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0
	}
	dur, err := time.ParseDuration(raw)
	if err != nil {
		d.logger.Warn("invalid duration in env var, using default", "var", name, "value", raw, "error", err)
		return 0
	}
	return dur
}

// cookieSecureEnv reports whether FORGE_WEB_COOKIE_SECURE forces the
// Secure cookie flag. Defaults to false so local development on plain
// HTTP works; production deployments behind HTTPS should set this to 1.
func cookieSecureEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FORGE_WEB_COOKIE_SECURE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
