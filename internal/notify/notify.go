// Package notify sends notifications to webhook endpoints.
//
// Teams webhooks receive rich Adaptive Cards (schema v1.4) formatted for display
// in Teams channels. Generic JSON payloads are also supported for custom
// receivers such as dashboards and Slack integrations (see SendGenericRelease).
//
// Disabled by default — configure notifications.teams_webhook_url and/or
// notifications.release_webhook_urls (notifications.*) in forge.yaml.
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
	"sync"
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
	EventPRCreated        EventType = "pr_created"
	EventBeadFailed       EventType = "bead_failed"
	EventDailyCost        EventType = "daily_cost"
	EventWorkerDone       EventType = "worker_done"
	EventBeadDecomposed   EventType = "bead_decomposed"
	EventReleasePublished EventType = "release_published"
	EventPRReadyToMerge   EventType = "pr_ready_to_merge"
	// EventRelease is the event type used in generic webhook event filters for
	// release notifications. Teams webhooks use EventReleasePublished and
	// receive Adaptive Cards; generic webhooks use EventRelease and receive a
	// simple GenericPayload.
	EventRelease EventType = "release"
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

// SubBead holds the ID and title of a sub-bead for decomposition notifications.
type SubBead struct {
	ID    string
	Title string
}

// BeadDecomposed sends a notification when Schematic decomposes a bead into
// sub-beads. This is significant because it changes the work queue.
func (n *Notifier) BeadDecomposed(ctx context.Context, anvil, beadID, beadTitle string, subBeads []SubBead) {
	if !n.ShouldNotify(EventBeadDecomposed) {
		return
	}

	var lines []string
	for _, sb := range subBeads {
		lines = append(lines, fmt.Sprintf("• **%s** — %s", sb.ID, sb.Title))
	}

	card := adaptiveCard(
		"🔧 Bead Decomposed",
		"accent",
		[]cardFact{
			{Title: "Anvil", Value: anvil},
			{Title: "Parent", Value: fmt.Sprintf("%s — %s", beadID, beadTitle)},
			{Title: "Sub-beads", Value: fmt.Sprintf("%d created", len(subBeads))},
			{Title: "Details", Value: strings.Join(lines, "\n")},
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

// ReleasePublished sends a notification when a new Forge release is published.
func (n *Notifier) ReleasePublished(ctx context.Context, version, tag, releaseURL, changelogSummary string) {
	if !n.ShouldNotify(EventReleasePublished) {
		return
	}

	facts := []cardFact{
		{Title: "Version", Value: version},
	}
	if tag != "" && tag != version {
		facts = append(facts, cardFact{Title: "Tag", Value: tag})
	}
	if releaseURL != "" {
		facts = append(facts, cardFact{Title: "Release", Value: fmt.Sprintf("[View on GitHub](%s)", releaseURL)})
	}
	if changelogSummary != "" {
		runes := []rune(changelogSummary)
		if len(runes) > 500 {
			changelogSummary = string(runes[:497]) + "..."
		}
		facts = append(facts, cardFact{Title: "Changes", Value: changelogSummary})
	}

	card := adaptiveCard("🚀 Forge Release Published", "good", facts)
	n.send(ctx, card)
}

// PRReadyToMerge sends a notification when a PR is ready to merge (CI passing
// and warden-approved with no blocking reviews or conflicts).
func (n *Notifier) PRReadyToMerge(ctx context.Context, anvil, beadID string, prNumber int, prURL, title string) {
	if !n.ShouldNotify(EventPRReadyToMerge) {
		return
	}

	prLink := fmt.Sprintf("#%d", prNumber)
	if prURL != "" {
		prLink = fmt.Sprintf("[#%d](%s)", prNumber, prURL)
	}

	card := adaptiveCard(
		"✅ PR Ready to Merge",
		"good",
		[]cardFact{
			{Title: "Anvil", Value: anvil},
			{Title: "Bead", Value: beadID},
			{Title: "PR", Value: prLink},
			{Title: "Title", Value: title},
		},
	)

	n.send(ctx, card)
}

// WebhookPayload is the canonical generic JSON structure sent to non-Teams webhook URLs.
// It provides a pre-formatted summary and structured metadata so receivers can
// display rich notifications without parsing event-specific fields.
//
// All generic/non-Teams Forge webhook POSTs use this schema. Teams webhooks instead
// receive Adaptive Card JSON. The source field is always "forge" so receivers can
// identify the origin and apply the appropriate badge or routing.
type WebhookPayload struct {
	Source  string `json:"source"`            // Always "forge"
	Summary string `json:"summary"`           // Pre-formatted human-readable one-liner for list view
	Event   string `json:"event"`             // Machine-readable event type (snake_case)
	Detail  string `json:"detail,omitempty"`  // Secondary description (changelog, commit message, etc.)
	URL     string `json:"url,omitempty"`     // Relevant link (PR, release, issue, etc.)
	Repo    string `json:"repo,omitempty"`    // Repository / anvil name
	Version string `json:"version,omitempty"` // Version string (may differ from Tag, e.g. "2.0.0" vs "v2.0.0")
	Tag     string `json:"tag,omitempty"`     // Git tag if applicable (may include "v" prefix)
	Bead    string `json:"bead,omitempty"`    // Bead ID if the event relates to a bead
	PR      int    `json:"pr,omitempty"`      // PR number if applicable
}

// SendGenericPRReadyToMerge posts a generic JSON pr_ready_to_merge payload to any webhook URL.
// This is used for non-Teams endpoints such as dashboards or custom receivers.
// Errors are logged but do not cause a fatal failure.
func SendGenericPRReadyToMerge(ctx context.Context, webhookURL string, payload WebhookPayload, logger *slog.Logger) {
	sendGenericWebhook(ctx, webhookURL, payload, "pr_ready_to_merge", logger)
}

// SendGenericRelease posts a generic JSON release payload to any webhook URL.
// This is used for non-Teams endpoints such as dashboards or custom receivers.
// Errors are logged but do not cause a fatal failure.
func SendGenericRelease(ctx context.Context, webhookURL string, payload WebhookPayload, logger *slog.Logger) {
	sendGenericWebhook(ctx, webhookURL, payload, payload.Event, logger)
}

// sendGenericWebhook marshals payload and POSTs it to webhookURL.
// eventLabel is used only in log messages to identify which event type failed.
func sendGenericWebhook(ctx context.Context, webhookURL string, payload WebhookPayload, eventLabel string, logger *slog.Logger) {
	if webhookURL == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		if logger != nil {
			logger.Error("failed to marshal webhook payload", "event", eventLabel, "error", err)
		}
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		if logger != nil {
			logger.Error("failed to create webhook request", "event", eventLabel, "url", webhookURL, "error", err)
		}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forge-Event", eventLabel)

	client := &http.Client{Timeout: webhookTimeout}
	resp, err := client.Do(req)
	if err != nil {
		if logger != nil {
			logger.Warn("webhook POST failed", "event", eventLabel, "url", webhookURL, "error", err)
		}
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		if logger != nil {
			logger.Warn("webhook returned error status", "event", eventLabel, "url", webhookURL, "status", resp.StatusCode)
		}
		return
	}

	if logger != nil {
		logger.Debug("webhook notification sent", "event", eventLabel, "url", webhookURL, "status", resp.StatusCode)
	}
}

// --- Generic webhook dispatcher ---

// WebhookTarget represents a single generic JSON webhook target.
// It is the notify-package counterpart of config.WebhookTargetConfig.
type WebhookTarget struct {
	Name   string
	URL    string
	Events []string // Empty = subscribe to all events
}

// GenericPayload is the uniform JSON payload sent to generic (non-Teams) webhook
// targets. Unlike the Teams Adaptive Cards, every event produces the same
// structure so receivers can handle all events with a single schema.
type GenericPayload struct {
	EventType string    `json:"event_type"`
	BeadID    string    `json:"bead_id,omitempty"`
	Anvil     string    `json:"anvil,omitempty"`
	Message   string    `json:"message,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type dispatchTarget struct {
	name   string
	url    string
	events map[EventType]bool // empty = all events
}

// WebhookDispatcher sends events to configured generic JSON webhook targets.
// Each target can subscribe to a specific subset of events.
// A nil WebhookDispatcher is safe to use — all methods become no-ops.
type WebhookDispatcher struct {
	targets []dispatchTarget
	logger  *slog.Logger
	client  *http.Client
	wg      sync.WaitGroup
}

// NewWebhookDispatcher creates a dispatcher from a list of webhook targets.
// Returns nil if no valid targets are configured (empty list or all URLs empty).
func NewWebhookDispatcher(targets []WebhookTarget, logger *slog.Logger) *WebhookDispatcher {
	var dts []dispatchTarget
	for _, t := range targets {
		url := strings.TrimSpace(t.URL)
		if url == "" {
			continue
		}
		events := make(map[EventType]bool)
		for _, e := range t.Events {
			event := strings.TrimSpace(e)
			if event != "" {
				events[EventType(event)] = true
			}
		}
		dts = append(dts, dispatchTarget{
			name:   t.Name,
			url:    url,
			events: events,
		})
	}
	if len(dts) == 0 {
		return nil
	}
	return &WebhookDispatcher{
		targets: dts,
		logger:  logger,
		client:  &http.Client{Timeout: webhookTimeout},
	}
}

// Dispatch sends a GenericPayload to all webhook targets that subscribe to the
// given event. Each delivery is dispatched in its own goroutine so the caller
// is never blocked by a slow or unreachable webhook.
//
// Dispatch detaches from the caller's context via context.WithoutCancel so
// that goroutines are not cancelled when the caller cancels its own context
// (e.g. via a deferred cancel()). Context values are preserved for tracing.
// The HTTP client's Timeout enforces the delivery deadline.
func (d *WebhookDispatcher) Dispatch(ctx context.Context, event EventType, beadID, anvil, message string) {
	if d == nil {
		return
	}
	payload := GenericPayload{
		EventType: string(event),
		BeadID:    beadID,
		Anvil:     anvil,
		Message:   message,
		Timestamp: time.Now().UTC(),
	}
	// Detach from caller's context to prevent a cancellation race: Dispatch is
	// fire-and-forget and the caller's deferred cancel() fires as soon as this
	// function returns, before the HTTP goroutines have a chance to complete.
	sendCtx := context.WithoutCancel(ctx)
	for _, t := range d.targets {
		if len(t.events) > 0 && !t.events[event] {
			continue
		}
		t := t // capture loop variable for goroutine
		d.wg.Add(1)
		go func(target dispatchTarget) {
			defer d.wg.Done()
			d.sendToTarget(sendCtx, target, payload)
		}(t)
	}
}

// Wait blocks until all dispatched webhooks have completed their HTTP requests.
func (d *WebhookDispatcher) Wait() {
	if d == nil {
		return
	}
	d.wg.Wait()
}

func (d *WebhookDispatcher) sendToTarget(ctx context.Context, t dispatchTarget, payload GenericPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		if d.logger != nil {
			d.logger.Error("failed to marshal webhook payload", "webhook", t.name, "error", err)
		}
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		if d.logger != nil {
			d.logger.Error("failed to create webhook request", "webhook", t.name, "error", err)
		}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forge-Event", payload.EventType)

	resp, err := d.client.Do(req)
	if err != nil {
		if d.logger != nil {
			d.logger.Warn("webhook delivery failed", "webhook", t.name, "url", t.url, "error", err)
		}
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		if d.logger != nil {
			d.logger.Warn("webhook returned error status", "webhook", t.name, "url", t.url, "status", resp.StatusCode)
		}
		return
	}

	if d.logger != nil {
		d.logger.Debug("webhook notification sent", "webhook", t.name, "event", string(payload.EventType), "status", resp.StatusCode)
	}
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
