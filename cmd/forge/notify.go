package main

import (
	"fmt"
	"log/slog"
	"os"
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

		// Resolve Teams webhook URL: flag > env var > config
		teamsURL := webhookURLFlag
		if teamsURL == "" {
			teamsURL = os.Getenv("FORGE_NOTIFICATIONS_TEAMS_WEBHOOK_URL")
		}
		if teamsURL == "" && cfg != nil {
			teamsURL = cfg.Notifications.TeamsWebhookURL
		}

		// Collect generic webhook URLs: flag + config
		allExtraURLs := append([]string{}, extraURLs...)
		if cfg != nil {
			allExtraURLs = append(allExtraURLs, cfg.Notifications.ReleaseWebhookURLs...)
		}
		// Also check env var for a single generic webhook URL
		if envURL := os.Getenv("FORGE_RELEASE_WEBHOOK_URL"); envURL != "" {
			allExtraURLs = append(allExtraURLs, envURL)
		}

		sent := 0

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
			sent++
			if !jsonOutput {
				fmt.Printf("Notified Teams webhook (%s)\n", version)
			}
		}

		// Send generic JSON to extra URLs
		payload := notify.ReleasePayload{
			Event:            "release_published",
			Version:          version,
			Tag:              tag,
			ReleaseURL:       releaseURL,
			ChangelogSummary: changelogSummary,
		}
		for _, u := range allExtraURLs {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			notify.SendGenericRelease(rootCtx, u, payload, logger)
			sent++
			if !jsonOutput {
				fmt.Printf("Notified webhook: %s (%s)\n", u, version)
			}
		}

		if sent == 0 {
			fmt.Fprintln(os.Stderr, "No webhook URLs configured. Set notifications.teams_webhook_url or notifications.release_webhook_urls in forge.yaml, or use --webhook-url / --extra-url flags.")
		}

		if jsonOutput {
			fmt.Printf(`{"sent":%d,"version":%q}`+"\n", sent, version)
		}

		return nil
	},
}
