// Package vardebeads sends the Forge's bead writes to Varde instead of the beads database (DevInfra-gbx.8): the
// daemon's own bd write commands are translated into Varde's MCP bead tools, signed with the Forge's key, so the
// Forge needs only a read-only database user. Reads stay on bd.
package vardebeads

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The environment that switches it on, set by the chart: Varde's base URL and the file holding the Forge's key.
const (
	EnvURL     = "FORGE_VARDE_URL"
	EnvKeyFile = "FORGE_VARDE_KEY_FILE"
	EnvSession = "FORGE_VARDE_SESSION"
)

// HalfSet reports a configuration that names Varde but cannot reach it: one of the two variables without the other.
func HalfSet() bool { return (os.Getenv(EnvURL) == "") != (os.Getenv(EnvKeyFile) == "") }

// Enabled reports whether bead writes go through Varde.
func Enabled() bool { return os.Getenv(EnvURL) != "" && os.Getenv(EnvKeyFile) != "" }

// IsWrite reports whether a bd argument list writes. Reads (show, list, ready, sql, comments without add, --help)
// stay on bd.
func IsWrite(args []string) bool {
	if len(args) == 0 {
		return false
	}
	sub := ""
	if len(args) > 1 {
		sub = args[1]
	}
	switch args[0] {
	case "create", "update", "close", "reopen", "delete":
		return true
	case "dep", "label":
		return sub == "add" || sub == "remove"
	case "comments":
		return sub == "add"
	case "github":
		return sub == "push"
	}
	return false
}

// Call is one Varde MCP tool call.
type Call struct {
	Tool string
	Args map[string]any
}

// Plan is what one bd write becomes: the calls in order, and how to answer the caller.
type Plan struct {
	Calls []Call
	// Create: print the new bead's id, bare (bd create --silent) or as {"id": …} (--json).
	Create, Silent bool
	// Lookup: before the calls, the parent of this bead is read (beads_show) and set as the create's parent.
	Lookup string
	// Tolerate: refusals that mean the bead is already as asked (a release with no claim, a close of a closed bead).
	Tolerate []string
	// NoOp: nothing to send (bd github push; external_ref is set at create).
	NoOp bool
	// DependsOn: after the create, the new bead is made to depend on these (bd create --deps <id>).
	DependsOn []string
}

// Varde's limits on what it stores; longer text is cut with a marker rather than refused.
const (
	limitTitle  = 200
	limitText   = 8000
	limitNote   = 4000
	limitReason = 500
)

func cut(text string, limit int) string {
	r := []rune(text)
	if len(r) <= limit {
		return text
	}
	const mark = " … (cut to Varde's limit)"
	return string(r[:limit-len([]rune(mark))]) + mark
}

// The flags each bd write may carry; any other flag is refused, so nothing is silently dropped.
var allowedFlags = map[string]map[string]bool{
	"create":   {"title": true, "description": true, "type": true, "priority": true, "labels": true, "deps": true, "json": true, "silent": true},
	"update":   {"status": true, "assignee": true, "add-label": true, "remove-label": true, "append-notes": true, "title": true, "description": true, "priority": true, "json": true},
	"close":    {"reason": true, "force": true, "json": true},
	"reopen":   {"reason": true, "json": true},
	"dep":      {"type": true, "json": true},
	"label":    {"json": true},
	"comments": {"json": true},
	"github":   {"json": true},
}

// notDoing tells a close that delivered nothing (a rollback, a discard, a stop) from finished work, by the Forge's own reasons.
func notDoing(reason string) bool {
	r := strings.ToLower(reason)
	return strings.Contains(r, "rollback") || strings.HasPrefix(r, "discarded") || strings.HasPrefix(r, "stopped")
}

type parsed struct {
	pos   []string
	flags map[string][]string
	bools map[string]bool
}

// Flags that take no value in the bd writes the Forge makes.
var boolFlags = map[string]bool{"json": true, "silent": true, "force": true, "quiet": true}

func parse(args []string) parsed {
	p := parsed{flags: map[string][]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			p.pos = append(p.pos, a)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if boolFlags[name] && !hasValue {
			p.bools[name] = true
			continue
		}
		if !hasValue && i+1 < len(args) {
			i++
			value = args[i]
		}
		p.flags[name] = append(p.flags[name], value)
	}
	return p
}

