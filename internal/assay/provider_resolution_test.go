package assay

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// legacyProviderForBeforeIyddo is the resolver this package shipped before
// per-pass resolution, copied verbatim (keyed by tier) so the no-assay-keys
// table below compares against what a review actually ran on, not against a
// restatement of the new code.
func legacyProviderForBeforeIyddo(c Config, tier string) provider.Provider {
	spec := c.ReviewProvider
	model := c.ReviewModel
	if tier == tierTriage {
		if c.TriageProvider != "" {
			spec = c.TriageProvider
		}
		if c.TriageModel != "" {
			model = c.TriageModel
		}
	}
	pv := provider.Provider{Kind: provider.Claude}
	if spec != "" {
		if list := provider.FromConfig([]string{spec}); len(list) > 0 {
			pv = list[0]
		}
	}
	if model != "" {
		pv.Model = model
	}
	return pv
}

// With no assay stage key anywhere, every pass must spawn exactly the provider
// it spawned before — including with smith_providers and settings.providers
// set, which the old resolver never read either.
func TestProvidersForWithoutAssayStageKeysMatchesLegacy(t *testing.T) {
	munin := func() config.AssayConfig {
		enabled := true
		return config.AssayConfig{
			Enabled:     &enabled,
			ModelTier:   "opus_all",
			TriageModel: "claude-opus-5",
			ReviewModel: "claude-opus-5",
		}
	}
	cases := []struct {
		name        string
		global      config.AssayConfig
		overlay     *config.AssayConfig
		wantTriage  string
		wantDeep    string
		wantSources ProviderSource
	}{
		{
			name:        "munin shape",
			global:      munin(),
			wantTriage:  "claude/claude-opus-5",
			wantDeep:    "claude/claude-opus-5",
			wantSources: SourceLegacyAssayBlock,
		},
		{
			name:        "munin shape with a triage-only anvil override",
			global:      munin(),
			overlay:     &config.AssayConfig{TriageModel: "claude-sonnet-5"},
			wantTriage:  "claude/claude-sonnet-5",
			wantDeep:    "claude/claude-opus-5",
			wantSources: SourceLegacyAssayBlock,
		},
		{
			name: "triage provider only",
			global: config.AssayConfig{
				TriageProvider: "gemini/gemini-2.5-flash",
				ReviewModel:    "claude-opus-5",
			},
			// The triage model hint still falls back to review_model, as it
			// always did.
			wantTriage:  "gemini/claude-opus-5",
			wantDeep:    "claude/claude-opus-5",
			wantSources: SourceLegacyAssayBlock,
		},
		{
			name:        "nothing configured",
			global:      config.AssayConfig{},
			wantTriage:  "claude",
			wantDeep:    "claude",
			wantSources: SourceProviderDefaults,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Assay: tc.global,
				Settings: config.SettingsConfig{
					Providers:      []string{"copilot"},
					SmithProviders: []string{"openai/o3"},
					StageProviders: map[string][]string{"smith": {"gemini"}, "warden": {"copilot"}},
				},
				Anvils: map[string]config.AnvilConfig{
					"munin": {
						Path:           "/repos/munin",
						Assay:          tc.overlay,
						StageProviders: map[string][]string{"smith": {"openai"}},
					},
				},
			}
			ec := ForAnvil(cfg, "munin")
			for _, rc := range ec.ResolvedChains() {
				tier := tierReview
				want := tc.wantDeep
				if rc.Pass == passTriage.Name {
					tier, want = tierTriage, tc.wantTriage
				}
				got := ec.providerFor(rc.Pass)
				if !reflect.DeepEqual(got, legacyProviderForBeforeIyddo(ec, tier)) {
					t.Errorf("%s: got %+v, legacy resolver gave %+v", rc.Pass, got, legacyProviderForBeforeIyddo(ec, tier))
				}
				if got.Label() != want {
					t.Errorf("%s: head = %s, want %s", rc.Pass, got.Label(), want)
				}
				if rc.Source != tc.wantSources {
					t.Errorf("%s: source = %s, want step %d", rc.Pass, rc.SourceLabel(), tc.wantSources)
				}
			}
			if ec.ModelTier != tc.global.ModelTier && tc.global.ModelTier != "" {
				t.Errorf("model tier = %q, want %q", ec.ModelTier, tc.global.ModelTier)
			}
		})
	}
}

