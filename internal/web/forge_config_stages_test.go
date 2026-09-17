package web

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

// assayStageKeys are the stage_providers keys Assay reads: the whole-review
// chain and one per pass, in the order providerStages lists them.
var assayStageKeys = []string{
	"assay",
	"assay.triage", "assay.logic", "assay.security", "assay.conventions", "assay.tests-missing", "assay.repo-specific",
}

// TestProviderStagesAssayKeysFollowReviewfix pins the position and order of the
// Assay keys: directly after reviewfix, in the documented order.
func TestProviderStagesAssayKeysFollowReviewfix(t *testing.T) {
	at := -1
	for i, s := range providerStages {
		if s == "reviewfix" {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("providerStages has no reviewfix: %v", providerStages)
	}
	got := providerStages[at+1:]
	if strings.Join(got, ",") != strings.Join(assayStageKeys, ",") {
		t.Errorf("stages after reviewfix = %v, want %v", got, assayStageKeys)
	}
}

// TestForgeConfig_StageProvidersAssayKeysValidation runs the same accept/reject
// table through the global PATCH and the per-anvil PATCH, since both persist a
// stage_providers map and neither may accept a key the other refuses.
func TestForgeConfig_StageProvidersAssayKeysValidation(t *testing.T) {
	routes := []struct {
		name    string
		path    string
		fixture func(t *testing.T, srv *Server) string
	}{
		{"global", "/api/forge/config", func(t *testing.T, srv *Server) string {
			return withConfigFixture(t, srv, "settings:\n  schematic_enabled: false\n")
		}},
		{"anvil", "/api/forge/config/anvils/forge", func(t *testing.T, srv *Server) string {
			return anvilFixture(t, srv, "")
		}},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			srv := newServerWithDefaults(t, nil)
			cookie := loginAndGetCookie(t, srv)
			path := rt.fixture(t, srv)

			for _, key := range assayStageKeys {
				rec := forgeRequest(t, srv, http.MethodPatch, rt.path,
					`{"stage_providers":{"`+key+`":["claude/x"]}}`, cookie)
				if rec.Code != http.StatusOK {
					t.Errorf("%s: expected 200, got %d body=%s", key, rec.Code, rec.Body.String())
					continue
				}
				raw, _ := os.ReadFile(path)
				if !strings.Contains(string(raw), key+":") {
					t.Errorf("%s: expected key persisted, got:\n%s", key, raw)
				}
				cfg, err := config.Load(path)
				if err != nil {
					t.Fatalf("%s: persisted config does not load: %v", key, err)
				}
				m := cfg.Settings.StageProviders
				if rt.name == "anvil" {
					m = cfg.Anvils["forge"].StageProviders
				}
				if got := m[key]; len(got) != 1 || got[0] != "claude/x" {
					t.Errorf("%s: loaded stage_providers = %v, want %s:[claude/x]", key, m, key)
				}
			}

			rejects := []struct {
				name, body, want string
			}{
				{"unknown pass", `{"stage_providers":{"assay.logics":["claude"]}}`,
					`key "stage_providers": unknown stage "assay.logics" (allowed: ` + strings.Join(providerStages, ", ") + `)`},
				{"empty chain", `{"stage_providers":{"assay.security":[]}}`,
					`key "stage_providers": stage "assay.security" must have at least one provider`},
			}
			for _, rj := range rejects {
				rec := forgeRequest(t, srv, http.MethodPatch, rt.path, rj.body, cookie)
				if rec.Code != http.StatusBadRequest {
					t.Errorf("%s: expected 400, got %d body=%s", rj.name, rec.Code, rec.Body.String())
					continue
				}
				var body struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("%s: decode error body: %v (body=%s)", rj.name, err, rec.Body.String())
				}
				if body.Error != rj.want {
					t.Errorf("%s: error = %q, want %q", rj.name, body.Error, rj.want)
				}
			}
		})
	}
}

// frontendProviderMapFieldFile is the frontend mirror of providerStages.
const frontendProviderMapFieldFile = "frontend/src/components/ProviderMapField.tsx"

// TestFrontendProviderStagesMatchBackend reads PROVIDER_STAGES out of the .tsx
// source and compares it, in order, against the schema Options the settings
// page is served. A TypeScript-side assertion cannot catch the two lists
// drifting, since it holds for whatever both halves of it say.
func TestFrontendProviderStagesMatchBackend(t *testing.T) {
	raw, err := os.ReadFile(frontendProviderMapFieldFile)
	if err != nil {
		t.Fatalf("read %s: %v", frontendProviderMapFieldFile, err)
	}
	decl := regexp.MustCompile(`(?s)export const PROVIDER_STAGES(?:\s*:[^=]*)?\s*=\s*\[(.*?)\]`)
	m := decl.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s: no `export const PROVIDER_STAGES = [...]` declaration found", frontendProviderMapFieldFile)
	}
	// Drop line comments so a quoted word in prose is not read as a member.
	body := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(m[1]), "")
	var frontend []string
	for _, lit := range regexp.MustCompile(`'([^']*)'|"([^"]*)"`).FindAllStringSubmatch(body, -1) {
		frontend = append(frontend, lit[1]+lit[2])
	}

	var backend []string
	for _, def := range managedConfigKeys {
		if def.Key == "stage_providers" {
			backend = def.Options
		}
	}
	if backend == nil {
		t.Fatal("no stage_providers key in the settings schema")
	}

	if strings.Join(frontend, ",") == strings.Join(backend, ",") {
		return
	}
	inFront := map[string]bool{}
	for _, s := range frontend {
		inFront[s] = true
	}
	inBack := map[string]bool{}
	for _, s := range backend {
		inBack[s] = true
	}
	var missing, extra []string
	for _, s := range backend {
		if !inFront[s] {
			missing = append(missing, s)
		}
	}
	for _, s := range frontend {
		if !inBack[s] {
			extra = append(extra, s)
		}
	}
	t.Errorf("PROVIDER_STAGES in %s differs from the backend stage_providers Options\n"+
		"  frontend: %v\n  backend:  %v\n  missing from frontend: %v\n  extra in frontend: %v",
		frontendProviderMapFieldFile, frontend, backend, missing, extra)
}
