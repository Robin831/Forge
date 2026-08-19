package crucible

import (
	"context"
	"errors"
	"testing"

	"github.com/Robin831/Forge/internal/executil"
)

// withFlagAwareBd puts a `bd` on PATH that answers `bd show` the way bd 1.1.2
// does: the `dependents` array is emitted ONLY when --include-dependents is
// present, and an unflagged show reports a bare `dependent_count`.
//
// It is what makes these tests regressions rather than restatements — a
// FetchBead that drops the flag does not fail against this fixture, it walks an
// empty tree and reports a childless epic, which is precisely the bug.
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
  deps=""
  case "$id" in
    parent-1) deps='{"id":"child-1","dependency_type":"blocks"},{"id":"child-2","dependency_type":"parent-child"}' ;;
    child-1)  deps='{"id":"grandchild-1","dependency_type":"blocks"}' ;;
  esac
  case "$id" in
    parent-1|child-1|child-2|grandchild-1) ;;
    *) continue ;;
  esac
  if [ -n "$flag" ]; then
    rec='{"id":"'"$id"'","status":"open","dependents":['"$deps"']}'
  else
    rec='{"id":"'"$id"'","status":"open","dependent_count":2}'
  fi
  if [ -z "$out" ]; then out="$rec"; else out="$out,$rec"; fi
done
printf '[%s]\n' "$out"`)
}

// The acceptance criterion for the Crucible half: FetchChildren walks a real
// two-level descendant tree. Without the flag the parent's dependents array is
// absent and the walk stops before it starts.
func TestFetchChildren_WalksTheTreeBdOnlyEmitsWhenAsked(t *testing.T) {
	withFlagAwareBd(t)

	children, err := FetchChildren(context.Background(), "parent-1", t.TempDir())
	if err != nil {
		t.Fatalf("FetchChildren: %v", err)
	}

	got := beadIDs(children)
	want := map[string]bool{"child-1": true, "child-2": true, "grandchild-1": true}
	if len(got) != len(want) {
		t.Fatalf("children = %v, want the three descendants %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected descendant %q in %v", id, got)
		}
	}
}

// FetchBead reports both dependency types as children, which is what the
// topological sort then orders.
func TestFetchBead_ExtractsBlocksFromTheDependentsArray(t *testing.T) {
	withFlagAwareBd(t)

	bead, err := FetchBead(context.Background(), "parent-1", t.TempDir())
	if err != nil {
		t.Fatalf("FetchBead: %v", err)
	}
	if len(bead.Blocks) != 2 || bead.Blocks[0] != "child-1" || bead.Blocks[1] != "child-2" {
		t.Errorf("Blocks = %v, want [child-1 child-2]", bead.Blocks)
	}
}

// The older-bd behaviour is decided, not accidental: a bd that rejects the flag
// makes FetchBead fail with the sentinel naming the upgrade, rather than
// succeeding with a parent that appears to have no children — which the Crucible
// would take as an epic ready for its final PR.
func TestFetchBead_OlderBdIsANamedError(t *testing.T) {
	withFakeBd(t, `for a in "$@"; do
  case "$a" in
    --include-dependents)
      echo "Error: unknown flag: --include-dependents" >&2
      exit 1
      ;;
  esac
done
echo '[{"id":"parent-1","status":"open","dependent_count":2}]'`)

	bead, err := FetchBead(context.Background(), "parent-1", t.TempDir())
	if err == nil {
		t.Fatalf("FetchBead succeeded with %+v; want an error on a bd without the flag", bead)
	}
	if !errors.Is(err, executil.ErrIncludeDependentsUnsupported) {
		t.Errorf("error %v does not wrap ErrIncludeDependentsUnsupported", err)
	}
}
