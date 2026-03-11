package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/notify"
)

// newTestNotifier creates a Notifier pointed at the given URL with all events enabled.
func newTestNotifier(t *testing.T, webhookURL string) *notify.Notifier {
	t.Helper()
	return notify.NewNotifier(notify.Config{
		WebhookURL: webhookURL,
		Enabled:    true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// captureRequest starts a test server that captures the first incoming request body.
func captureRequest(t *testing.T) (serverURL string, body func() []byte) {
	t.Helper()
	ch := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		// Capture only the first request body without blocking on subsequent requests.
		select {
		case ch <- b:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []byte { return <-ch }
}

// TestReleasePublished_SendsAdaptiveCard verifies that ReleasePublished posts a
// Teams Adaptive Card containing version and release URL facts.
func TestReleasePublished_SendsAdaptiveCard(t *testing.T) {
	url, getBody := captureRequest(t)
	n := newTestNotifier(t, url)

	n.ReleasePublished(context.Background(), "v1.2.3", "v1.2.3",
		"https://github.com/org/repo/releases/tag/v1.2.3",
		"- Added release notifications")

	raw := getBody()
	if len(raw) == 0 {
		t.Fatal("expected a request body, got none")
	}

	// Body must be valid JSON
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
	}

	body := string(raw)
	for _, want := range []string{"v1.2.3", "Forge Release Published", "View on GitHub"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q\nbody: %s", want, body)
		}
	}
}

// TestReleasePublished_TagDiffersFromVersion verifies that when the tag differs
// from the version string, both values appear in the card body.
func TestReleasePublished_TagDiffersFromVersion(t *testing.T) {
	url, getBody := captureRequest(t)
	n := newTestNotifier(t, url)

	// version is bare (e.g. "2.0.0"), tag includes the "v" prefix ("v2.0.0")
	n.ReleasePublished(context.Background(), "2.0.0", "v2.0.0",
		"https://github.com/org/repo/releases/tag/v2.0.0", "")

	raw := getBody()
	if len(raw) == 0 {
		t.Fatal("expected a request body, got none")
	}

	body := string(raw)
	for _, want := range []string{"2.0.0", "v2.0.0", "Tag"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q\nbody: %s", want, body)
		}
	}
}

// TestReleasePublished_TruncatesLongChangelog verifies that changelogs longer
// than 500 runes are truncated safely without splitting multibyte characters.
func TestReleasePublished_TruncatesLongChangelog(t *testing.T) {
	url, getBody := captureRequest(t)
	n := newTestNotifier(t, url)

	// Build a changelog that is >500 runes using multibyte characters (€ = 3 bytes each).
	longEntry := strings.Repeat("€", 600) // 600 runes, 1800 bytes
	n.ReleasePublished(context.Background(), "v1.0.0", "v1.0.0", "", longEntry)

	body := string(getBody())
	if !strings.Contains(body, "...") {
		t.Error("expected truncated changelog to end with '...'")
	}
	// The truncated text must be valid JSON-encoded content (no mangled runes).
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("truncated body is not valid JSON: %v", err)
	}
}

