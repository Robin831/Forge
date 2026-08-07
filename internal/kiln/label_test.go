package kiln

import (
	"errors"
	"strings"
	"testing"
)

func TestPreviewLabel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Forge-ir70", "forge-ir70"},
		{"wi_12", "wi-12"},
		{"forge/Forge-ir70", "forge-forge-ir70"},
		{"a--b__c", "a-b-c"},
		{"already-hyphenfree", "already-hyphenfree"},
		{"trailing-", "trailing"},
		{"-leading", "leading"},
		{"123", "p-123"},
		{"  spaced id  ", "spaced-id"},
		{"", "preview"},
		{"---", "preview"},
	}
	for _, tc := range tests {
		if got := PreviewLabel(tc.in); got != tc.want {
			t.Errorf("PreviewLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPreviewLabelIsDNSSafe pins the property the proxy depends on: whatever a
// bead id looks like, the label is a usable hostname component — which is also
// what keeps "--" available as the service separator.
func TestPreviewLabelIsDNSSafe(t *testing.T) {
	for _, in := range []string{"Forge/ir-70", "feature/ABC.1", "42", "ünïcode", "", "___"} {
		got := PreviewLabel(in)
		if !isDNSLabel(got) {
			t.Errorf("PreviewLabel(%q) = %q, which is not a valid DNS label", in, got)
		}
		if strings.Contains(got, "--") {
			t.Errorf("PreviewLabel(%q) = %q contains the service separator %q", in, got, "--")
		}
	}
}

func TestCheckPreviewLabelCollisions(t *testing.T) {
	t.Run("disjoint ids are fine", func(t *testing.T) {
		if err := CheckPreviewLabelCollisions([]string{"Forge-aaa1", "Forge-bbb2", "Hytte-9epl"}); err != nil {
			t.Fatalf("unexpected collision: %v", err)
		}
	})

	t.Run("the same id twice is not a collision", func(t *testing.T) {
		if err := CheckPreviewLabelCollisions([]string{"wi-12", "wi-12"}); err != nil {
			t.Fatalf("unexpected collision: %v", err)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if err := CheckPreviewLabelCollisions(nil); err != nil {
			t.Fatalf("unexpected collision: %v", err)
		}
	})

	t.Run("folded ids collide", func(t *testing.T) {
		err := CheckPreviewLabelCollisions([]string{"wi_12", "wi-12"})
		if err == nil {
			t.Fatal("expected a collision for wi_12 / wi-12")
		}
		if !errors.Is(err, ErrPreviewLabelCollision) {
			t.Errorf("error does not wrap ErrPreviewLabelCollision: %v", err)
		}
		var collision *PreviewLabelCollisionError
		if !errors.As(err, &collision) {
			t.Fatalf("error is not a *PreviewLabelCollisionError: %v", err)
		}
		if collision.Label != "wi-12" {
			t.Errorf("Label = %q, want %q", collision.Label, "wi-12")
		}
		// Sorted, so the message is the same on every run.
		if got := strings.Join(collision.BeadIDs, ","); got != "wi-12,wi_12" {
			t.Errorf("BeadIDs = %v, want [wi-12 wi_12]", collision.BeadIDs)
		}
		for _, want := range []string{`"wi-12"`, `"wi_12"`, "preview_proxy_base"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %s", err, want)
			}
		}
	})

	t.Run("deterministic across orderings", func(t *testing.T) {
		a := CheckPreviewLabelCollisions([]string{"z_1", "z-1", "a_2", "a-2"})
		b := CheckPreviewLabelCollisions([]string{"a-2", "z-1", "z_1", "a_2"})
		if a == nil || b == nil {
			t.Fatalf("expected collisions, got %v / %v", a, b)
		}
		if a.Error() != b.Error() {
			t.Errorf("input order changed the message:\n  %v\n  %v", a, b)
		}
	})
}

func TestParsePreviewHost(t *testing.T) {
	const base = "preview.example.test"

	tests := []struct {
		name       string
		host, base string
		label, svc string
		ok         bool
	}{
		{name: "bare label", host: "wi-12." + base, base: base, label: "wi-12", ok: true},
		{name: "label and service", host: "wi-12--api." + base, base: base, label: "wi-12", svc: "api", ok: true},
		{name: "single label base", host: "wi-12.localtest", base: "localtest", label: "wi-12", ok: true},
		{
			name: "case insensitive with a port suffix", host: "WI-12.PREVIEW.EXAMPLE.TEST:8080",
			base: base, label: "wi-12", ok: true,
		},
		{name: "trailing root dot", host: "wi-12." + base + ".", base: base, label: "wi-12", ok: true},
		{name: "base configured with a trailing dot", host: "wi-12." + base, base: base + ".", label: "wi-12", ok: true},
		{name: "padded host", host: "  wi-12." + base + "  ", base: base, label: "wi-12", ok: true},

		{name: "apex is not a preview", host: base, base: base},
		{name: "apex with a port", host: base + ":8080", base: base},
		{name: "extra labels", host: "a.wi-12." + base, base: base},
		{name: "empty base disables matching", host: "wi-12." + base, base: ""},
		{name: "empty host", host: "", base: base},
		{name: "different base", host: "wi-12.other.test", base: base},
		{name: "suffix without the dot boundary", host: "wi-12x" + base, base: base},
		{name: "empty label before the separator", host: "--api." + base, base: base},
		{name: "empty service after the separator", host: "wi-12--." + base, base: base},
		{name: "hyphen-opened service", host: "wi-12---api." + base, base: base},
		{name: "two separators", host: "wi-12--api--v2." + base, base: base},
		{name: "underscore is not DNS-legal", host: "wi_12." + base, base: base},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			label, svc, ok := ParsePreviewHost(tc.host, tc.base)
			if ok != tc.ok {
				t.Fatalf("ParsePreviewHost(%q, %q) ok = %v, want %v", tc.host, tc.base, ok, tc.ok)
			}
			if !tc.ok {
				if label != "" || svc != "" {
					t.Errorf("a rejected host returned label=%q service=%q, want both empty", label, svc)
				}
				return
			}
			if label != tc.label || svc != tc.svc {
				t.Errorf("ParsePreviewHost(%q, %q) = (%q, %q), want (%q, %q)",
					tc.host, tc.base, label, svc, tc.label, tc.svc)
			}
		})
	}
}

// TestParsePreviewHostRoundTripsPreviewLabel ties the two halves together: what
// PreviewLabel puts in front of the base is what ParsePreviewHost gets back.
func TestParsePreviewHostRoundTripsPreviewLabel(t *testing.T) {
	const base = "preview.example.test"
	for _, beadID := range []string{"Forge-ir70", "wi_12", "forge/Forge-ir70", "123"} {
		want := PreviewLabel(beadID)
		label, svc, ok := ParsePreviewHost(want+"."+base, base)
		if !ok || label != want || svc != "" {
			t.Errorf("round trip for %q: got (%q, %q, %v), want (%q, \"\", true)",
				beadID, label, svc, ok, want)
		}
		label, svc, ok = ParsePreviewHost(want+"--api."+base, base)
		if !ok || label != want || svc != "api" {
			t.Errorf("service round trip for %q: got (%q, %q, %v), want (%q, \"api\", true)",
				beadID, label, svc, ok, want)
		}
	}
}
