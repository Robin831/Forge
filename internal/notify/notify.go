// Package notify sends notifications to Microsoft Teams via incoming webhooks.
//
// Notifications are formatted as Adaptive Cards (schema v1.4) for rich display
// in Teams channels. Each event type has a dedicated card template.
//
// Disabled by default — configure notifications.teams_webhook_url in forge.yaml.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// AdaptiveCardSchema is the schema URL for adaptive cards.
	AdaptiveCardSchema = "http://adaptivecards.io/schemas/adaptive-card.json"

	// CardVersion is the adaptive card version.
	CardVersion = "1.4"

	// Timeout for webhook HTTP calls.
	webhookTimeout = 10 * time.Second
)

// EventType identifies a notification event.
type EventType string

const (
	EventPRCreated   EventType = "pr_created"
	EventBeadFailed  EventType = "bead_failed"
	EventDailyCost   EventType = "daily_cost"
	EventWorkerDone  EventType = "worker_done"
)

// Notifier sends notifications to Teams.
type Notifier struct {
	webhookURL string
	enabled    bool
	events     map[EventType]bool // Empty = all events
	logger     *slog.Logger
	client     *http.Client
}

// Config holds notifier settings.
type Config struct {
	WebhookURL string
	Enabled    bool
	Events     []string // Filter to these events only; empty = all
}

// NewNotifier creates a Teams notifier. Returns nil if disabled or no URL.
func NewNotifier(cfg Config, logger *slog.Logger) *Notifier {
	if !cfg.Enabled || cfg.WebhookURL == "" {
		return nil
	}

	events := make(map[EventType]bool)
	for _, e := range cfg.Events {
		events[EventType(e)] = true
	}

	return &Notifier{
		webhookURL: cfg.WebhookURL,
		enabled:    true,
		events:     events,
		logger:     logger,
		client:     &http.Client{Timeout: webhookTimeout},
	}
}

// ShouldNotify checks if an event type should trigger a notification.
func (n *Notifier) ShouldNotify(event EventType) bool {
	if n == nil || !n.enabled {
		return false
	}
	// Empty filter = all events
	if len(n.events) == 0 {
		return true
	}
	return n.events[event]
}

// PRCreated sends a notification when a PR is created.
func (n *Notifier) PRCreated(ctx context.Context, anvil, beadID string, prNumber int, prURL, title string) {
	if !n.ShouldNotify(EventPRCreated) {
		return
	}

	card := adaptiveCard(
		"🔨 PR Created",
		"good",
		[]cardFact{
			{Title: "Anvil", Value: anvil},
			{Title: "Bead", Value: beadID},
			{Title: "PR", Value: fmt.Sprintf("[#%d](%s)", prNumber, prURL)},
			{Title: "Title", Value: title},
		},
	)

	n.send(ctx, card)
}

// BeadFailed sends a notification when a bead fails after retries.
func (n *Notifier) BeadFailed(ctx context.Context, anvil, beadID string, retries int, lastError string) {
	if !n.ShouldNotify(EventBeadFailed) {
		return
	}

	// Truncate error if too long
	if len(lastError) > 200 {
		lastError = lastError[:200] + "..."
	}

	card := adaptiveCard(
		"❌ Bead Failed (needs human)",
		"attention",
		[]cardFact{
			{Title: "Anvil", Value: anvil},
			{Title: "Bead", Value: beadID},
			{Title: "Retries", Value: fmt.Sprintf("%d", retries)},
			{Title: "Error", Value: lastError},
		},
	)

	n.send(ctx, card)
}

// DailyCost sends a daily cost summary.
func (n *Notifier) DailyCost(ctx context.Context, date string, totalCost float64, limit float64, inputTokens, outputTokens int64) {
	if !n.ShouldNotify(EventDailyCost) {
		return
	}

	limitStr := "none"
	if limit > 0 {
		limitStr = fmt.Sprintf("$%.2f", limit)
		if totalCost > limit {
			limitStr += " ⚠️ EXCEEDED"
		}
	}

	card := adaptiveCard(
		"💰 Daily Cost Summary",
		"default",
		[]cardFact{
			{Title: "Date", Value: date},
			{Title: "Total Cost", Value: fmt.Sprintf("$%.4f", totalCost)},
			{Title: "Budget", Value: limitStr},
			{Title: "Input", Value: formatTokens(inputTokens)},
			{Title: "Output", Value: formatTokens(outputTokens)},
		},
	)

	n.send(ctx, card)
}

// WorkerDone sends a notification when a worker successfully completes.
func (n *Notifier) WorkerDone(ctx context.Context, anvil, beadID, workerID string, duration time.Duration) {
	if !n.ShouldNotify(EventWorkerDone) {
		return
	}

	card := adaptiveCard(
		"✅ Worker Completed",
		"good",
		[]cardFact{
			{Title: "Anvil", Value: anvil},
			{Title: "Bead", Value: beadID},
			{Title: "Worker", Value: workerID},
			{Title: "Duration", Value: duration.Round(time.Second).String()},
		},
	)

	n.send(ctx, card)
}

// --- Adaptive Card helpers ---

type cardFact struct {
	Title string
	Value string
}

// adaptiveCard builds a Teams Adaptive Card message payload.
func adaptiveCard(title, style string, facts []cardFact) map[string]any {
	factItems := make([]map[string]any, len(facts))
	for i, f := range facts {
		factItems[i] = map[string]any{
			"title": f.Title,
			"value": f.Value,
		}
	}

	return map[string]any{
		"type": "message",
		"attachments": []map[string]any{
			{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"contentUrl":  nil,
				"content": map[string]any{
					"$schema": AdaptiveCardSchema,
					"type":    "AdaptiveCard",
					"version": CardVersion,
					"body": []map[string]any{
						{
							"type":   "TextBlock",
							"size":   "Medium",
							"weight": "Bolder",
							"text":   title,
							"style":  style,
						},
						{
							"type":  "FactSet",
							"facts": factItems,
						},
					},
				},
			},
		},
	}
}

// send posts the card payload to Teams.
func (n *Notifier) send(ctx context.Context, payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		n.logger.Error("failed to marshal notification", "error", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(body))
	if err != nil {
		n.logger.Error("failed to create request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		n.logger.Warn("teams webhook failed", "error", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		n.logger.Warn("teams webhook returned error", "status", resp.StatusCode)
		return
	}

	n.logger.Debug("teams notification sent", "status", resp.StatusCode)
}

// formatTokens formats token counts with K/M suffixes.
func formatTokens(count int64) string {
	switch {
	case count >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	case count >= 1_000:
		return fmt.Sprintf("%.1fK", float64(count)/1_000)
	default:
		return fmt.Sprintf("%d", count)
	}
}

// FormatWebhookURL validates and normalises a Teams webhook URL.
func FormatWebhookURL(url string) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", fmt.Errorf("webhook URL is empty")
	}
	if !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("webhook URL must use HTTPS")
	}
	return url, nil
}
