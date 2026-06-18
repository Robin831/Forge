package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

// withConfigFixture points the server's config read/write hooks at a temp
// file seeded with the given YAML, returning the file path.
func withConfigFixture(t *testing.T, srv *Server, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	srv.configLoader = func() (*config.Config, error) { return config.Load(path) }
	srv.configPath = func() string { return path }
	return path
}

func TestForgeConfig_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	withConfigFixture(t, srv, "")

	// GET without a session.
	rec := forgeRequest(t, srv, http.MethodGet, "/api/forge/config", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET: expected 401, got %d", rec.Code)
	}
	// PATCH without a session.
	rec = forgeRequest(t, srv, http.MethodPatch, "/api/forge/config", `{"schematic_enabled":true}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("PATCH: expected 401, got %d", rec.Code)
	}
}

func TestForgeConfig_GetReturnsAllManagedKeys(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	// Empty config file: every key falls back to its documented default.
	withConfigFixture(t, srv, "")

	var resp ConfigResponse
	rec := forgeRequest(t, srv, http.MethodGet, "/api/forge/config", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Keys) != len(managedConfigKeys) {
		t.Fatalf("expected %d keys, got %d", len(managedConfigKeys), len(resp.Keys))
	}

	byKey := map[string]ConfigKeyInfo{}
	for _, k := range resp.Keys {
		byKey[k.Key] = k
		if k.Area == "" || k.Label == "" || k.Description == "" {
			t.Errorf("key %s missing metadata: %+v", k.Key, k)
		}
	}

	// All defaults => IsDefault must be true for every key.
	for _, k := range resp.Keys {
		if !k.IsDefault {
			t.Errorf("key %s: expected IsDefault=true on empty config, got value=%v", k.Key, k.Value)
		}
	}

	// Tri-state *bool keys default to true.
	for _, key := range []string{"auto_merge_crucible_children", "vulncheck_enabled", "smelter_enabled"} {
		if !byKey[key].Value {
			t.Errorf("tri-state key %s: expected default true, got false", key)
		}
	}
	// Plain bool keys default to false.
	if byKey["schematic_enabled"].Value {
		t.Errorf("schematic_enabled: expected default false")
	}

	// Only the two hot-reloadable keys are flagged.
	hot := map[string]bool{}
	for _, k := range resp.Keys {
		if k.HotReloadable {
			hot[k.Key] = true
		}
	}
	if len(hot) != 2 || !hot["copilot_combined_smith_warden"] || !hot["smelter_enabled"] {
		t.Errorf("expected exactly copilot_combined_smith_warden + smelter_enabled hotReloadable, got %v", hot)
	}
}

func TestForgeConfig_GetReflectsFileValues(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	withConfigFixture(t, srv, `settings:
  schematic_enabled: true
  smelter_enabled: false
`)

	var resp ConfigResponse
	rec := forgeRequest(t, srv, http.MethodGet, "/api/forge/config", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byKey := map[string]ConfigKeyInfo{}
	for _, k := range resp.Keys {
		byKey[k.Key] = k
	}

	// schematic_enabled flipped to true => not default.
	if !byKey["schematic_enabled"].Value || byKey["schematic_enabled"].IsDefault {
		t.Errorf("schematic_enabled: expected value=true, isDefault=false, got %+v", byKey["schematic_enabled"])
	}
	// smelter_enabled explicitly false => not default (default is true).
	if byKey["smelter_enabled"].Value || byKey["smelter_enabled"].IsDefault {
		t.Errorf("smelter_enabled: expected value=false, isDefault=false, got %+v", byKey["smelter_enabled"])
	}
}

func TestForgeConfig_PatchPersistsAndPreservesCommentsAndKeys(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	path := withConfigFixture(t, srv, `# top-level comment
settings:
  # keep me — comment on poll_interval
  poll_interval: 7m
  schematic_enabled: false
anvils:
  forge:
    path: /tmp/forge
`)

	body := `{"schematic_enabled":true,"vulncheck_enabled":false}`
	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Response is the re-read config reflecting the change.
	var resp ConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	byKey := map[string]ConfigKeyInfo{}
	for _, k := range resp.Keys {
		byKey[k.Key] = k
	}
	if !byKey["schematic_enabled"].Value {
		t.Errorf("response: schematic_enabled not true")
	}
	if byKey["vulncheck_enabled"].Value {
		t.Errorf("response: vulncheck_enabled not false")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(raw)

	// Comments and unrelated keys must survive the node-tree edit.
	for _, want := range []string{
		"# top-level comment",
		"keep me — comment on poll_interval",
		"poll_interval: 7m",
		"path: /tmp/forge",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected preserved %q in file, got:\n%s", want, got)
		}
	}
	// The edited keys are written.
	if !strings.Contains(got, "schematic_enabled: true") {
		t.Errorf("expected schematic_enabled: true, got:\n%s", got)
	}
	if !strings.Contains(got, "vulncheck_enabled: false") {
		t.Errorf("expected vulncheck_enabled: false, got:\n%s", got)
	}
}

func TestForgeConfig_PatchUnknownKeyRejectedAndNoWrite(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	original := `settings:
  schematic_enabled: false
`
	path := withConfigFixture(t, srv, original)

	body := `{"schematic_enabled":true,"not_a_real_key":true}`
	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config", body, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not_a_real_key") {
		t.Errorf("error should name the offending key, got %s", rec.Body.String())
	}

	// All-or-nothing: nothing was written, including the valid key.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != original {
		t.Errorf("file should be unchanged, got:\n%s", string(raw))
	}
}

func TestForgeConfig_PatchEmptyBodyRejected(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	withConfigFixture(t, srv, "")

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config", `{}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty patch, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestForgeConfig_PatchRequiresCSRFHeader(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	withConfigFixture(t, srv, "")

	// Authenticated PATCH without the X-Forge-Action header must be rejected.
	req := httptest.NewRequest(http.MethodPatch, "/api/forge/config",
		strings.NewReader(`{"schematic_enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF header, got %d", rec.Code)
	}
}

// TestForgeConfig_TriStateNilResolvesToDefault verifies the value accessors
// resolve a nil *bool to its documented default directly (independent of the
// viper loader, which may materialise defaults of its own).
func TestForgeConfig_TriStateNilResolvesToDefault(t *testing.T) {
	var empty config.SettingsConfig // all *bool fields nil
	for _, key := range []string{"auto_merge_crucible_children", "vulncheck_enabled", "smelter_enabled"} {
		def := configKeyByName[key]
		if !def.value(empty) {
			t.Errorf("key %s: nil *bool should resolve to true, got false", key)
		}
		if !def.Default {
			t.Errorf("key %s: expected documented default true", key)
		}
	}
}

func TestForgeConfig_PatchCreatesFileWhenMissing(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	// Point at a path that does not yet exist (and whose dir is missing).
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	srv.configLoader = func() (*config.Config, error) { return config.Load(path) }
	srv.configPath = func() string { return path }

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config", `{"crucible_enabled":true}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected created file: %v", err)
	}
	if !strings.Contains(string(raw), "crucible_enabled: true") {
		t.Errorf("expected crucible_enabled: true in new file, got:\n%s", string(raw))
	}
}

// TestForgeConfig_GetIncludesPerAnvilSettings verifies the anvils.<name>.<key>
// contract: tri-state *bool settings round-trip as true/false/null and plain
// bool settings (auto_merge, wicket_auto_dispatch) are always present.
func TestForgeConfig_GetIncludesPerAnvilSettings(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	// "set" anvil overrides every tri-state field (mix of true/false) and
	// enables the plain bools; "unset" anvil leaves all tri-state fields nil.
	withConfigFixture(t, srv, `anvils:
  set:
    path: /tmp/set
    auto_merge: true
    wicket_auto_dispatch: true
    schematic_enabled: true
    golangci_lint: false
    go_race_detection: true
    depcheck_enabled: false
    questgiver_enabled: true
    wicket_enabled: false
  unset:
    path: /tmp/unset
`)

	rec := forgeRequest(t, srv, http.MethodGet, "/api/forge/config", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Decode into raw JSON to assert the literal null vs true/false distinction
	// for tri-state fields, which a *bool struct decode would also handle but
	// raw map makes the contract explicit.
	var raw struct {
		Anvils map[string]map[string]json.RawMessage `json:"anvils"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Anvils) != 2 {
		t.Fatalf("expected 2 anvils, got %d: %v", len(raw.Anvils), raw.Anvils)
	}

	set := raw.Anvils["set"]
	if set == nil {
		t.Fatalf("missing 'set' anvil")
	}
	expectSet := map[string]string{
		"auto_merge":           "true",
		"wicket_auto_dispatch": "true",
		"schematic_enabled":    "true",
		"golangci_lint":        "false",
		"go_race_detection":    "true",
		"depcheck_enabled":     "false",
		"questgiver_enabled":   "true",
		"wicket_enabled":       "false",
	}
	for k, want := range expectSet {
		if got := strings.TrimSpace(string(set[k])); got != want {
			t.Errorf("set.%s: expected %s, got %q", k, want, got)
		}
	}

	unset := raw.Anvils["unset"]
	if unset == nil {
		t.Fatalf("missing 'unset' anvil")
	}
	// Tri-state fields must be JSON null (inherit/unset).
	for _, k := range []string{"schematic_enabled", "golangci_lint", "go_race_detection", "depcheck_enabled", "questgiver_enabled", "wicket_enabled"} {
		if got := strings.TrimSpace(string(unset[k])); got != "null" {
			t.Errorf("unset.%s: expected null, got %q", k, got)
		}
	}
	// Plain bools are always present and default to false.
	for _, k := range []string{"auto_merge", "wicket_auto_dispatch"} {
		if got := strings.TrimSpace(string(unset[k])); got != "false" {
			t.Errorf("unset.%s: expected false, got %q", k, got)
		}
	}
}

// anvilFixture seeds a config with a single "forge" anvil and points the
// server's read/write hooks at it, returning the file path.
func anvilFixture(t *testing.T, srv *Server, extra string) string {
	t.Helper()
	return withConfigFixture(t, srv, `anvils:
  forge:
    path: /tmp/forge
`+extra)
}

// decodeAnvilPatch decodes an AnvilConfigPatchResponse from a recorder.
func decodeAnvilPatch(t *testing.T, body []byte) AnvilConfigPatchResponse {
	t.Helper()
	var resp AnvilConfigPatchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode anvil patch response: %v (body=%s)", err, string(body))
	}
	return resp
}

func TestForgeAnvilConfig_PatchRequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	anvilFixture(t, srv, "")

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge", `{"auto_merge":true}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// TestForgeAnvilConfig_PatchAutoMergeIsInstant verifies the hot-reloadable
// key reports applies=instant and persists.
func TestForgeAnvilConfig_PatchAutoMergeIsInstant(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	path := anvilFixture(t, srv, "")

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge", `{"auto_merge":true}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeAnvilPatch(t, rec.Body.Bytes())
	if resp.Anvil != "forge" {
		t.Errorf("expected anvil=forge, got %q", resp.Anvil)
	}
	if !resp.Settings.AutoMerge {
		t.Errorf("expected settings.auto_merge=true, got %+v", resp.Settings)
	}
	if len(resp.Applied) != 1 || resp.Applied[0].Key != "auto_merge" || resp.Applied[0].Applies != appliesInstant {
		t.Errorf("expected auto_merge=instant, got %+v", resp.Applied)
	}
	if resp.Applied[0].Cleared {
		t.Errorf("explicit true should not be marked cleared")
	}

	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "auto_merge: true") {
		t.Errorf("expected auto_merge: true persisted, got:\n%s", string(raw))
	}
}

// TestForgeAnvilConfig_PatchNextRunKey verifies a non-hot-reloadable key
// reports applies=next_run.
func TestForgeAnvilConfig_PatchNextRunKey(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	anvilFixture(t, srv, "")

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge", `{"schematic_enabled":true}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeAnvilPatch(t, rec.Body.Bytes())
	if len(resp.Applied) != 1 || resp.Applied[0].Applies != appliesNextRun {
		t.Errorf("expected schematic_enabled=next_run, got %+v", resp.Applied)
	}
	if resp.Settings.SchematicEnabled == nil || !*resp.Settings.SchematicEnabled {
		t.Errorf("expected schematic_enabled=true in settings, got %+v", resp.Settings.SchematicEnabled)
	}
}

// TestForgeAnvilConfig_PatchExplicitTrueVsFalse verifies explicit true and
// false both persist as concrete pointer values (not cleared/inherited).
func TestForgeAnvilConfig_PatchExplicitTrueVsFalse(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	path := anvilFixture(t, srv, "")

	// Explicit false.
	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge", `{"golangci_lint":false}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit false: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeAnvilPatch(t, rec.Body.Bytes())
	if resp.Settings.GolangciLint == nil || *resp.Settings.GolangciLint {
		t.Errorf("expected golangci_lint=false (non-nil), got %+v", resp.Settings.GolangciLint)
	}
	if resp.Applied[0].Cleared {
		t.Errorf("explicit false should not be marked cleared")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "golangci_lint: false") {
		t.Errorf("expected golangci_lint: false persisted, got:\n%s", string(raw))
	}

	// Explicit true.
	rec = forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge", `{"golangci_lint":true}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit true: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resp = decodeAnvilPatch(t, rec.Body.Bytes())
	if resp.Settings.GolangciLint == nil || !*resp.Settings.GolangciLint {
		t.Errorf("expected golangci_lint=true (non-nil), got %+v", resp.Settings.GolangciLint)
	}
}

// TestForgeAnvilConfig_PatchClearInherits verifies that null clears a
// tri-state key — the key is removed from the file (inherit) and the re-read
// settings report nil, distinct from an explicit false.
func TestForgeAnvilConfig_PatchClearInherits(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	path := anvilFixture(t, srv, "    schematic_enabled: true\n")

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge", `{"schematic_enabled":null}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeAnvilPatch(t, rec.Body.Bytes())
	if resp.Settings.SchematicEnabled != nil {
		t.Errorf("expected schematic_enabled cleared to nil (inherit), got %v", *resp.Settings.SchematicEnabled)
	}
	if len(resp.Applied) != 1 || !resp.Applied[0].Cleared {
		t.Errorf("expected schematic_enabled marked cleared, got %+v", resp.Applied)
	}

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "schematic_enabled") {
		t.Errorf("expected schematic_enabled removed from file, got:\n%s", string(raw))
	}
	// The anvil itself and unrelated keys must survive.
	if !strings.Contains(string(raw), "path: /tmp/forge") {
		t.Errorf("expected anvil path preserved, got:\n%s", string(raw))
	}
}

// TestForgeAnvilConfig_PatchClearRejectedForPlainBool verifies null is
// rejected for plain bool keys (auto_merge, wicket_auto_dispatch) which have
// no inherit semantics.
func TestForgeAnvilConfig_PatchClearRejectedForPlainBool(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	path := anvilFixture(t, srv, "    auto_merge: true\n")

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge", `{"auto_merge":null}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "auto_merge") {
		t.Errorf("error should name the key, got %s", rec.Body.String())
	}
	// Nothing written: the original value survives.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "auto_merge: true") {
		t.Errorf("file should be unchanged, got:\n%s", string(raw))
	}
}

// TestForgeAnvilConfig_PatchUnknownAnvil verifies an unknown anvil name is
// rejected with 404 and nothing is written.
func TestForgeAnvilConfig_PatchUnknownAnvil(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	path := anvilFixture(t, srv, "")
	original, _ := os.ReadFile(path)

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/nope", `{"auto_merge":true}`, cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != string(original) {
		t.Errorf("file should be unchanged, got:\n%s", string(raw))
	}
}

