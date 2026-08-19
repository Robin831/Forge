package poller

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/executil"
)

// withFlagAwareBd puts a `bd` on PATH that answers `bd show` the way bd 1.1.2
// actually does: the `dependents` array appears ONLY when
// --include-dependents is passed, and an unflagged show reports the bare
// `dependent_count` instead.
//
// That discrimination is the whole point of these tests. A call site that loses
// the flag does not fail here — it quietly decodes an empty dependents array
// and reports the parent as childless, which is the shape of the bug this
// fixture is here to catch. Labels are answered on both forms, since the
// second lookup that reads them (resolveIndependent) needs no flag.
func withFlagAwareBd(t *testing.T) {
	t.Helper()
	withFakeBd(t, `flag=""
ids=""
for a in "$@"; do
  case "$a" in
    --include-dependents) flag=1 ;;
    show|--json|-*) ;;
    *) ids="$ids $a" ;;
  esac
done
out=""
for id in $ids; do
  rec=""
  case "$id" in
    parent-1)
      if [ -n "$flag" ]; then
        rec='{"id":"parent-1","status":"open","labels":["crucible"],"dependent_count":2,"dependents":[{"id":"child-1","dependency_type":"blocks","status":"open"},{"id":"child-2","dependency_type":"parent-child","status":"closed"}]}'
      else
        rec='{"id":"parent-1","status":"open","labels":["crucible"],"dependent_count":2}'
      fi
      ;;
    child-1) rec='{"id":"child-1","status":"open","labels":["forgeReady"]}' ;;
    child-2) rec='{"id":"child-2","status":"closed","labels":[]}' ;;
    childless-1)
      if [ -n "$flag" ]; then
        rec='{"id":"childless-1","status":"open","dependent_count":0,"dependents":[]}'
      else
        rec='{"id":"childless-1","status":"open","dependent_count":0}'
      fi
      ;;
  esac
  [ -z "$rec" ] && continue
  if [ -z "$out" ]; then out="$rec"; else out="$out,$rec"; fi
done
printf '[%s]\n' "$out"`)
}

// withFlagRejectingBd puts a `bd` on PATH that predates --include-dependents
// and rejects it the way cobra does. This is the "older bd" half of the
// compatibility story: the behaviour must be a named failure, not an empty
// child list that reads as a finished epic.
func withFlagRejectingBd(t *testing.T) {
	t.Helper()
	withFakeBd(t, `for a in "$@"; do
  case "$a" in
    --include-dependents)
      echo "Error: unknown flag: --include-dependents" >&2
      exit 1
      ;;
  esac
done
echo '[{"id":"parent-1","status":"open","dependent_count":2}]'`)
}

// The acceptance criterion: a parent with open children comes back with a
// non-empty Blocks. It only holds if lookupBlocks asks for the dependents
// array — the fixture answers an unflagged show with a dependent_count and
// nothing else, exactly as bd does.
func TestResolveBlocks_AsksBdForTheDependentsArray(t *testing.T) {
	withFlagAwareBd(t)

	beads := []Bead{{ID: "parent-1", Anvil: "a"}}
	ResolveBlocks(context.Background(), beads, map[string]string{"a": t.TempDir()})

	want := []string{"child-1"}
	if len(beads[0].Blocks) != len(want) || beads[0].Blocks[0] != want[0] {
		t.Errorf("Blocks = %v, want %v (child-2 is closed)", beads[0].Blocks, want)
	}
}

// A bead with no children still resolves to nothing — the flag does not invent
// a child, so the childless case stays the ordinary pipeline's.
func TestResolveBlocks_ChildlessBeadStaysEmpty(t *testing.T) {
	withFlagAwareBd(t)

	beads := []Bead{{ID: "childless-1", Anvil: "a"}}
	ResolveBlocks(context.Background(), beads, map[string]string{"a": t.TempDir()})

	if len(beads[0].Blocks) != 0 {
		t.Errorf("Blocks = %v, want empty", beads[0].Blocks)
	}
}

// The wider question OpenChildren answers has the same dependency on the flag,
// and a wrong answer here is what decides between dispatching an opted-in
// parent and holding it.
func TestOpenChildren_AsksBdForTheDependentsArray(t *testing.T) {
	withFlagAwareBd(t)

	open, err := OpenChildren(context.Background(), "parent-1", t.TempDir())
	if err != nil {
		t.Fatalf("OpenChildren: %v", err)
	}
	if len(open) != 1 || open[0] != "child-1" {
		t.Errorf("open children = %v, want [child-1] (child-2 is closed)", open)
	}
}

// An older bd is a refusal OpenChildren reports, never an empty slice: its
// caller reads "no children" as permission to merge the parent to main, so the
// two must not be spelled the same way. The sentinel is what names the fix.
func TestOpenChildren_OlderBdIsANamedError(t *testing.T) {
	withFlagRejectingBd(t)

	open, err := OpenChildren(context.Background(), "parent-1", t.TempDir())
	if err == nil {
		t.Fatalf("OpenChildren succeeded with open = %v; want an error on a bd without the flag", open)
	}
	if !errors.Is(err, executil.ErrIncludeDependentsUnsupported) {
		t.Errorf("error %v does not wrap ErrIncludeDependentsUnsupported", err)
	}
	if open != nil {
		t.Errorf("open children = %v, want nil alongside the error", open)
	}
}

// lookupBlocks has no error channel — it returns nil, which every orchestration
// gate reads as "no children to orchestrate". So the decided behaviour on an
// older bd is: no children inferred (nothing is dispatched on a guess) AND a
// log line that names the flag, since that nil is otherwise indistinguishable
// from an epic that is genuinely empty.
func TestResolveBlocks_OlderBdLogsTheUnsupportedFlag(t *testing.T) {
	withFlagRejectingBd(t)

	var logged bytes.Buffer
	restore := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(restore) })

	beads := []Bead{{ID: "parent-1", Anvil: "a"}}
	ResolveBlocks(context.Background(), beads, map[string]string{"a": t.TempDir()})

	if len(beads[0].Blocks) != 0 {
		t.Errorf("Blocks = %v, want empty — an unreadable answer must not be read as children", beads[0].Blocks)
	}
	if !strings.Contains(logged.String(), executil.BdIncludeDependentsFlag) {
		t.Errorf("log %q does not name %s", logged.String(), executil.BdIncludeDependentsFlag)
	}
}
