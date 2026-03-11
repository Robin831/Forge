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
// correct JSON structure to the target URL.
func TestSendGenericRelease_PostsJSON(t *testing.T) {
	url, getBody := captureRequest(t)

	payload := notify.ReleasePayload{
		Event:            "release_published",
		Version:          "v2.0.0",
		Tag:              "v2.0.0",
		ReleaseURL:       "https://github.com/org/repo/releases/tag/v2.0.0",
		ChangelogSummary: "- Added something new",
	}
	notify.SendGenericRelease(context.Background(), url, payload,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	raw := getBody()
	if len(raw) == 0 {
		t.Fatal("expected a request body, got none")
	}

	var got notify.ReleasePayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
	}

	if got.Event != "release_published" {
		t.Errorf("event = %q, want %q", got.Event, "release_published")
	}
	if got.Version != "v2.0.0" {
		t.Errorf("version = %q, want %q", got.Version, "v2.0.0")
	}
	if got.Tag != "v2.0.0" {
		t.Errorf("tag = %q, want %q", got.Tag, "v2.0.0")
	}
	if got.ReleaseURL != payload.ReleaseURL {
		t.Errorf("release_url = %q, want %q", got.ReleaseURL, payload.ReleaseURL)
	}
}

// TestSendGenericRelease_EmptyURLIsNoop verifies that an empty URL does nothing.
func TestSendGenericRelease_EmptyURLIsNoop(t *testing.T) {
	// No panic / no crash — just a silent no-op.
	notify.SendGenericRelease(context.Background(), "", notify.ReleasePayload{
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
// sends the correct JSON structure to the target URL.
func TestSendGenericPRReadyToMerge_PostsJSON(t *testing.T) {
	url, getBody := captureRequest(t)

	payload := notify.PRReadyToMergePayload{
		Event:    "pr_ready_to_merge",
		Anvil:    "my-anvil",
		BeadID:   "Forge-99",
		PRNumber: 42,
		PRURL:    "https://github.com/org/repo/pull/42",
		PRTitle:  "Fix all the things",
	}
	notify.SendGenericPRReadyToMerge(context.Background(), url, payload,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	raw := getBody()
	if len(raw) == 0 {
		t.Fatal("expected a request body, got none")
	}

	var got notify.PRReadyToMergePayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, raw)
	}

	if got.Event != "pr_ready_to_merge" {
		t.Errorf("event = %q, want %q", got.Event, "pr_ready_to_merge")
	}
	if got.Anvil != "my-anvil" {
		t.Errorf("anvil = %q, want %q", got.Anvil, "my-anvil")
	}
	if got.BeadID != "Forge-99" {
		t.Errorf("bead_id = %q, want %q", got.BeadID, "Forge-99")
	}
	if got.PRNumber != 42 {
		t.Errorf("pr_number = %d, want %d", got.PRNumber, 42)
	}
	if got.PRURL != payload.PRURL {
		t.Errorf("pr_url = %q, want %q", got.PRURL, payload.PRURL)
	}
	if got.PRTitle != payload.PRTitle {
		t.Errorf("pr_title = %q, want %q", got.PRTitle, payload.PRTitle)
	}
}

// TestSendGenericPRReadyToMerge_EmptyURLIsNoop verifies that an empty URL does nothing.
func TestSendGenericPRReadyToMerge_EmptyURLIsNoop(t *testing.T) {
	// No panic / no crash — just a silent no-op.
	notify.SendGenericPRReadyToMerge(context.Background(), "", notify.PRReadyToMergePayload{
		Event:    "pr_ready_to_merge",
		PRNumber: 1,
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
