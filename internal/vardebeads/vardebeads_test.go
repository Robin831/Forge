package vardebeads

import (
	"reflect"
	"strings"
	"testing"
)

func TestTranslateMapsTheForgesWrites(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []Call
	}{
		{"claim", []string{"update", "B-1", "--status=in_progress", "--json"}, []Call{{"beads_claim", map[string]any{"session": "Forge", "id": "B-1"}}}},
		{"release", []string{"update", "B-1", "--status=open", "--assignee=", "--json"}, []Call{{"beads_release", map[string]any{"session": "Forge", "id": "B-1", "why": "Released by the Forge."}}}},
		{"release and label", []string{"update", "B-1", "--status", "open", "--assignee=", "--add-label=needs-human"}, []Call{
			{"beads_release", map[string]any{"session": "Forge", "id": "B-1", "why": "Released by the Forge."}},
			{"beads_label", map[string]any{"session": "Forge", "id": "B-1", "add": []string{"needs-human"}}}}},
		{"note", []string{"update", "B-1", "--append-notes", "Smith started."}, []Call{{"beads_note", map[string]any{"session": "Forge", "id": "B-1", "text": "Smith started."}}}},
		{"comment", []string{"comments", "add", "B-1", "PR opened"}, []Call{{"beads_note", map[string]any{"session": "Forge", "id": "B-1", "text": "PR opened"}}}},
		{"label add", []string{"label", "add", "B-1", "forge-decomposed"}, []Call{{"beads_label", map[string]any{"session": "Forge", "id": "B-1", "add": []string{"forge-decomposed"}}}}},
		{"close", []string{"close", "B-1", "--reason=Merged #12", "--json"}, []Call{{"beads_close", map[string]any{"session": "Forge", "id": "B-1", "kind": "done", "reason": "Merged #12"}}}},
		{"force close", []string{"close", "B-1", "--force", "--reason", "All parts done"}, []Call{{"beads_close", map[string]any{"session": "Forge", "id": "B-1", "kind": "done", "reason": "All parts done", "force": true}}}},
		{"dep", []string{"dep", "add", "B-2", "B-1"}, []Call{{"beads_dep", map[string]any{"session": "Forge", "id": "B-2", "on": "B-1", "type": "blocks"}}}},
		{"priority", []string{"update", "B-1", "--priority", "P1"}, []Call{{"beads_update", map[string]any{"session": "Forge", "id": "B-1", "priority": 1}}}},
		{"split part", []string{"create", "--title", "Part 1", "--type=task", "--priority=2", "--deps", "blocks:B-1", "--labels", "schematic:B-1", "--json"}, []Call{
			{"beads_create", map[string]any{"session": "Forge", "title": "Part 1", "type": "task", "priority": 2, "labels": []string{"schematic:B-1"}, "split_from": "B-1"}}}},
		{"scanner bead", []string{"create", "--title=Bump x", "--type=bug", "--labels=deps,security", "--silent"}, []Call{
			{"beads_create", map[string]any{"session": "Forge", "title": "Bump x", "type": "bug", "labels": []string{"deps", "security"}, "parent": "EP-1"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := Translate(c.args, "Forge", "EP-1")
			if err != nil {
				t.Fatalf("Translate(%v): %v", c.args, err)
			}
			if !reflect.DeepEqual(plan.Calls, c.want) {
				t.Errorf("Translate(%v)\n got  %#v\n want %#v", c.args, plan.Calls, c.want)
			}
		})
	}
}

func TestTranslateRefusesWhatVardeCannotDoSafely(t *testing.T) {
	for _, args := range [][]string{
		{"update", "B-1", "--notes", "replaces every note"},
		{"update", "B-1", "--status=blocked"},
		{"update", "B-1", "--assignee=bob"},
		{"update", "B-1"},
		{"create", "--title", "No home"},
		{"create", "--title", "x", "--deps", "related:B-1"},
		{"delete", "B-1"},
		{"label", "list", "B-1"},
	} {
		parent := "EP-1"
		if args[0] == "create" && len(args) == 3 {
			parent = ""
		}
		if _, err := Translate(args, "Forge", parent); err == nil {
			t.Errorf("Translate(%v) = nil error, want a refusal", args)
		}
	}
}

func TestPlanShapes(t *testing.T) {
	if p, _ := Translate([]string{"github", "push"}, "Forge", ""); !p.NoOp {
		t.Error("bd github push is dropped (Robin's choice (c))")
	}
	if p, _ := Translate([]string{"create", "--title", "x", "--silent"}, "Forge", "EP-1"); !p.Create || !p.Silent {
		t.Errorf("create --silent: %+v", p)
	}
	if p, _ := Translate([]string{"close", "B-1"}, "Forge", ""); !strings.Contains(p.Calls[0].Args["reason"].(string), "Forge") {
		t.Error("a close without a reason still sends one")
	}
}

func TestIsWrite(t *testing.T) {
	for args, want := range map[string]bool{"show B-1 --json": false, "list --json": false, "ready": false, "sql x": false, "comments B-1": false,
		"comments add B-1 x": true, "update B-1 --claim": true, "close B-1": true, "create --title x": true, "dep add a b": true, "github push": true, "": false} {
		if got := IsWrite(strings.Fields(args)); got != want {
			t.Errorf("IsWrite(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestHandOverRefusal(t *testing.T) {
	if HandOverRefusal("forgeSkybert") != "" {
		t.Error("with bd, a label write is a label write")
	}
	t.Setenv(EnvURL, "http://varde.invalid")
	t.Setenv(EnvKeyFile, "/nowhere")
	for label, refused := range map[string]bool{"forgeSkybert": true, "ForgeExplorerSkybert": true, "forge-decomposed": false, "needs-human": false, "neste": false} {
		if got := HandOverRefusal(label) != ""; got != refused {
			t.Errorf("HandOverRefusal(%q) refused=%v, want %v", label, got, refused)
		}
	}
}
