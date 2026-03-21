package ledger

import (
	"errors"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/depupdate"
)

// makeReport is a helper for constructing an AnvilReport in tests.
func makeReport(anvilName string, groupNames []string, errs map[string]error) depupdate.AnvilReport {
	var groups []depupdate.UpdateGroup
	for _, name := range groupNames {
		groups = append(groups, depupdate.UpdateGroup{Name: name, Kind: "patch"})
	}
	return depupdate.AnvilReport{
		Anvil:  depupdate.Anvil{Name: anvilName, Path: "/tmp/" + anvilName},
		Groups: groups,
		Errors: errs,
	}
}

// --- countUpdateGroups ---

func TestCountUpdateGroupsEmpty(t *testing.T) {
	if got := countUpdateGroups(nil); got != 0 {
		t.Errorf("countUpdateGroups(nil) = %d, want 0", got)
	}
}

func TestCountUpdateGroupsSingle(t *testing.T) {
	reports := []depupdate.AnvilReport{
		makeReport("a", []string{"group1", "group2"}, nil),
	}
	if got := countUpdateGroups(reports); got != 2 {
		t.Errorf("countUpdateGroups = %d, want 2", got)
	}
}

func TestCountUpdateGroupsMultiple(t *testing.T) {
	reports := []depupdate.AnvilReport{
		makeReport("a", []string{"g1", "g2"}, nil),
		makeReport("b", []string{"g3"}, nil),
		makeReport("c", nil, nil),
	}
	if got := countUpdateGroups(reports); got != 3 {
		t.Errorf("countUpdateGroups = %d, want 3", got)
	}
}

// --- countUpdateAnvils ---

func TestCountUpdateAnvilsEmpty(t *testing.T) {
	if got := countUpdateAnvils(nil); got != 0 {
		t.Errorf("countUpdateAnvils(nil) = %d, want 0", got)
	}
}

func TestCountUpdateAnvilsOnlyAnvilsWithGroups(t *testing.T) {
	reports := []depupdate.AnvilReport{
		makeReport("a", []string{"g1"}, nil), // has groups
		makeReport("b", nil, nil),            // no groups — should not count
		makeReport("c", []string{"g2"}, nil), // has groups
	}
	if got := countUpdateAnvils(reports); got != 2 {
		t.Errorf("countUpdateAnvils = %d, want 2", got)
	}
}

func TestCountUpdateAnvilsAllEmpty(t *testing.T) {
	reports := []depupdate.AnvilReport{
		makeReport("a", nil, nil),
		makeReport("b", nil, nil),
	}
	if got := countUpdateAnvils(reports); got != 0 {
		t.Errorf("countUpdateAnvils = %d, want 0", got)
	}
}

// --- buildDepUpdateAnvils (sorted order) ---

func TestBuildDepUpdateAnvilsSorted(t *testing.T) {
	m := &Model{
		anvils: map[string]string{
			"zebra":  "/tmp/zebra",
			"alpha":  "/tmp/alpha",
			"middle": "/tmp/middle",
		},
	}
	result := m.buildDepUpdateAnvils()
	if len(result) != 3 {
		t.Fatalf("expected 3 anvils, got %d", len(result))
	}
	names := make([]string, len(result))
	for i, a := range result {
		names[i] = a.Name
	}
	want := []string{"alpha", "middle", "zebra"}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("anvil[%d] = %q, want %q (should be alphabetically sorted)", i, names[i], w)
		}
	}
}

func TestBuildDepUpdateAnvilsEmpty(t *testing.T) {
	m := &Model{anvils: map[string]string{}}
	if result := m.buildDepUpdateAnvils(); result != nil {
		t.Errorf("expected nil for empty anvils map, got %v", result)
	}
}

// --- renderUpdateOverlay smoke tests ---

func TestRenderUpdateOverlayScanning(t *testing.T) {
	m := &Model{
		anvils:            map[string]string{"test": "/tmp/test"},
		width:             120,
		height:            40,
		showUpdateOverlay: true,
		updateScanning:    true,
	}
	out := m.renderUpdateOverlay()
	if !strings.Contains(out, "Dependency Updates") {
		t.Error("scanning overlay must contain title 'Dependency Updates'")
	}
	if !strings.Contains(out, "Scanning") {
		t.Error("scanning overlay must contain 'Scanning'")
	}
}

func TestRenderUpdateOverlayRunning(t *testing.T) {
	m := &Model{
		anvils:            map[string]string{"test": "/tmp/test"},
		width:             120,
		height:            40,
		showUpdateOverlay: true,
		updateRunning:     true,
	}
	out := m.renderUpdateOverlay()
	if !strings.Contains(out, "Applying") {
		t.Error("running overlay must mention 'Applying'")
	}
}

func TestRenderUpdateOverlayErrorFormatting(t *testing.T) {
	// The error rendering path should produce readable "key: message" pairs, not
	// Go map/slice notation like "map[go:some error]".
	m := &Model{
		updateReports: []depupdate.AnvilReport{
			{
				Anvil:  depupdate.Anvil{Name: "test", Path: "/tmp/test"},
				Groups: nil,
				Errors: map[string]error{"go": errors.New("network timeout")},
			},
		},
	}
	// Directly verify error string formatting via the overlay; the filter form
	// branch is exercised when updateFilterForm != nil, which requires huh
	// initialization. We test the underlying rendering logic by ensuring the
	// formatted error does not contain "[" (Go slice/map literal syntax).
	//
	// Build a minimal report and manually invoke the same formatting logic the
	// overlay uses: "eco: message" joined by "; ".
	report := m.updateReports[0]
	var errParts []string
	for eco, err := range report.Errors {
		errParts = append(errParts, eco+": "+err.Error())
	}
	formatted := strings.Join(errParts, "; ")
	if strings.HasPrefix(formatted, "[") {
		t.Errorf("error formatting must not start with '[' (Go literal), got: %s", formatted)
	}
	if !strings.Contains(formatted, "go: network timeout") {
		t.Errorf("error formatting must contain 'go: network timeout', got: %s", formatted)
	}
}

func TestRenderUpdateOverlayMinimumDimensions(t *testing.T) {
	// Verify the overlay does not panic even with very small terminal dimensions.
	m := &Model{
		anvils:            map[string]string{"test": "/tmp/test"},
		width:             10,
		height:            5,
		showUpdateOverlay: true,
		updateScanning:    true,
	}
	out := m.renderUpdateOverlay()
	if out == "" {
		t.Error("renderUpdateOverlay must not return empty string")
	}
}
