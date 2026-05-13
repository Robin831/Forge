package forgechat

import (
	"strings"
	"testing"
)

func TestParseEmissionResponse_FencedJSON(t *testing.T) {
	out := "preamble\n```json\n" +
		`{"summary":"split into two","beads":[{"id":"p1","anvil":"forge","title":"Foo","description":"do foo","type":"feature","priority":2}]}` +
		"\n```\nchatter"
	env, err := ParseEmissionResponse(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(env.Beads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(env.Beads))
	}
	if env.Beads[0].ProposalID != "p1" {
		t.Errorf("proposal id lost: %q", env.Beads[0].ProposalID)
	}
	if env.Summary != "split into two" {
		t.Errorf("summary lost: %q", env.Summary)
	}
}

func TestParseEmissionResponse_BareJSON(t *testing.T) {
	out := `chatter before {"beads":[{"id":"p1","anvil":"forge","title":"X","description":"","type":"task","priority":2}]} trailing`
	env, err := ParseEmissionResponse(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(env.Beads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(env.Beads))
	}
}

func TestValidateEmission_DetectsCycle(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "a", Title: "1", Type: "task", Priority: 2, DependsOn: []string{"p2"}},
		{ProposalID: "p2", Anvil: "a", Title: "2", Type: "task", Priority: 2, DependsOn: []string{"p3"}},
		{ProposalID: "p3", Anvil: "a", Title: "3", Type: "task", Priority: 2, DependsOn: []string{"p1"}},
	}}
	problems := ValidateEmission(env, map[string]bool{"a": true})
	if len(problems) == 0 {
		t.Fatal("expected cycle detection to report at least one problem")
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "cycle") {
		t.Errorf("expected 'cycle' in problems, got %q", joined)
	}
}

func TestValidateEmission_DetectsSelfDep(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "a", Title: "1", Type: "task", Priority: 2, DependsOn: []string{"p1"}},
	}}
	problems := ValidateEmission(env, map[string]bool{"a": true})
	if len(problems) == 0 {
		t.Fatal("expected self-dep to be rejected")
	}
}

func TestValidateEmission_RejectsCrossAnvilDeps(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "alpha", Title: "1", Type: "task", Priority: 2},
		{ProposalID: "p2", Anvil: "beta", Title: "2", Type: "task", Priority: 2, DependsOn: []string{"p1"}},
	}}
	problems := ValidateEmission(env, map[string]bool{"alpha": true, "beta": true})
	if len(problems) == 0 {
		t.Fatal("expected cross-anvil dep to be rejected")
	}
	if !strings.Contains(strings.Join(problems, "\n"), "cross-anvil") {
		t.Errorf("expected 'cross-anvil' in problems, got %v", problems)
	}
}

func TestValidateEmission_RejectsUnknownAnvil(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "ghost", Title: "1", Type: "task", Priority: 2},
	}}
	problems := ValidateEmission(env, map[string]bool{"alpha": true})
	if len(problems) == 0 {
		t.Fatal("expected unknown anvil to be rejected")
	}
}

func TestValidateEmission_RejectsBadType(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "alpha", Title: "1", Type: "story", Priority: 2},
	}}
	problems := ValidateEmission(env, map[string]bool{"alpha": true})
	if len(problems) == 0 {
		t.Fatal("expected unknown bead type to be rejected")
	}
}

func TestValidateEmission_RejectsBadPriority(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "alpha", Title: "1", Type: "task", Priority: 9},
	}}
	problems := ValidateEmission(env, map[string]bool{"alpha": true})
	if len(problems) == 0 {
		t.Fatal("expected priority out of range to be rejected")
	}
}

func TestValidateEmission_DefaultsBlankTypeToTask(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "alpha", Title: "1", Type: "", Priority: 2},
	}}
	problems := ValidateEmission(env, map[string]bool{"alpha": true})
	if len(problems) > 0 {
		t.Fatalf("expected blank type to default to task, got %v", problems)
	}
	if env.Beads[0].Type != "task" {
		t.Errorf("expected blank type to be normalised to 'task', got %q", env.Beads[0].Type)
	}
}

func TestValidateEmission_DuplicateProposalIDs(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "a", Title: "1", Type: "task", Priority: 2},
		{ProposalID: "p1", Anvil: "a", Title: "2", Type: "task", Priority: 2},
	}}
	problems := ValidateEmission(env, map[string]bool{"a": true})
	if len(problems) == 0 {
		t.Fatal("expected duplicate proposal ids to be rejected")
	}
}

func TestParseEmissionResponse_FencedWithCodeInDescription(t *testing.T) {
	// Description contains its own ```json block — the parser must not treat
	// the inner fence as the closing fence of the outer block.
	inner := "```json\n{\"key\":\"value\"}\n```"
	bead := `{"id":"p1","anvil":"forge","title":"Foo","description":"` + "see code: \\n\\n```json\\n{\\\"key\\\":\\\"value\\\"}\\n```" + `","type":"task","priority":2}`
	out := "```json\n" +
		`{"beads":[` + bead + `]}` +
		"\n```"
	env, err := ParseEmissionResponse(out)
	if err != nil {
		t.Fatalf("parse: %v (inner fence was: %q)", err, inner)
	}
	if len(env.Beads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(env.Beads))
	}
}