// Each case removes the layer that decided the previous one, walking all six
// steps for the logic pass.
func TestProvidersForPrecedence(t *testing.T) {
	type layers struct {
		anvilPass, anvilAssay, globalPass, globalAssay, legacy bool
	}
	build := func(l layers) *config.Config {
		cfg := &config.Config{
			Settings: config.SettingsConfig{
				Providers:      []string{"copilot"},
				SmithProviders: []string{"copilot/gpt-5"},
				StageProviders: map[string][]string{"smith": {"copilot"}},
			},
			Anvils: map[string]config.AnvilConfig{
				"api": {Path: "/repos/api", StageProviders: map[string][]string{"smith": {"copilot"}}},
			},
		}
		a := cfg.Anvils["api"]
		if l.anvilPass {
			a.StageProviders["assay.logic"] = []string{"gemini/anvil-pass", "claude"}
		}
		if l.anvilAssay {
			a.StageProviders["assay"] = []string{"gemini/anvil-assay"}
		}
		cfg.Anvils["api"] = a
		if l.globalPass {
			cfg.Settings.StageProviders["assay.logic"] = []string{"openai/global-pass"}
		}
		if l.globalAssay {
			cfg.Settings.StageProviders["assay"] = []string{"openai/global-assay"}
		}
		if l.legacy {
			cfg.Assay.ReviewProvider = "claude"
			cfg.Assay.ReviewModel = "legacy-review"
		}
		return cfg
	}
	cases := []struct {
		name      string
		layers    layers
		wantChain string
		wantSrc   ProviderSource
		wantLabel string
	}{
		{"1 anvil assay.logic", layers{true, true, true, true, true}, "gemini/anvil-pass -> claude", SourceAnvilPassStage, "anvil stage_providers[assay.logic]"},
		{"2 anvil assay", layers{false, true, true, true, true}, "gemini/anvil-assay", SourceAnvilAssayStage, "anvil stage_providers[assay]"},
		{"3 global assay.logic", layers{false, false, true, true, true}, "openai/global-pass", SourceGlobalPassStage, "settings.stage_providers[assay.logic]"},
		{"4 global assay", layers{false, false, false, true, true}, "openai/global-assay", SourceGlobalAssayStage, "settings.stage_providers[assay]"},
		{"5 legacy block", layers{false, false, false, false, true}, "claude/legacy-review", SourceLegacyAssayBlock, "assay.review_provider+assay.review_model"},
		{"6 provider defaults", layers{}, "claude", SourceProviderDefaults, "provider defaults"},
		// Anvil scope outranks pass specificity.
		{"anvil assay beats global assay.logic", layers{false, true, true, false, false}, "gemini/anvil-assay", SourceAnvilAssayStage, "anvil stage_providers[assay]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := ForAnvil(build(tc.layers), "api").resolveChain("logic")
			if got := rc.ChainLabel(); got != tc.wantChain {
				t.Errorf("chain = %s, want %s", got, tc.wantChain)
			}
			if rc.Source != tc.wantSrc || rc.SourceLabel() != tc.wantLabel {
				t.Errorf("source = %d (%s), want %d (%s)", rc.Source, rc.SourceLabel(), tc.wantSrc, tc.wantLabel)
			}
			for _, pv := range rc.Providers {
				if pv.Kind == provider.Copilot {
					t.Errorf("chain inherited smith_providers/providers: %s", rc.ChainLabel())
				}
			}
		})
	}

	// A per-pass key decides only its own pass.
	ec := ForAnvil(build(layers{anvilPass: true, legacy: true}), "api")
	if got := ec.providerFor("security").Label(); got != "claude/legacy-review" {
		t.Errorf("security = %s, want the legacy provider", got)
	}
	if got := ec.providerFor(passTriage.Name).Label(); got != "claude/legacy-review" {
		t.Errorf("triage = %s, want the legacy provider", got)
	}

	// An empty stage chain is unset, not an empty answer.
	cfg := build(layers{globalAssay: true})
	a := cfg.Anvils["api"]
	a.StageProviders["assay.logic"] = []string{}
	cfg.Anvils["api"] = a
	if rc := ForAnvil(cfg, "api").resolveChain("logic"); rc.Source != SourceGlobalAssayStage {
		t.Errorf("empty anvil chain: source = %s, want settings.stage_providers[assay]", rc.SourceLabel())
	}
}

