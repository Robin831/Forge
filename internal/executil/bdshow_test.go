package executil

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The argument vector is the whole point of the helper: bd omits the dependents
// array unless asked, so a call site that loses the flag reads every bead as
// childless without erroring. Pinning the shape here is what keeps every id
// named through --id (bd takes several) and the flags trailing.
func TestBdShowDependentsArgs(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want []string
	}{
		{
			name: "single id",
			ids:  []string{"Forge-abc1"},
			want: []string{"show", "--id=Forge-abc1", "--json", "--include-dependents"},
		},
		{
			name: "several ids",
			ids:  []string{"a", "b", "c"},
			want: []string{"show", "--id=a", "--id=b", "--id=c", "--json", "--include-dependents"},
		},
		{
			name: "no ids",
			ids:  nil,
			want: []string{"show", "--json", "--include-dependents"},
		},
		{
			// A bead id is a value Forge did not write. Passed positionally, one
			// shaped like a flag would be parsed as one — silently changing the
			// command, or drawing an "unknown flag" rejection
			// ClassifyBdShowError would then report as an old bd. --id= carries
			// it verbatim instead, so it is neither dropped nor obeyed.
			name: "flag-shaped id is named, not parsed",
			ids:  []string{"--include-dependents=x", "-f"},
			want: []string{"show", "--id=--include-dependents=x", "--id=-f", "--json", "--include-dependents"},
		},
		{
			// An empty id is not a bead; sending `--id=` would be a request bd
			// can only reject.
			name: "empty id is dropped",
			ids:  []string{"", "Forge-abc1", ""},
			want: []string{"show", "--id=Forge-abc1", "--json", "--include-dependents"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BdShowDependentsArgs(tt.ids...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BdShowDependentsArgs(%v) = %v, want %v", tt.ids, got, tt.want)
			}
		})
	}
}

// The whole compatibility story rests on recognising the rejection: a bd that
// does not know the flag must produce a loud, named failure rather than being
// retried unflagged (which would hand back the empty array the flag exists to
// fix). These are the wordings an argument parser actually emits.
func TestIsUnsupportedIncludeDependentsOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "cobra rejection",
			output: "Error: unknown flag: --include-dependents\nUsage:\n  bd show [id...]\n",
			want:   true,
		},
		{
			name:   "stdlib flag rejection",
			output: "flag provided but not defined: -include-dependents",
			want:   true,
		},
		{
			name:   "case insensitive",
			output: "ERROR: UNKNOWN FLAG: --INCLUDE-DEPENDENTS",
			want:   true,
		},
		{
			name:   "unrelated failure naming the flag is not a rejection",
			output: "bd show --include-dependents: database is locked",
			want:   false,
		},
		{
			name:   "unknown flag that is not ours",
			output: "Error: unknown flag: --include-comments",
			want:   false,
		},
		{
			name:   "empty",
			output: "",
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUnsupportedIncludeDependentsOutput(tt.output); got != tt.want {
				t.Errorf("IsUnsupportedIncludeDependentsOutput(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestClassifyBdShowError(t *testing.T) {
	t.Run("nil error stays nil", func(t *testing.T) {
		if err := ClassifyBdShowError(nil, "Error: unknown flag: --include-dependents"); err != nil {
			t.Errorf("ClassifyBdShowError(nil, ...) = %v, want nil", err)
		}
	})

	t.Run("flag rejection is wrapped as the sentinel", func(t *testing.T) {
		cause := errors.New("exit status 1")
		err := ClassifyBdShowError(cause, "Error: unknown flag: --include-dependents")
		if !errors.Is(err, ErrIncludeDependentsUnsupported) {
			t.Fatalf("error %v does not wrap ErrIncludeDependentsUnsupported", err)
		}
		// The original cause survives in the message so an operator still sees
		// what bd actually did.
		if got := err.Error(); !strings.Contains(got, "exit status 1") {
			t.Errorf("error %q does not carry the underlying cause", got)
		}
	})

	t.Run("any other failure is passed through untouched", func(t *testing.T) {
		cause := errors.New("exit status 1")
		err := ClassifyBdShowError(cause, "Error: issue not found: Forge-zzzz")
		if errors.Is(err, ErrIncludeDependentsUnsupported) {
			t.Error("an unrelated bd failure was misreported as an unsupported flag")
		}
		if !errors.Is(err, cause) {
			t.Errorf("error = %v, want the original cause", err)
		}
	})
}
