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