func TestResolvedChainFallbacks(t *testing.T) {
	cfg := &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"api": {Path: "/a", StageProviders: map[string][]string{"assay.security": {"gemini/g", "claude", "copilot"}}},
		},
	}
	rc := ForAnvil(cfg, "api").resolveChain("security")
	if got := rc.Fallbacks(); len(got) != 2 || got[0].Kind != provider.Claude || got[1].Kind != provider.Copilot {
		t.Errorf("Fallbacks = %v, want [claude copilot]", got)
	}
	if got := ForAnvil(cfg, "api").resolveChain("logic").Fallbacks(); got != nil {
		t.Errorf("default chain Fallbacks = %v, want none", got)
	}
}

func TestLegacyStageConflicts(t *testing.T) {
	enabled := true
	cfg := &config.Config{
		Assay: config.AssayConfig{Enabled: &enabled, ReviewProvider: "claude"},
		Settings: config.SettingsConfig{
			StageProviders: map[string][]string{"smith": {"claude"}},
		},
		Anvils: map[string]config.AnvilConfig{
			"api":   {Path: "/a", StageProviders: map[string][]string{"assay.logic": {"gemini"}}},
			"plain": {Path: "/b"},
			"off": {
				Path:           "/c",
				Assay:          &config.AssayConfig{Enabled: new(bool)},
				StageProviders: map[string][]string{"assay": {"gemini"}},
			},
		},
	}
	got := LegacyStageConflicts(cfg)
	if len(got) != 1 || got[0].Anvil != "api" {
		t.Fatalf("conflicts = %+v, want exactly api", got)
	}
	msg := got[0].String()
	for _, want := range []string{"anvil api", "assay.review_provider", "anvil:assay.logic", "stage_providers wins for logic", "legacy keys still decide triage, security"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

// mixedPassConfig puts logic on copilot and triage on gemini beside a claude
// default for everything else.
func mixedPassConfig() Config {
	c := DefaultConfig()
	c.ReviewProvider = "claude"
	c.AnvilStageProviders = map[string][]string{
		"assay.logic":  {"copilot"},
		"assay.triage": {"gemini"},
	}
	return c
}

func wantKindForPass(pass string) provider.Kind {
	switch pass {
	case "logic":
		return provider.Copilot
	case passTriage.Name:
		return provider.Gemini
	default:
		return provider.Claude
	}
}

// Review must attribute each PassReport to the provider its own pass resolved,
// not to one provider per tier: RenderPassTelemetry groups on this field, and a
// lookup by tier would put the copilot logic pass under claude and license
// tools=0 on it.
func TestReviewPassReportsCarryPerPassProvider(t *testing.T) {
	db := openTestDB(t)
	runner := newScriptRunner(baseScript(triageJSON(t, nil, ""), nil))
	cfg := mixedPassConfig().WithRunner(runner.run)

	res, err := Review(context.Background(), testRequest(), db, cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(res.Passes) != 1+len(deepPasses) {
		t.Fatalf("got %d pass reports, want %d", len(res.Passes), 1+len(deepPasses))
	}
	for _, p := range res.Passes {
		if want := string(wantKindForPass(p.Name)); p.Provider != want {
			t.Errorf("pass %s: Provider = %q, want %q", p.Name, p.Provider, want)
		}
	}
}

// The production runner must spawn each pass on the provider resolved for
// that pass NAME, whatever tier it is called with.
func TestSmithRunnerSpawnsPerPassProvider(t *testing.T) {
	var mu sync.Mutex
	got := map[string]provider.Provider{}
	orig := spawnPassSession
	spawnPassSession = func(_ context.Context, _, _, _ string, pv provider.Provider, _ []string, opts smith.SpawnOptions) (*smith.Process, error) {
		mu.Lock()
		defer mu.Unlock()
		got[opts.LogPrefix] = pv
		return nil, errors.New("stub spawn")
	}
	t.Cleanup(func() { spawnPassSession = orig })

	req := testRequest()
	req.WorkDir = t.TempDir()
	run := newSmithRunner(mixedPassConfig(), req)
	for _, pass := range PassNames() {
		// Every pass is called with the review tier: the tier must not decide.
		if _, err := run(context.Background(), pass, tierReview, "prompt"); err == nil {
			t.Fatalf("%s: expected the stub spawn error", pass)
		}
		pv, ok := got[PassLogPrefix(req.LogKey, pass)]
		if !ok {
			t.Fatalf("%s: runner never spawned", pass)
		}
		if pv.Kind != wantKindForPass(pass) {
			t.Errorf("%s: spawned %s, want %s", pass, pv.Kind, wantKindForPass(pass))
		}
	}
}