func (p parsed) one(name string) (string, bool) {
	v, ok := p.flags[name]
	if !ok || len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

func splitList(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// Translate turns one bd write into Varde calls. anvilParent is the anvil's beads_parent: the epic a new bead goes
// under when nothing else names one (the scanners' beads, Robin's choice (a), 2.11).
func Translate(args []string, session, anvilParent string) (Plan, error) {
	if len(args) == 0 {
		return Plan{}, fmt.Errorf("no bd command")
	}
	p := parse(args[1:])
	if allowed, ok := allowedFlags[args[0]]; ok {
		for name := range p.flags {
			if !allowed[name] {
				return Plan{}, fmt.Errorf("bd %s --%s has no counterpart in Varde; nothing was written", args[0], name)
			}
		}
		for name := range p.bools {
			if !allowed[name] {
				return Plan{}, fmt.Errorf("bd %s --%s has no counterpart in Varde; nothing was written", args[0], name)
			}
		}
	}
	with := func(m map[string]any) map[string]any { m["session"] = session; return m }
	switch args[0] {
	case "github":
		return Plan{NoOp: true}, nil
	case "comments":
		if len(p.pos) < 3 {
			return Plan{}, fmt.Errorf("bd comments add needs an id and a text")
		}
		return Plan{Calls: []Call{{"beads_note", with(map[string]any{"id": p.pos[1], "text": cut(strings.Join(p.pos[2:], " "), limitNote)})}}}, nil
	case "label":
		if len(p.pos) < 3 || (p.pos[0] != "add" && p.pos[0] != "remove") {
			return Plan{}, fmt.Errorf("bd label: only add and remove, with an id and a label")
		}
		key := p.pos[0]
		return Plan{Calls: []Call{{"beads_label", with(map[string]any{"id": p.pos[1], key: p.pos[2:]})}}}, nil
	case "dep":
		if len(p.pos) < 3 || (p.pos[0] != "add" && p.pos[0] != "remove") {
			return Plan{}, fmt.Errorf("bd dep: only add and remove, with two ids")
		}
		m := with(map[string]any{"id": p.pos[1], "on": p.pos[2], "type": "blocks"})
		if t, ok := p.one("type"); ok && t != "" {
			m["type"] = t
		}
		if p.pos[0] == "remove" {
			m["remove"] = true
		}
		return Plan{Calls: []Call{{"beads_dep", m}}}, nil
	case "close":
		if len(p.pos) < 1 {
			return Plan{}, fmt.Errorf("bd close needs an id")
		}
		reason, _ := p.one("reason")
		if strings.TrimSpace(reason) == "" {
			reason = "Closed by the Forge."
		}
		kind := "done"
		if notDoing(reason) {
			kind = "not-doing"
		}
		m := with(map[string]any{"id": p.pos[0], "kind": kind, "reason": cut(reason, limitReason)})
		if p.bools["force"] {
			m["force"] = true
		}
		return Plan{Calls: []Call{{"beads_close", m}}, Tolerate: []string{"closed already", "tombstone already"}}, nil
	case "reopen":
		if len(p.pos) < 1 {
			return Plan{}, fmt.Errorf("bd reopen needs an id")
		}
		reason, _ := p.one("reason")
		if reason == "" {
			reason = "Reopened by the Forge."
		}
		return Plan{Calls: []Call{{"beads_reopen", with(map[string]any{"id": p.pos[0], "reason": cut(reason, limitReason)})}}}, nil
	case "update":
		return translateUpdate(p, with)
	case "create":
		return translateCreate(p, with, anvilParent)
	}
	return Plan{}, fmt.Errorf("bd %s is not something the Forge writes through Varde", args[0])
}

func translateUpdate(p parsed, with func(map[string]any) map[string]any) (Plan, error) {
	if len(p.pos) < 1 {
		return Plan{}, fmt.Errorf("bd update needs an id")
	}
	id := p.pos[0]
	var plan Plan
	// Labels first: an escalation that fails halfway must not leave a released bead without needs-human.
	if add, remove := splitList(p.flags["add-label"]), splitList(p.flags["remove-label"]); len(add)+len(remove) > 0 {
		m := with(map[string]any{"id": id})
		if len(add) > 0 {
			m["add"] = add
		}
		if len(remove) > 0 {
			m["remove"] = remove
		}
		plan.Calls = append(plan.Calls, Call{"beads_label", m})
	}
	status, hasStatus := p.one("status")
	assignee, setsAssignee := p.one("assignee")
	if setsAssignee && assignee != "" {
		return Plan{}, fmt.Errorf("bd update --assignee=%s: Varde sets an assignee only by a claim", assignee)
	}
	clearsAssignee := setsAssignee
	if hasStatus || clearsAssignee {
		switch {
		case status == "in_progress":
			plan.Calls = append(plan.Calls, Call{"beads_claim", with(map[string]any{"id": id})})
		case status == "open" || (!hasStatus && clearsAssignee):
			plan.Calls = append(plan.Calls, Call{"beads_release", with(map[string]any{"id": id})})
			plan.Tolerate = append(plan.Tolerate, "no assignee", "no claim to let go")
		default:
			return Plan{}, fmt.Errorf("bd update --status=%s is not one the Forge writes through Varde", status)
		}
	}
	if notes, ok := p.one("append-notes"); ok {
		plan.Calls = append(plan.Calls, Call{"beads_note", with(map[string]any{"id": id, "text": cut(notes, limitNote)})})
	}
	fields := with(map[string]any{"id": id})
	if v, ok := p.one("title"); ok {
		fields["title"] = cut(v, limitTitle)
	}
	if v, ok := p.one("description"); ok {
		fields["description"] = cut(v, limitText)
	}
	if v, ok := p.one("priority"); ok {
		n, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(v), "P"))
		if err != nil {
			return Plan{}, fmt.Errorf("bd update --priority %q is not a number", v)
		}
		fields["priority"] = n
	}
	if len(fields) > 2 {
		plan.Calls = append(plan.Calls, Call{"beads_update", fields})
	}
	if len(plan.Calls) == 0 {
		return Plan{}, fmt.Errorf("bd update changes nothing the Forge writes through Varde")
	}
	return plan, nil
}

