package vardebeads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Caller is what Run needs from a Client (a fake in tests).
type Caller interface {
	Call(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error)
}

// Run carries out one bd write through Varde and writes what bd would have printed for it: the new id for a create
// (bare with --silent, {"id": …} otherwise), {"ok": true} for anything else with --json. It returns a non-nil error
// whenever bd would have exited non-zero.
func Run(ctx context.Context, c Caller, args []string, session, anvilParent string, stdout io.Writer) error {
	plan, err := Translate(args, session, anvilParent)
	if err != nil {
		return err
	}
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		}
	}
	if plan.NoOp {
		if jsonOut {
			fmt.Fprintln(stdout, `{"ok":true}`)
		}
		return nil
	}
	if plan.Lookup != "" {
		raw, err := c.Call(ctx, "beads_show", map[string]any{"session": session, "id": plan.Lookup})
		if err != nil {
			return fmt.Errorf("finding the parent of %s: %w", plan.Lookup, err)
		}
		var shown struct {
			State struct {
				Parent string `json:"parent"`
			} `json:"state"`
		}
		_ = json.Unmarshal(raw, &shown)
		parent := shown.State.Parent
		if parent == "" {
			parent = anvilParent
		}
		if parent == "" {
			return fmt.Errorf("%s has no parent, and this anvil has no beads_parent", plan.Lookup)
		}
		plan.Calls[0].Args["parent"] = parent
	}
	var created string
	for _, call := range plan.Calls {
		raw, err := c.Call(ctx, call.Tool, call.Args)
		var refused *ErrRefused
		if errors.As(err, &refused) && tolerated(refused.Text, plan.Tolerate) {
			continue
		}
		if err != nil {
			return err
		}
		if plan.Create {
			var answer struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &answer) != nil || answer.ID == "" {
				return fmt.Errorf("varde %s: no id in the answer", call.Tool)
			}
			created = answer.ID
		}
	}
	// bd create --deps <id>: the new bead waits on <id>. Without that wait it would be dispatched too early, so a bead
	// whose wait could not be set is closed again and the create fails as a whole, as bd's would.
	for _, on := range plan.DependsOn {
		if _, err := c.Call(ctx, "beads_dep", map[string]any{"session": session, "id": created, "on": on, "type": "blocks"}); err != nil {
			_, closeErr := c.Call(context.WithoutCancel(ctx), "beads_close", map[string]any{"session": session, "id": created, "kind": "not-doing", "reason": "rollback: its dependency on " + on + " could not be set"})
			if closeErr != nil {
				return fmt.Errorf("created %s, could not make it wait on %s (%w), and could not close it again: %v", created, on, err, closeErr)
			}
			return fmt.Errorf("could not make the new bead wait on %s, so it was closed again: %w", on, err)
		}
	}
	switch {
	case plan.Create && plan.Silent:
		fmt.Fprintln(stdout, created)
	case plan.Create:
		out, _ := json.Marshal(map[string]string{"id": created})
		fmt.Fprintln(stdout, string(out))
	case jsonOut:
		fmt.Fprintln(stdout, `{"ok":true}`)
	}
	return nil
}

func tolerated(text string, phrases []string) bool {
	for _, p := range phrases {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}
