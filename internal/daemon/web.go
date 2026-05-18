package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/forgechat"
	"github.com/Robin831/Forge/internal/provider"
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

	srv, err := web.New(web.Config{
		Addr:         addr,
		Users:        users,
		CookieSecure: cookieSecureEnv(),
	}, d.db, d.handleIPC, d.logger)
	if err != nil {
		return fmt.Errorf("constructing web server: %w", err)
	}

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
	return forgechat.NewClaudeRunner(providers[0], cfg.Settings.ClaudeFlags)
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
