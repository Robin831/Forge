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
