package hearth

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestRenderWicketPanel_Empty verifies the panel renders without rows gracefully.
func TestRenderWicketPanel_Empty(t *testing.T) {
	m := NewModel(nil)
	m.wicketSummary = []WicketRepoSummary{}
	got := m.renderWicketPanel(30, 3)
	if got == "" {
		t.Fatal("expected non-empty render even with no items")
	}
	if !strings.Contains(got, "Wicket") {
		t.Errorf("expected panel title 'Wicket' in output, got: %q", got)
	}
}

// TestRenderWicketPanel_SingleRepo verifies a single repo row is rendered.
func TestRenderWicketPanel_SingleRepo(t *testing.T) {
	m := NewModel(nil)
	m.wicketSummary = []WicketRepoSummary{
		{Repo: "org/forge", OpenCount: 3, NeedsHumanCount: 0},
	}
	got := m.renderWicketPanel(40, 4)
	if !strings.Contains(got, "forge") {
		t.Errorf("expected repo short name 'forge' in output, got: %q", got)
	}
	if !strings.Contains(got, "3 open") {
		t.Errorf("expected '3 open' in output, got: %q", got)
	}
	if !strings.Contains(got, "0⚠") {
		t.Errorf("expected '0⚠' in output, got: %q", got)
	}
}

// TestRenderWicketPanel_NeedsHumanHighlighted verifies the ⚠ count is rendered
// when NeedsHumanCount > 0.
func TestRenderWicketPanel_NeedsHumanHighlighted(t *testing.T) {
	m := NewModel(nil)
	m.wicketSummary = []WicketRepoSummary{
		{Repo: "org/heimdall", OpenCount: 5, NeedsHumanCount: 2},
	}
	got := m.renderWicketPanel(40, 4)
	if !strings.Contains(got, "2⚠") {
		t.Errorf("expected '2⚠' in output, got: %q", got)
	}
}

// TestRenderWicketPanel_MultipleRepos verifies multiple repos render correctly
// and are capped at 4.
func TestRenderWicketPanel_MultipleRepos(t *testing.T) {
	m := NewModel(nil)
	m.wicketSummary = []WicketRepoSummary{
		{Repo: "org/alpha", OpenCount: 1, NeedsHumanCount: 0},
		{Repo: "org/beta", OpenCount: 2, NeedsHumanCount: 1},
		{Repo: "org/gamma", OpenCount: 3, NeedsHumanCount: 0},
		{Repo: "org/delta", OpenCount: 4, NeedsHumanCount: 2},
		{Repo: "org/epsilon", OpenCount: 5, NeedsHumanCount: 0},
	}
	got := m.renderWicketPanel(50, 7)
	// Should show alpha through delta but not epsilon (capped at 4).
	for _, name := range []string{"alpha", "beta", "gamma", "delta"} {
		if !strings.Contains(got, name) {
			t.Errorf("expected repo %q in output, got: %q", name, got)
		}
	}
	if strings.Contains(got, "epsilon") {
		t.Errorf("expected epsilon to be hidden (capped at 4), got: %q", got)
	}
}

// TestRenderWicketPanel_RepoDisplayName verifies that "owner/repo" is displayed
// as just "repo".
func TestRenderWicketPanel_RepoDisplayName(t *testing.T) {
	m := NewModel(nil)
	m.wicketSummary = []WicketRepoSummary{
		{Repo: "myorg/myrepo", OpenCount: 1, NeedsHumanCount: 0},
	}
	got := m.renderWicketPanel(40, 4)
	if strings.Contains(got, "myorg") {
		t.Errorf("expected owner 'myorg' to be stripped from display, got: %q", got)
	}
	if !strings.Contains(got, "myrepo") {
		t.Errorf("expected repo name 'myrepo' in display, got: %q", got)
	}
}

// TestRenderCenterColumn_WicketPanelInserted verifies that when wicket is enabled
// the center column contains the Wicket panel between Workers and Usage.
func TestRenderCenterColumn_WicketPanelInserted(t *testing.T) {
	ds := &DataSource{WicketEnabled: true}
	m := NewModel(ds)
	m.ready = true
	m.width = 80
	m.height = 40
	m.wicketSummary = []WicketRepoSummary{
		{Repo: "org/forge", OpenCount: 2, NeedsHumanCount: 0},
	}

	got := m.renderCenterColumn(30, 15, 10)
	if !strings.Contains(got, "Wicket") {
		t.Errorf("expected Wicket panel when WicketEnabled=true and data present, got: %q", got)
	}
	if !strings.Contains(got, "Workers") {
		t.Errorf("expected Workers panel in output, got: %q", got)
	}
	if !strings.Contains(got, "Usage") {
		t.Errorf("expected Usage panel in output, got: %q", got)
	}
}

