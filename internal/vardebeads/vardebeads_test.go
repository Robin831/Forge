package vardebeads

import (
	"reflect"
	"slices"
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
		{"release", []string{"update", "B-1", "--status=open", "--assignee=", "--json"}, []Call{{"beads_release", map[string]any{"session": "Forge", "id": "B-1"}}}},
		{"label before release", []string{"update", "B-1", "--status", "open", "--assignee=", "--add-label=needs-human"}, []Call{
			{"beads_label", map[string]any{"session": "Forge", "id": "B-1", "add": []string{"needs-human"}}},
			{"beads_release", map[string]any{"session": "Forge", "id": "B-1"}}}},
		{"rollback close", []string{"close", "B-1", "--reason", "rollback: chat emission failed"}, []Call{{"beads_close", map[string]any{"session": "Forge", "id": "B-1", "kind": "not-doing", "reason": "rollback: chat emission failed"}}}},
		{"discard close", []string{"close", "B-1", "--reason=Discarded by user during orphan recovery"}, []Call{{"beads_close", map[string]any{"session": "Forge", "id": "B-1", "kind": "not-doing", "reason": "Discarded by user during orphan recovery"}}}},
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
		{"create", "--title", "x", "--metadata", "{}"},
		{"update", "B-1", "--design", "x"},
		{"create", "--title", "x", "--deps", "blocks:B-1,blocks:B-2"},
		{"create", "--title", "x", "--deps", "blocks:B-1,B-2"},
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
		"comments add B-1 x": true, "update B-1 --claim": true, "close B-1": true, "create --title x": true, "dep add a b": true, "github push": true, "": false,
		"dep list B-1": false, "dep tree B-1": false, "label list B-1": false, "github status": false, "label remove B-1 x": true} {
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

// Every argument the translation sends is one Varde's tool takes: Varde refuses unknown fields (DevInfra-gbx.8).
// From varde/app/beads-write.mjs; keep in step when a tool's schema changes.
var vardeSchema = map[string][]string{
	"beads_note": {"session", "id", "text"}, "beads_release": {"session", "id"}, "beads_claim": {"session", "id", "for"},
	"beads_close": {"session", "id", "kind", "pr", "of", "reason", "force"}, "beads_reopen": {"session", "id", "reason"},
	"beads_label": {"session", "id", "add", "remove"}, "beads_dep": {"session", "id", "on", "type", "remove"},
	"beads_update": {"session", "id", "title", "type", "parent", "priority", "description", "acceptance"},
	"beads_create": {"session", "title", "type", "parent", "pool", "priority", "description", "acceptance", "labels", "discovered_from", "split_from", "external_ref"},
}

func TestEveryCallFitsVardesSchema(t *testing.T) {
	for _, args := range [][]string{
		{"update", "B-1", "--status=in_progress"}, {"update", "B-1", "--status=open", "--assignee=", "--add-label=x", "--remove-label=y", "--append-notes", "n", "--title", "t", "--description", "d", "--priority", "1"},
		{"close", "B-1", "--force", "--reason", "r"}, {"reopen", "B-1"}, {"dep", "add", "a", "b", "--type", "related"}, {"dep", "remove", "a", "b"},
		{"label", "add", "B-1", "x"}, {"comments", "add", "B-1", "x"},
		{"create", "--title", "t", "--description", "d", "--type", "bug", "--priority", "2", "--labels", "a", "--deps", "blocks:X"},
		{"create", "--title", "t", "--deps", "discovered-from:X"}, {"create", "--title", "t", "--deps", "X"},
	} {
		plan, err := Translate(args, "Forge", "EP-1")
		if err != nil {
			t.Fatalf("Translate(%v): %v", args, err)
		}
		for _, call := range plan.Calls {
			allowed, ok := vardeSchema[call.Tool]
			if !ok {
				t.Fatalf("%v calls %s, which is not in the schema list", args, call.Tool)
			}
			for field := range call.Args {
				if !slices.Contains(allowed, field) {
					t.Errorf("%v: %s gets %q, which Varde refuses", args, call.Tool, field)
				}
			}
		}
	}
}

// forgechat's real arguments: --deps <id> is depends-on, set with beads_dep after the create, not a split.
func TestForgechatDependsOnBecomesADepAfterCreate(t *testing.T) {
	plan, err := Translate([]string{"create", "--title", "x", "--description", "d", "--type", "task", "--priority", "2", "--json", "--labels", "a,b", "--deps", "B-7"}, "Forge", "EP-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, split := plan.Calls[0].Args["split_from"]; split || !reflect.DeepEqual(plan.DependsOn, []string{"B-7"}) {
		t.Errorf("plan = %+v", plan)
	}
}

func TestLongTextIsCutToVardesLimits(t *testing.T) {
	long := strings.Repeat("å", 9000)
	plan, _ := Translate([]string{"create", "--title", long, "--description", long}, "Forge", "EP-1")
	if n := len([]rune(plan.Calls[0].Args["title"].(string))); n != limitTitle {
		t.Errorf("title is %d runes", n)
	}
	if n := len([]rune(plan.Calls[0].Args["description"].(string))); n != limitText {
		t.Errorf("description is %d runes", n)
	}
	plan, _ = Translate([]string{"close", "B-1", "--reason", long}, "Forge", "")
	if n := len([]rune(plan.Calls[0].Args["reason"].(string))); n != limitReason {
		t.Errorf("reason is %d runes", n)
	}
}