// TestForgeAnvilConfig_PatchUnknownKey verifies an unknown key is rejected
// 400 (all-or-nothing) and the valid key alongside it is not written.
func TestForgeAnvilConfig_PatchUnknownKey(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	path := anvilFixture(t, srv, "")
	original, _ := os.ReadFile(path)

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge",
		`{"auto_merge":true,"bogus_key":true}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bogus_key") {
		t.Errorf("error should name the offending key, got %s", rec.Body.String())
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != string(original) {
		t.Errorf("all-or-nothing: file should be unchanged, got:\n%s", string(raw))
	}
}

// TestForgeAnvilConfig_PatchInvalidValue verifies a non-bool/non-null value
// is rejected with 400.
func TestForgeAnvilConfig_PatchInvalidValue(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	anvilFixture(t, srv, "")

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge", `{"auto_merge":"yes"}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestForgeAnvilConfig_PatchEmptyBody verifies an empty patch is rejected.
func TestForgeAnvilConfig_PatchEmptyBody(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	anvilFixture(t, srv, "")

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge", `{}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty patch, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestForgeAnvilConfig_PatchPreservesCommentsAndOtherAnvils verifies the
// node-tree edit preserves comments, unrelated keys, and sibling anvils, and
// that the result is re-readable (atomic write).
func TestForgeAnvilConfig_PatchPreservesCommentsAndOtherAnvils(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	path := withConfigFixture(t, srv, `# top comment
settings:
  poll_interval: 7m
anvils:
  forge:
    # keep me
    path: /tmp/forge
    schematic_enabled: true
  other:
    path: /tmp/other
    auto_merge: true
`)

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge",
		`{"auto_merge":true,"schematic_enabled":null}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		"# top comment",
		"poll_interval: 7m",
		"# keep me",
		"path: /tmp/forge",
		"path: /tmp/other",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected preserved %q, got:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "auto_merge: true") {
		t.Errorf("expected auto_merge: true on forge, got:\n%s", got)
	}

	// The sibling anvil's auto_merge must be untouched, and the cleared key
	// gone from the forge anvil.
	reread, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reread.Anvils["forge"].AutoMerge {
		t.Errorf("forge.auto_merge should be true after edit")
	}
	if reread.Anvils["forge"].SchematicEnabled != nil {
		t.Errorf("forge.schematic_enabled should be cleared (nil)")
	}
	if !reread.Anvils["other"].AutoMerge {
		t.Errorf("other.auto_merge should remain true")
	}
}

// TestForgeAnvilConfig_PatchMultipleKeysOrderedCoverage verifies multiple
// keys in one request all persist and the per-key coverage is reported in
// allowlist order with the correct instant/next_run split.
func TestForgeAnvilConfig_PatchMultipleKeysOrderedCoverage(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	anvilFixture(t, srv, "")

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge",
		`{"wicket_enabled":true,"auto_merge":true}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeAnvilPatch(t, rec.Body.Bytes())
	if len(resp.Applied) != 2 {
		t.Fatalf("expected 2 applied entries, got %d: %+v", len(resp.Applied), resp.Applied)
	}
	// auto_merge precedes wicket_enabled in the allowlist order.
	if resp.Applied[0].Key != "auto_merge" || resp.Applied[0].Applies != appliesInstant {
		t.Errorf("expected first=auto_merge/instant, got %+v", resp.Applied[0])
	}
	if resp.Applied[1].Key != "wicket_enabled" || resp.Applied[1].Applies != appliesNextRun {
		t.Errorf("expected second=wicket_enabled/next_run, got %+v", resp.Applied[1])
	}
}

// TestForgeAnvilConfig_PatchRequiresCSRFHeader mirrors the global config
// CSRF guard for the per-anvil route.
func TestForgeAnvilConfig_PatchRequiresCSRFHeader(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	anvilFixture(t, srv, "")

	req := httptest.NewRequest(http.MethodPatch, "/api/forge/config/anvils/forge",
		strings.NewReader(`{"auto_merge":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF header, got %d", rec.Code)
	}
}

// TestForgeAnvilConfig_PatchCreatesAnvilKeysWhenAbsent verifies that setting a
// key on an anvil whose YAML block has only a path adds the key without
// disturbing the path, and the file round-trips.
func TestForgeAnvilConfig_PatchCreatesAnvilKeysWhenAbsent(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	path := anvilFixture(t, srv, "")

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge",
		`{"depcheck_enabled":false}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	reread, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	dc := reread.Anvils["forge"].DepcheckEnabled
	if dc == nil || *dc {
		t.Errorf("expected depcheck_enabled=false (non-nil), got %v", dc)
	}
}

// TestForgeAnvilConfig_PatchCreatesAnvilBlockWithPath verifies that when the
// anvil exists in the loaded config but not in the YAML file, patching it
// creates a new anvil block that includes the path field so the file stays valid.
func TestForgeAnvilConfig_PatchCreatesAnvilBlockWithPath(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(""), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	srv.configLoader = func() (*config.Config, error) {
		return &config.Config{
			Anvils: map[string]config.AnvilConfig{
				"forge": {Path: "/srv/forge"},
			},
		}, nil
	}
	srv.configPath = func() string { return cfgPath }

	rec := forgeRequest(t, srv, http.MethodPatch, "/api/forge/config/anvils/forge",
		`{"auto_merge":true}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "path: /srv/forge") {
		t.Errorf("expected written config to contain anvil path, got:\n%s", content)
	}
	if !strings.Contains(content, "auto_merge: true") {
		t.Errorf("expected written config to contain auto_merge, got:\n%s", content)
	}
}

// TestForgeConfig_GetEmptyAnvilsIsObjectNotNull verifies the no-anvils case
// serializes anvils as {} rather than null.
func TestForgeConfig_GetEmptyAnvilsIsObjectNotNull(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	withConfigFixture(t, srv, "")

	rec := forgeRequest(t, srv, http.MethodGet, "/api/forge/config", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Anvils map[string]json.RawMessage `json:"anvils"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Anvils == nil {
		t.Fatal("expected anvils to be a non-nil empty object, got null")
	}
	if len(resp.Anvils) != 0 {
		t.Errorf("expected empty anvils map, got %d entries", len(resp.Anvils))
	}
}
