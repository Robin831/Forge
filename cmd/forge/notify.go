package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/Robin831/Forge/internal/notify"
	"github.com/spf13/cobra"
)

func init() {
	// notify release flags
	notifyReleaseCmd.Flags().String("version", "", "Release version (e.g. v0.3.1)")
	notifyReleaseCmd.Flags().String("tag", "", "Git tag (defaults to --version if omitted)")
	notifyReleaseCmd.Flags().String("release-url", "", "URL to the GitHub release page")
	notifyReleaseCmd.Flags().String("changelog", "", "Short changelog summary to include in the notification")
	notifyReleaseCmd.Flags().String("webhook-url", "", "Teams webhook URL (overrides notifications.teams_webhook_url in config)")
	notifyReleaseCmd.Flags().StringArray("extra-url", nil, "Additional generic-JSON webhook URL(s) to notify (repeatable)")
	_ = notifyReleaseCmd.MarkFlagRequired("version")

	notifyCmd.AddCommand(notifyReleaseCmd)
	rootCmd.AddCommand(notifyCmd)
}

// repoNameFromURL extracts the repository name from a GitHub URL.
// repoNameFromURL extracts the repository name from the second path segment of any URL.
// e.g. "https://github.com/Robin831/Forge/releases/tag/v1.0.0" → "Forge"
// The host is not validated; any URL with path /<owner>/<repo>/... is accepted.
// Returns an empty string if the URL cannot be parsed or has fewer than two path segments.
func repoNameFromURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	// Path looks like /<owner>/<repo>/...
	parts := strings.SplitN(strings.TrimPrefix(path.Clean(u.Path), "/"), "/", 3)
	if len(parts) >= 2 && parts[1] != "" {
		return parts[1]
	}
	return ""
}

var notifyCmd = &cobra.Command{
	Use:     "notify",
	Short:   "Send Forge notifications",
	Long:    `Send notifications to configured webhooks (Teams, custom dashboards, etc.).`,
	GroupID: "config",
}

var notifyReleaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Notify webhooks of a new Forge release",
	Long: `Sends a release notification to configured webhook endpoints.

Teams webhooks receive a rich Adaptive Card with version, tag, release URL, and
changelog summary. Additional URLs (--extra-url or notifications.release_webhook_urls
in forge.yaml) receive a generic JSON payload suitable for dashboards.

Example (from a release script):
  forge notify release \
    --version v0.3.1 \
    --release-url https://github.com/org/forge/releases/tag/v0.3.1 \
    --changelog "- Added release notifications\n- Fixed warden rules"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		version, _ := cmd.Flags().GetString("version")
		tag, _ := cmd.Flags().GetString("tag")
		releaseURL, _ := cmd.Flags().GetString("release-url")
		changelogSummary, _ := cmd.Flags().GetString("changelog")
		webhookURLFlag, _ := cmd.Flags().GetString("webhook-url")
		extraURLs, _ := cmd.Flags().GetStringArray("extra-url")

		if tag == "" {
			tag = version
		}

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))

		// Resolve Teams webhook URL: flag > env var > config (only if notifications are enabled)
		teamsURL := webhookURLFlag
		if teamsURL == "" {
			teamsURL = os.Getenv("FORGE_NOTIFICATIONS_TEAMS_WEBHOOK_URL")
		}
		if teamsURL == "" && cfg != nil && cfg.Notifications.Enabled {
			teamsURL = cfg.Notifications.TeamsWebhookURL
		}

		// Collect generic webhook URLs: flag + config (only if notifications are enabled)
		allExtraURLs := append([]string{}, extraURLs...)
		if cfg != nil && cfg.Notifications.Enabled {
			allExtraURLs = append(allExtraURLs, cfg.Notifications.ReleaseWebhookURLs...)
		}
		// Also check env var for a single generic webhook URL
		if envURL := os.Getenv("FORGE_RELEASE_WEBHOOK_URL"); envURL != "" {
			allExtraURLs = append(allExtraURLs, envURL)
		}

		attempted := 0

		// Send Teams Adaptive Card
		if teamsURL != "" {
			formatted, err := notify.FormatWebhookURL(strings.TrimSpace(teamsURL))
			if err != nil {
				return fmt.Errorf("invalid Teams webhook URL: %w", err)
			}
			n := notify.NewNotifier(notify.Config{
				WebhookURL: formatted,
				Enabled:    true,
			}, logger)
			n.ReleasePublished(rootCtx, version, tag, releaseURL, changelogSummary)
			attempted++
			if !jsonOutput {
				fmt.Printf("Attempted Teams webhook notification (%s)\n", version)
			}
		}

		// Send generic JSON to extra URLs
		repoName := repoNameFromURL(releaseURL)
		summary := fmt.Sprintf("Release published: %s", version)
		if repoName != "" {
			summary = fmt.Sprintf("Release published: %s (%s)", version, repoName)
		}
		payload := notify.WebhookPayload{
			Source:  "forge",
			Summary: summary,
			Event:   "release_published",
			Detail:  changelogSummary,
			URL:     releaseURL,
			Repo:    repoName,
			Version: version,
			Tag:     tag,
		}
		for _, u := range allExtraURLs {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			notify.SendGenericRelease(rootCtx, u, payload, logger)
			attempted++
			if !jsonOutput {
				fmt.Printf("Attempted webhook notification: %s (%s)\n", u, version)
			}
		}

		// Send GenericPayload to new webhooks[] targets that subscribe to the
		// "release" event (new config style).
		if cfg != nil && cfg.Notifications.Enabled && len(cfg.Notifications.Webhooks) > 0 {
			var targets []notify.WebhookTarget
			for _, w := range cfg.Notifications.Webhooks {
				targets = append(targets, notify.WebhookTarget{
					Name:   w.Name,
					URL:    w.URL,
					Events: w.Events,
				})
			}
			dispatcher := notify.NewWebhookDispatcher(targets, logger)
			if dispatcher != nil {
				msg := fmt.Sprintf("Forge %s released", version)
				if releaseURL != "" {
					msg = fmt.Sprintf("Forge %s released: %s", version, releaseURL)
				}
				dispatcher.Dispatch(rootCtx, notify.EventRelease, "", "", msg)
				// Count dispatched targets that subscribe to release
				for _, w := range cfg.Notifications.Webhooks {
					subscribes := len(w.Events) == 0
					for _, e := range w.Events {
						if e == string(notify.EventRelease) {
							subscribes = true
							break
						}
					}
					if subscribes && w.URL != "" {
						attempted++
						if !jsonOutput {
							fmt.Printf("Attempted webhook notification: %s [%s] (%s)\n", w.Name, w.URL, version)
						}
					}
				}
			}
		}

		if attempted == 0 {
			fmt.Fprintln(os.Stderr, "No webhook URLs configured. Set notifications.teams_webhook_url, notifications.release_webhook_urls, or notifications.webhooks in forge.yaml, or use --webhook-url / --extra-url flags.")
		}

		if jsonOutput {
			fmt.Printf(`{"attempted":%d,"version":%q}`+"\n", attempted, version)
		}

		return nil
	},
}