// TestReleasePublished_SkippedWhenEventFiltered verifies that ReleasePublished
// does not send when the event is excluded from the filter list.
func TestReleasePublished_SkippedWhenEventFiltered(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	n := notify.NewNotifier(notify.Config{
		WebhookURL: srv.URL,
		Enabled:    true,
		Events:     []string{"pr_created"}, // release_published excluded
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n.ReleasePublished(context.Background(), "v1.0.0", "v1.0.0", "", "")
	if called {
		t.Error("expected no request when release_published is filtered out")
	}
}

// TestSendGenericRelease_PostsJSON verifies that SendGenericRelease sends the
// correct JSON structure to the target URL, including the rich payload fields.
func TestSendGenericRelease_PostsJSON(t *testing.T) {
	url, getBody := captureRequest(t)

	payload := notify.WebhookPayload{
		Source:  "forge",
		Summary: "Release published: v2.0.0 (repo)",
		Event:   "release_published",
		Detail:  "- Added something new",
		URL:     "https://github.com/org/repo/releases/tag/v2.0.0",
		Repo:    "repo",
		Version: "v2.0.0",
	}
	notify.SendGenericRelease(context.Background(), url, payload,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	raw := getBody()
	if len(raw) == 0 {
		t.Fatal("expected a request body, got none")
	}

	var got notify.WebhookPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
	}

	if got.Source != "forge" {
		t.Errorf("source = %q, want %q", got.Source, "forge")
	}
	if got.Event != "release_published" {
		t.Errorf("event = %q, want %q", got.Event, "release_published")
	}
	if got.Version != "v2.0.0" {
		t.Errorf("version = %q, want %q", got.Version, "v2.0.0")
	}
	if got.Summary != payload.Summary {
		t.Errorf("summary = %q, want %q", got.Summary, payload.Summary)
	}
	if got.URL != payload.URL {
		t.Errorf("url = %q, want %q", got.URL, payload.URL)
	}
}

// TestSendGenericRelease_TagFieldPreserved verifies that the Tag field in the
// payload is round-tripped correctly through JSON serialisation. This is a
// regression test for the silent breaking change where Tag was dropped from
// WebhookPayload even though --tag is a supported CLI flag with distinct
// semantics from --version (e.g. "2.0.0" vs "v2.0.0").
func TestSendGenericRelease_TagFieldPreserved(t *testing.T) {
	url, getBody := captureRequest(t)

	payload := notify.WebhookPayload{
		Source:  "forge",
		Summary: "Release published: 2.0.0 (repo)",
		Event:   "release_published",
		URL:     "https://github.com/org/repo/releases/tag/v2.0.0",
		Repo:    "repo",
		Version: "2.0.0",
		Tag:     "v2.0.0", // tag has "v" prefix, version does not
	}
	notify.SendGenericRelease(context.Background(), url, payload,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	raw := getBody()
	if len(raw) == 0 {
		t.Fatal("expected a request body, got none")
	}

	var got notify.WebhookPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
	}

	if got.Tag != "v2.0.0" {
		t.Errorf("tag = %q, want %q", got.Tag, "v2.0.0")
	}
	if got.Version != "2.0.0" {
		t.Errorf("version = %q, want %q", got.Version, "2.0.0")
	}
}

// TestSendGenericRelease_TagOmittedWhenEmpty verifies that the tag field is
// omitted from JSON output when it is the zero value, keeping payloads compact.
func TestSendGenericRelease_TagOmittedWhenEmpty(t *testing.T) {
	url, getBody := captureRequest(t)

	payload := notify.WebhookPayload{
		Source:  "forge",
		Summary: "Release published: v1.0.0",
		Event:   "release_published",
		Version: "v1.0.0",
		// Tag intentionally omitted
	}
	notify.SendGenericRelease(context.Background(), url, payload,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	raw := getBody()
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal response body: %v", err)
	}
	if _, ok := parsed["tag"]; ok {
		t.Error("expected 'tag' to be omitted from JSON when empty")
	}
}

// TestSendGenericRelease_EmptyURLIsNoop verifies that an empty URL does nothing.
func TestSendGenericRelease_EmptyURLIsNoop(t *testing.T) {
	// No panic / no crash — just a silent no-op.
	notify.SendGenericRelease(context.Background(), "", notify.WebhookPayload{
		Source:  "forge",
		Summary: "Release published: v1.0.0",
		Event:   "release_published",
		Version: "v1.0.0",
	}, nil)
}

// TestPRReadyToMerge_SendsAdaptiveCard verifies that PRReadyToMerge posts a
// Teams Adaptive Card containing the PR link (with URL), bead, anvil, and title.
func TestPRReadyToMerge_SendsAdaptiveCard(t *testing.T) {
	url, getBody := captureRequest(t)
	n := newTestNotifier(t, url)

	n.PRReadyToMerge(context.Background(), "my-anvil", "Forge-42", 7,
		"https://github.com/org/repo/pull/7", "Add widget support")

	raw := getBody()
	if len(raw) == 0 {
		t.Fatal("expected a request body, got none")
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
	}

	body := string(raw)
	for _, want := range []string{
		"PR Ready to Merge",
		"my-anvil",
		"Forge-42",
		"#7",
		"https://github.com/org/repo/pull/7",
		"Add widget support",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q\nbody: %s", want, body)
		}
	}
}

// TestPRReadyToMerge_PRLinkWithoutURL verifies that when no URL is provided,
// the PR is referenced by number only (no broken link).
func TestPRReadyToMerge_PRLinkWithoutURL(t *testing.T) {
	url, getBody := captureRequest(t)
	n := newTestNotifier(t, url)

	n.PRReadyToMerge(context.Background(), "anvil", "Forge-1", 3, "", "Some title")

	body := string(getBody())
	if strings.Contains(body, "](") {
		t.Error("expected no markdown link when prURL is empty")
	}
	if !strings.Contains(body, "#3") {
		t.Error("expected PR number to appear in body")
	}
}

// TestPRReadyToMerge_SkippedWhenEventFiltered verifies that PRReadyToMerge
// does not send when the event is excluded from the filter list.
func TestPRReadyToMerge_SkippedWhenEventFiltered(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	n := notify.NewNotifier(notify.Config{
		WebhookURL: srv.URL,
		Enabled:    true,
		Events:     []string{"pr_created"}, // pr_ready_to_merge excluded
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n.PRReadyToMerge(context.Background(), "anvil", "Forge-1", 1, "", "title")
	if called {
		t.Error("expected no request when pr_ready_to_merge is filtered out")
	}
}

// TestSendGenericPRReadyToMerge_PostsJSON verifies that SendGenericPRReadyToMerge
// sends the correct JSON structure to the target URL, including the rich payload fields.
func TestSendGenericPRReadyToMerge_PostsJSON(t *testing.T) {
	url, getBody := captureRequest(t)

	payload := notify.WebhookPayload{
		Source:  "forge",
		Summary: "PR #42 ready to merge: Fix all the things (my-anvil)",
		Event:   "pr_ready_to_merge",
		URL:     "https://github.com/org/repo/pull/42",
		Repo:    "my-anvil",
		Bead:    "Forge-99",
		PR:      42,
	}
	notify.SendGenericPRReadyToMerge(context.Background(), url, payload,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	raw := getBody()
	if len(raw) == 0 {
		t.Fatal("expected a request body, got none")
	}

	var got notify.WebhookPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
	}

	if got.Source != "forge" {
		t.Errorf("source = %q, want %q", got.Source, "forge")
	}
	if got.Event != "pr_ready_to_merge" {
		t.Errorf("event = %q, want %q", got.Event, "pr_ready_to_merge")
	}
	if got.Repo != "my-anvil" {
		t.Errorf("repo = %q, want %q", got.Repo, "my-anvil")
	}
	if got.Bead != "Forge-99" {
		t.Errorf("bead = %q, want %q", got.Bead, "Forge-99")
	}
	if got.PR != 42 {
		t.Errorf("pr = %d, want %d", got.PR, 42)
	}
	if got.URL != payload.URL {
		t.Errorf("url = %q, want %q", got.URL, payload.URL)
	}
	if got.Summary != payload.Summary {
		t.Errorf("summary = %q, want %q", got.Summary, payload.Summary)
	}
}

// TestSendGenericPRReadyToMerge_EmptyURLIsNoop verifies that an empty URL does nothing.
func TestSendGenericPRReadyToMerge_EmptyURLIsNoop(t *testing.T) {
	// No panic / no crash — just a silent no-op.
	notify.SendGenericPRReadyToMerge(context.Background(), "", notify.WebhookPayload{
		Source:  "forge",
		Summary: "PR #1 ready to merge",
		Event:   "pr_ready_to_merge",
		PR:      1,
	}, nil)
}

// TestShouldNotify_PRReadyToMerge verifies event filtering for pr_ready_to_merge.
func TestShouldNotify_PRReadyToMerge(t *testing.T) {
	all := notify.NewNotifier(notify.Config{
		WebhookURL: "https://example.webhook.office.com/fake",
		Enabled:    true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	filtered := notify.NewNotifier(notify.Config{
		WebhookURL: "https://example.webhook.office.com/fake",
		Enabled:    true,
		Events:     []string{"pr_created"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !all.ShouldNotify(notify.EventPRReadyToMerge) {
		t.Error("all-events notifier should notify pr_ready_to_merge")
	}
	if filtered.ShouldNotify(notify.EventPRReadyToMerge) {
		t.Error("filtered notifier should NOT notify pr_ready_to_merge")
	}
}

// TestWebhookDispatcher_RespectsEnabledFlagViaConstruction verifies that when
// NewWebhookDispatcher is called with no targets (as happens when the daemon
// skips building the target list due to notifications.enabled=false), the
// returned dispatcher is nil and therefore silently drops all events.
func TestWebhookDispatcher_NilIsNoop(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// NewWebhookDispatcher with no valid targets returns nil.
	var d *notify.WebhookDispatcher // simulates disabled state — daemon passes nil
	d.Dispatch(context.Background(), notify.EventPRCreated, "bead-1", "my-anvil", "test")
	if called {
		t.Error("nil dispatcher should be a no-op, but the server was called")
	}
}

// TestWebhookDispatcher_EventDailyCost verifies that the dispatcher delivers
// daily_cost events to subscribed webhook targets.
func TestWebhookDispatcher_EventDailyCost(t *testing.T) {
	url, getBody := captureRequest(t)

	d := notify.NewWebhookDispatcher([]notify.WebhookTarget{
		{Name: "test", URL: url, Events: []string{"daily_cost"}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	d.Dispatch(context.Background(), notify.EventDailyCost, "", "", "Daily cost $12.34 reached limit $50.00")

	raw := getBody()
	if len(raw) == 0 {
		t.Fatal("expected a request body, got none")
	}
	var got notify.GenericPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
	}
	if got.EventType != "daily_cost" {
		t.Errorf("event_type = %q, want %q", got.EventType, "daily_cost")
	}
}

// TestWebhookDispatcher_EventBeadDecomposed verifies that the dispatcher
// delivers bead_decomposed events to subscribed webhook targets.
func TestWebhookDispatcher_EventBeadDecomposed(t *testing.T) {
	url, getBody := captureRequest(t)

	d := notify.NewWebhookDispatcher([]notify.WebhookTarget{
		{Name: "test", URL: url, Events: []string{"bead_decomposed"}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	d.Dispatch(context.Background(), notify.EventBeadDecomposed, "Forge-99", "my-anvil", "Bead decomposed into 3 sub-beads")

	raw := getBody()
	if len(raw) == 0 {
		t.Fatal("expected a request body, got none")
	}
	var got notify.GenericPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
	}
	if got.EventType != "bead_decomposed" {
		t.Errorf("event_type = %q, want %q", got.EventType, "bead_decomposed")
	}
	if got.BeadID != "Forge-99" {
		t.Errorf("bead_id = %q, want %q", got.BeadID, "Forge-99")
	}
	if got.Anvil != "my-anvil" {
		t.Errorf("anvil = %q, want %q", got.Anvil, "my-anvil")
	}
}

// TestWebhookDispatcher_FilteredEventsNotDelivered verifies that when a target
// subscribes to specific events, unsubscribed events are not delivered.
func TestWebhookDispatcher_FilteredEventsNotDelivered(t *testing.T) {
	// Use a buffered channel so the server doesn't block if it fires unexpectedly.
	ch := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case ch <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	d := notify.NewWebhookDispatcher([]notify.WebhookTarget{
		{Name: "test", URL: srv.URL, Events: []string{"pr_created"}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Dispatch an event not in the filter — the dispatcher must skip it without
	// spawning a goroutine, so the check below is synchronous-safe.
	d.Dispatch(context.Background(), notify.EventBeadDecomposed, "Forge-1", "anvil", "msg")

	select {
	case <-ch:
		t.Error("dispatcher should not deliver events not in the target's filter")
	default:
		// Correct: no request was made.
	}
}

// TestWebhookDispatcher_DeliversAfterCallerContextCancelled verifies that
// Dispatch still delivers webhooks even when the caller cancels its context
// immediately after Dispatch returns (the common "defer cancel()" pattern in
// the daemon). This is a regression test for the context cancellation race.
func TestWebhookDispatcher_DeliversAfterCallerContextCancelled(t *testing.T) {
	ch := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case ch <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	d := notify.NewWebhookDispatcher([]notify.WebhookTarget{
		{Name: "test", URL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Simulate the daemon pattern: create a context, dispatch, then cancel
	// immediately — as if the caller did "defer cancel()" and then returned.
	ctx, cancel := context.WithCancel(context.Background())
	d.Dispatch(ctx, notify.EventPRCreated, "Forge-1", "anvil", "test")
	cancel() // cancel immediately, before the goroutine can complete the HTTP request

	select {
	case <-ch:
		// Correct: webhook was delivered despite caller context cancellation.
	case <-time.After(5 * time.Second):
		t.Error("webhook was not delivered after caller context was cancelled — context cancellation race not fixed")
	}
}

// TestShouldNotify_ReleasePublished verifies event filtering for the new event type.
func TestShouldNotify_ReleasePublished(t *testing.T) {
	all := notify.NewNotifier(notify.Config{
		WebhookURL: "https://example.webhook.office.com/fake",
		Enabled:    true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	filtered := notify.NewNotifier(notify.Config{
		WebhookURL: "https://example.webhook.office.com/fake",
		Enabled:    true,
		Events:     []string{"pr_created"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !all.ShouldNotify(notify.EventReleasePublished) {
		t.Error("all-events notifier should notify release_published")
	}
	if filtered.ShouldNotify(notify.EventReleasePublished) {
		t.Error("filtered notifier should NOT notify release_published")
	}
}