func translateCreate(p parsed, with func(map[string]any) map[string]any, anvilParent string) (Plan, error) {
	title, _ := p.one("title")
	if strings.TrimSpace(title) == "" {
		return Plan{}, fmt.Errorf("bd create needs --title")
	}
	m := with(map[string]any{"title": cut(title, limitTitle)})
	if v, ok := p.one("description"); ok {
		m["description"] = cut(v, limitText)
	}
	m["type"] = "task"
	if v, ok := p.one("type"); ok && v != "" {
		m["type"] = v
	}
	if v, ok := p.one("priority"); ok {
		n, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(v), "P"))
		if err != nil {
			return Plan{}, fmt.Errorf("bd create --priority %q is not a number", v)
		}
		m["priority"] = n
	}
	if labels := splitList(p.flags["labels"]); len(labels) > 0 {
		m["labels"] = labels
	}
	plan := Plan{Create: true, Silent: p.bools["silent"]}
	for _, dep := range splitList(p.flags["deps"]) {
		kind, on, ok := strings.Cut(dep, ":")
		if !ok {
			kind, on = "depends-on", dep
		}
		switch kind {
		case "blocks":
			// bd's blocks:X is "the new bead blocks X": only schematic's parts of a split bead X say it. Varde puts the
			// part under X's parent, with X's own Forge label allowed, and sets the dependency.
			if _, twice := m["split_from"]; twice {
				return Plan{}, fmt.Errorf("bd create --deps: a part belongs to one split bead, not several")
			}
			m["split_from"] = on
		case "discovered-from":
			m["discovered_from"] = on
			plan.Lookup = on
		case "depends-on", "blocked-by":
			plan.DependsOn = append(plan.DependsOn, on)
		default:
			return Plan{}, fmt.Errorf("bd create --deps %s: only blocks, depends-on and discovered-from", dep)
		}
	}
	if _, split := m["split_from"]; split && (plan.Lookup != "" || len(plan.DependsOn) > 0) {
		return Plan{}, fmt.Errorf("bd create --deps: a part of a split bead takes no other dependency")
	}
	if _, split := m["split_from"]; !split && plan.Lookup == "" {
		if anvilParent == "" {
			return Plan{}, fmt.Errorf("bd create: this anvil has no beads_parent, and Varde files every bead under an epic")
		}
		m["parent"] = anvilParent
	}
	plan.Calls = []Call{{"beads_create", m}}
	return plan, nil
}

// HandOverRefusal is why adding label is refused when bead writes go through Varde, or "" when it is not: a Forge
// dispatch label hands a bead to the Forge, and Varde does that only through beads_forge_label (gate and cap).
func HandOverRefusal(label string) string {
	if !Enabled() || !strings.HasPrefix(strings.ToLower(label), "forge") || label == "forge-decomposed" {
		return ""
	}
	return fmt.Sprintf("%s hands a bead to the Forge; through Varde that is beads_forge_label (the coordinator, past the forge-ready gate and the cap), not a label write", label)
}
