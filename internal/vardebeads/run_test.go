package vardebeads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeVarde struct {
	calls   []Call
	answers map[string]json.RawMessage
	refuse  map[string]string
}

func (f *fakeVarde) Call(_ context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	f.calls = append(f.calls, Call{tool, args})
	if text, ok := f.refuse[tool]; ok {
		return nil, &ErrRefused{Text: text}
	}
	return f.answers[tool], nil
}

func TestRunPrintsWhatBdWould(t *testing.T) {
	f := &fakeVarde{answers: map[string]json.RawMessage{"beads_create": json.RawMessage(`{"ok":true,"id":"B-9"}`)}}
	var out bytes.Buffer
	if err := Run(context.Background(), f, []string{"create", "--title", "x", "--silent"}, "Forge", "EP-1", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "B-9\n" {
		t.Errorf("--silent printed %q", out.String())
	}
	out.Reset()
	if err := Run(context.Background(), f, []string{"create", "--title", "x", "--json"}, "Forge", "EP-1", &out); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != `{"id":"B-9"}` {
		t.Errorf("--json printed %q", out.String())
	}
}

func TestRunToleratesOnlyTheNamedRefusals(t *testing.T) {
	f := &fakeVarde{refuse: map[string]string{"beads_close": "Refused (403): B-1 is closed already."}}
	if err := Run(context.Background(), f, []string{"close", "B-1"}, "Forge", "", &bytes.Buffer{}); err != nil {
		t.Errorf("closing a closed bead is what bd would shrug off: %v", err)
	}
	f = &fakeVarde{refuse: map[string]string{"beads_close": "Refused (403): B-1 has a forge label: the Forge has it."}}
	err := Run(context.Background(), f, []string{"close", "B-1"}, "Forge", "", &bytes.Buffer{})
	var refused *ErrRefused
	if !errors.As(err, &refused) {
		t.Errorf("any other refusal is an error, got %v", err)
	}
}

func TestRunFindsTheParentForADiscoveredBead(t *testing.T) {
	f := &fakeVarde{answers: map[string]json.RawMessage{
		"beads_show":   json.RawMessage(`{"state":{"id":"B-1","parent":"EP-7"}}`),
		"beads_create": json.RawMessage(`{"ok":true,"id":"B-2"}`)}}
	if err := Run(context.Background(), f, []string{"create", "--title", "x", "--deps", "discovered-from:B-1", "--json"}, "Forge", "EP-1", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[1].Args["parent"] != "EP-7" || f.calls[1].Args["discovered_from"] != "B-1" {
		t.Errorf("calls = %+v", f.calls)
	}
}

func TestRunStopsAtTheFirstFailure(t *testing.T) {
	f := &fakeVarde{refuse: map[string]string{"beads_release": "Refused (403): B-1 is assigned to claude/sophie."}}
	err := Run(context.Background(), f, []string{"update", "B-1", "--status=open", "--assignee=", "--add-label=needs-human"}, "Forge", "", &bytes.Buffer{})
	if err == nil || len(f.calls) != 2 || f.calls[0].Tool != "beads_label" {
		t.Errorf("err=%v calls=%+v: the label goes first, and a refused release is an error", err, f.calls)
	}
}

func TestRunAddsTheWaitAfterTheCreate(t *testing.T) {
	f := &fakeVarde{answers: map[string]json.RawMessage{"beads_create": json.RawMessage(`{"ok":true,"id":"B-9"}`)}}
	if err := Run(context.Background(), f, []string{"create", "--title", "x", "--deps", "B-7", "--json"}, "Forge", "EP-1", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[1].Tool != "beads_dep" || f.calls[1].Args["id"] != "B-9" || f.calls[1].Args["on"] != "B-7" {
		t.Errorf("calls = %+v", f.calls)
	}
}
