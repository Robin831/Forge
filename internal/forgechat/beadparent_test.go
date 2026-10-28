package forgechat

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestMaterializeEmissionWithParents_PassesEachAnvilsParent(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "heimdall", Title: "One", Type: "task", Priority: 2},
		{ProposalID: "p2", Anvil: "devinfra", Title: "Two", Priority: 2},
	}}
	bd := &fakeBd{createIDs: []string{"Heimdall-6tyq.1", "DevInfra-a"}}
	parents := func(anvil string) string {
		return map[string]string{"heimdall": "Heimdall-6tyq"}[anvil]
	}

	res := MaterializeEmissionWithParents(context.Background(), logger, env,
		anvilLookup(map[string]string{"heimdall": "/h", "devinfra": "/d"}), parents, bd.run)
	if res.Err != nil {
		t.Fatalf("materialize: %v", res.Err)
	}

	creates := bd.argsFor("create")
	if len(creates) != 2 {
		t.Fatalf("want 2 creates, got %d", len(creates))
	}
	byDir := map[string]string{}
	for _, c := range creates {
		byDir[c[0]] = strings.Join(c[1:], " ")
	}
	if !strings.Contains(byDir["@/h"], "--parent Heimdall-6tyq --no-inherit-labels") {
		t.Errorf("heimdall bead lacks its parent: %s", byDir["@/h"])
	}
	if strings.Contains(byDir["@/d"], "--parent") {
		t.Errorf("devinfra has no parent configured, yet: %s", byDir["@/d"])
	}
	if !strings.Contains(byDir["@/d"], "--type task") {
		t.Errorf("a proposal with no type must be sent as task: %s", byDir["@/d"])
	}
	if strings.Count(logs.String(), "no parent epic") != 1 || !strings.Contains(logs.String(), "anvil=devinfra") {
		t.Errorf("want one no-parent warning, for devinfra; logged %q", logs.String())
	}
}

func TestMaterializeEmission_NilParentsWarnsPerBead(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "alpha", Title: "One", Type: "task", Priority: 2},
		{ProposalID: "p2", Anvil: "alpha", Title: "Two", Type: "bug", Priority: 2},
	}}
	bd := &fakeBd{createIDs: []string{"a-1", "a-2"}}

	res := MaterializeEmission(context.Background(), logger, env, anvilLookup(map[string]string{"alpha": "/a"}), bd.run)
	if res.Err != nil {
		t.Fatalf("materialize: %v", res.Err)
	}
	for _, c := range bd.argsFor("create") {
		if strings.Contains(strings.Join(c, " "), "--parent") {
			t.Errorf("no parent lookup, yet: %q", c)
		}
	}
	if n := strings.Count(logs.String(), "no parent epic"); n != 2 {
		t.Errorf("want one warning per created bead (2), got %d: %q", n, logs.String())
	}
}