// TestRenderCenterColumn_WicketHiddenWhenDisabled verifies no Wicket panel when
// WicketEnabled is false.
func TestRenderCenterColumn_WicketHiddenWhenDisabled(t *testing.T) {
	ds := &DataSource{WicketEnabled: false}
	m := NewModel(ds)
	m.ready = true
	m.width = 80
	m.height = 40
	m.wicketSummary = []WicketRepoSummary{
		{Repo: "org/forge", OpenCount: 2, NeedsHumanCount: 0},
	}

	got := m.renderCenterColumn(30, 15, 10)
	if strings.Contains(got, "Wicket") {
		t.Errorf("expected no Wicket panel when WicketEnabled=false, got: %q", got)
	}
}

// TestRenderCenterColumn_WicketHiddenWhenNoData verifies no Wicket panel when
// WicketEnabled is true but there are no repos with open issues.
func TestRenderCenterColumn_WicketHiddenWhenNoData(t *testing.T) {
	ds := &DataSource{WicketEnabled: true}
	m := NewModel(ds)
	m.ready = true
	m.width = 80
	m.height = 40
	m.wicketSummary = nil

	got := m.renderCenterColumn(30, 15, 10)
	if strings.Contains(got, "Wicket") {
		t.Errorf("expected no Wicket panel when wicketSummary is empty, got: %q", got)
	}
}

// TestTabSkipsWicketWhenNotVisible verifies that Tab skips PanelWicket when
// wicketVisible() returns false (disabled or no data).
func TestTabSkipsWicketWhenNotVisible(t *testing.T) {
	// data=nil → WicketEnabled=false → wicketVisible()=false
	m := NewModel(nil)
	m.focused = PanelWorkers

	mTmp, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	got := mTmp.(*Model).focused
	if got == PanelWicket {
		t.Errorf("Tab from PanelWorkers should skip PanelWicket when wicketVisible()=false, got PanelWicket")
	}
	if got != PanelUsage {
		t.Errorf("Tab from PanelWorkers should land on PanelUsage when PanelWicket is skipped, got %v", got)
	}
}

// TestShiftTabSkipsWicketWhenNotVisible verifies that Shift+Tab skips PanelWicket
// when wicketVisible() returns false.
func TestShiftTabSkipsWicketWhenNotVisible(t *testing.T) {
	// data=nil → WicketEnabled=false → wicketVisible()=false
	m := NewModel(nil)
	m.focused = PanelUsage

	mTmp, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	got := mTmp.(*Model).focused
	if got == PanelWicket {
		t.Errorf("Shift+Tab from PanelUsage should skip PanelWicket when wicketVisible()=false, got PanelWicket")
	}
	if got != PanelWorkers {
		t.Errorf("Shift+Tab from PanelUsage should land on PanelWorkers when PanelWicket is skipped, got %v", got)
	}
}

// TestTabStopsAtWicketWhenVisible verifies that Tab lands on PanelWicket when
// wicketVisible() returns true.
func TestTabStopsAtWicketWhenVisible(t *testing.T) {
	ds := &DataSource{WicketEnabled: true}
	m := NewModel(ds)
	m.wicketSummary = []WicketRepoSummary{{Repo: "org/forge", OpenCount: 1}}
	m.focused = PanelWorkers

	mTmp, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	got := mTmp.(*Model).focused
	if got != PanelWicket {
		t.Errorf("Tab from PanelWorkers should land on PanelWicket when visible, got %v", got)
	}
}

// TestPanelAtPosCenterColumn_WithWicket_ReturnsWicket verifies that panelAtPos
// returns PanelWicket in the center column when the Wicket panel is visible,
// positioned between Workers and Usage.
func TestPanelAtPosCenterColumn_WithWicket_ReturnsWicket(t *testing.T) {
	m := newTestModelForPanelAtPos()
	ds := &DataSource{WicketEnabled: true}
	m.data = ds
	m.wicketSummary = []WicketRepoSummary{
		{Repo: "org/forge", OpenCount: 2, NeedsHumanCount: 0},
	}
	leftEnd, centerEnd, _, headerH := panelBoundaries(m)
	x := leftEnd + 5
	if x > centerEnd {
		x = (leftEnd + centerEnd) / 2
	}

	var workersY, wicketY, usageY int = -1, -1, -1
	for y := headerH + 1; y < m.height-1; y++ {
		panel := m.panelAtPos(x, y)
		if panel == PanelWorkers && workersY == -1 {
			workersY = y
		}
		if panel == PanelWicket && wicketY == -1 {
			wicketY = y
		}
		if panel == PanelUsage && usageY == -1 {
			usageY = y
		}
	}
	if workersY == -1 {
		t.Fatalf("expected PanelWorkers in center column, not found")
	}
	if wicketY == -1 {
		t.Fatalf("expected PanelWicket in center column when wicketVisible()=true, not found")
	}
	if usageY == -1 {
		t.Fatalf("expected PanelUsage in center column, not found")
	}
	if !(workersY < wicketY) {
		t.Errorf("expected PanelWorkers above PanelWicket, got workersY=%d, wicketY=%d", workersY, wicketY)
	}
	if !(wicketY < usageY) {
		t.Errorf("expected PanelWicket above PanelUsage, got wicketY=%d, usageY=%d", wicketY, usageY)
	}
}