func TestParseEmissionResponse_BareJSONWithBracesInStrings(t *testing.T) {
	// Description field contains unbalanced braces — the json.Decoder fallback
	// must handle this correctly (unlike a brace-counting scanner).
	out := `some preamble {"beads":[{"id":"p1","anvil":"forge","title":"X","description":"use { and } freely","type":"task","priority":2}]} trailing`
	env, err := ParseEmissionResponse(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(env.Beads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(env.Beads))
	}
}

func TestValidateEmission_CaseInsensitiveAnvilMatch(t *testing.T) {
	// Emission uses "Forge" (capital F) but the registered key is "forge".
	// ValidateEmission must accept the match and normalise the bead's Anvil
	// to the canonical casing so downstream routing works.
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "Forge", Title: "1", Type: "task", Priority: 2},
	}}
	problems := ValidateEmission(env, map[string]bool{"forge": true})
	if len(problems) > 0 {
		t.Fatalf("expected case-insensitive anvil match to pass, got %v", problems)
	}
	if env.Beads[0].Anvil != "forge" {
		t.Errorf("expected anvil to be normalised to canonical key %q, got %q", "forge", env.Beads[0].Anvil)
	}
}

func TestValidateEmission_AllValidPasses(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "a", Title: "Foo", Type: "feature", Priority: 1},
		{ProposalID: "p2", Anvil: "a", Title: "Bar", Type: "task", Priority: 2, DependsOn: []string{"p1"}},
	}}
	problems := ValidateEmission(env, map[string]bool{"a": true})
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}
}

func TestBuildPrompt_EmitMode_IncludesAnvilList(t *testing.T) {
	p := BuildPrompt(TurnRequest{
		Stage:  StageReady,
		Mode:   ModeEmit,
		Title:  "Foo",
		Plan:   "# plan",
		Anvils: AnvilContext{"forge": "/path/to/forge", "hytte": "/path/to/hytte"},
	})
	if !strings.Contains(p, "Registered anvils") {
		t.Fatal("emit prompt must include the registered anvils block")
	}
	if !strings.Contains(p, "forge") || !strings.Contains(p, "hytte") {
		t.Fatalf("emit prompt must list each anvil, got %q", p)
	}
	if !strings.Contains(p, "DAG") {
		t.Fatal("emit prompt should mention the DAG constraint")
	}
}

// TestBuildPrompt_EmitMode_PrefersSingleBead asserts the prompt instructs claude
// to default to ONE bead instead of splitting per-verb. Regression guard for
// the over-splitting bug where simple changes (e.g. a dependency upgrade) were
// fanned out into four sequential beads (audit / bump / migrate / test).
func TestBuildPrompt_EmitMode_PrefersSingleBead(t *testing.T) {
	p := BuildPrompt(TurnRequest{
		Stage:  StageReady,
		Mode:   ModeEmit,
		Title:  "Foo",
		Plan:   "# plan",
		Anvils: AnvilContext{"forge": "/path/to/forge"},
	})
	if !strings.Contains(p, "Default to ONE bead") {
		t.Errorf("emit prompt must default to one bead, got %q", p)
	}
	if !strings.Contains(p, "one branch, one sitting, one PR") {
		t.Errorf("emit prompt should include the one-developer-one-PR heuristic, got %q", p)
	}
	if !strings.Contains(p, "phases") {
		t.Errorf("emit prompt should explicitly warn against splitting by phase, got %q", p)
	}
}

func TestInterpretResponse_EmitParsesEnvelope(t *testing.T) {
	out := "```json\n" +
		`{"beads":[{"id":"p1","anvil":"a","title":"X","description":"d","type":"task","priority":2}]}` +
		"\n```"
	resp, err := interpretResponse(TurnRequest{Stage: StageReady, Mode: ModeEmit}, out, 0.05)
	if err != nil {
		t.Fatalf("interpret: %v", err)
	}
	if resp.Emission == nil {
		t.Fatal("expected Emission to be populated")
	}
	if len(resp.Emission.Beads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(resp.Emission.Beads))
	}
	if resp.CostUSD != 0.05 {
		t.Errorf("expected cost to round-trip, got %v", resp.CostUSD)
	}
}

func TestTopoSort_DAGYieldsTopologicalOrder(t *testing.T) {
	beads := []BeadProposal{
		{ProposalID: "p3", DependsOn: []string{"p1", "p2"}},
		{ProposalID: "p1"},
		{ProposalID: "p2", DependsOn: []string{"p1"}},
	}
	order, err := topoSort(beads)
	if err != nil {
		t.Fatalf("topo: %v", err)
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if pos["p1"] >= pos["p2"] {
		t.Errorf("p1 must come before p2: %v", order)
	}
	if pos["p2"] >= pos["p3"] {
		t.Errorf("p2 must come before p3: %v", order)
	}
	if pos["p1"] >= pos["p3"] {
		t.Errorf("p1 must come before p3: %v", order)
	}
}

func TestTopoSort_CycleReturnsError(t *testing.T) {
	beads := []BeadProposal{
		{ProposalID: "p1", DependsOn: []string{"p2"}},
		{ProposalID: "p2", DependsOn: []string{"p1"}},
	}
	if _, err := topoSort(beads); err == nil {
		t.Fatal("expected cycle to be reported")
	}
}
