package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"

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

	go func() {
		if err := srv.Start(ctx); err != nil && ctx.Err() == nil {
			d.logger.Error("web server stopped with error", "error", err)
		}
	}()
	d.logger.Info("hearth 2.0 web server started", "addr", addr, "users", len(users))
	return nil
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
