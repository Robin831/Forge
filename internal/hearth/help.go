package hearth

import (
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
)

// --- Key binding definitions ---

var (
	keyTab = key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("Tab", "next panel"),
	)
	keyShiftTab = key.NewBinding(
		key.WithKeys("shift+tab"),
		key.WithHelp("Shift+Tab", "prev panel"),
	)
	keyScroll = key.NewBinding(
		key.WithKeys("j", "k", "down", "up"),
		key.WithHelp("j/k", "scroll"),
	)
	keyEnter = key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "action"),
	)
	keyMerge = key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "merge"),
	)
	keyKill = key.NewBinding(
		key.WithKeys("K"),
		key.WithHelp("K", "kill worker"),
	)
	keyViewLog = key.NewBinding(
		key.WithKeys("o"),
		key.WithHelp("o", "view log"),
	)
	keyLabel = key.NewBinding(
		key.WithKeys("l"),
		key.WithHelp("l", "label for dispatch"),
	)
	keyFilter = key.NewBinding(
		key.WithKeys("/"),
		key.WithHelp("/", "filter"),
	)
	keyFollow = key.NewBinding(
		key.WithKeys("f"),
		key.WithHelp("f", "follow latest"),
	)
	keyExpand = key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "expand/collapse"),
	)
	keyCollapse = key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "collapse"),
	)
	keyDesc = key.NewBinding(
		key.WithKeys("d"),
		key.WithHelp("d", "description"),
	)
	keyNotes = key.NewBinding(
		key.WithKeys("n"),
		key.WithHelp("n", "notes"),
	)
	keyMouse = key.NewBinding(
		key.WithKeys("m"),
		key.WithHelp("m", "toggle mouse"),
	)
	keyQuit = key.NewBinding(
		key.WithKeys("q", "ctrl+c"),
		key.WithHelp("q", "quit"),
	)
)

// --- Per-panel KeyMap types ---

type queueKeyMap struct{ m *Model }

func (k queueKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyFilter, keyEnter, keyCollapse, keyDesc, keyNotes, keyLabel, keyTab, k.m.keyMouse(), keyQuit}
}
func (k queueKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyFilter, keyEnter, keyCollapse, keyDesc, keyNotes, keyLabel},
		{keyTab, keyShiftTab, k.m.keyMouse(), keyQuit},
	}
}

type cruciblesKeyMap struct{ m *Model }

func (k cruciblesKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyTab, k.m.keyMouse(), keyQuit}
}
func (k cruciblesKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll},
		{keyTab, keyShiftTab, k.m.keyMouse(), keyQuit},
	}
}

type readyToMergeKeyMap struct{ m *Model }

func (k readyToMergeKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyMerge, keyTab, k.m.keyMouse(), keyQuit}
}
func (k readyToMergeKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyMerge},
		{keyTab, keyShiftTab, k.m.keyMouse(), keyQuit},
	}
}

type needsAttentionKeyMap struct{ m *Model }

func (k needsAttentionKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyEnter, keyDesc, keyNotes, keyTab, k.m.keyMouse(), keyQuit}
}
func (k needsAttentionKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyEnter, keyDesc, keyNotes},
		{keyTab, keyShiftTab, k.m.keyMouse(), keyQuit},
	}
}

type workersKeyMap struct{ m *Model }

func (k workersKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyKill, keyViewLog, keyTab, k.m.keyMouse(), keyQuit}
}
func (k workersKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyKill, keyViewLog},
		{keyTab, keyShiftTab, k.m.keyMouse(), keyQuit},
	}
}

type usageKeyMap struct{ m *Model }

func (k usageKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyTab, k.m.keyMouse(), keyQuit}
}
func (k usageKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll},
		{keyTab, keyShiftTab, k.m.keyMouse(), keyQuit},
	}
}

type liveActivityKeyMap struct{ m *Model }

func (k liveActivityKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyExpand, keyCollapse, keyFollow, keyTab, k.m.keyMouse(), keyQuit}
}
func (k liveActivityKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyExpand, keyCollapse, keyFollow},
		{keyTab, keyShiftTab, k.m.keyMouse(), keyQuit},
	}
}

type eventsKeyMap struct{ m *Model }

func (k eventsKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyFollow, keyTab, k.m.keyMouse(), keyQuit}
}
func (k eventsKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyFollow},
		{keyTab, keyShiftTab, k.m.keyMouse(), keyQuit},
	}
}

// keyMouse returns the mouse toggle key binding with help text updated based
// on the current mouse state.
func (m *Model) keyMouse() key.Binding {
	if m.mouseEnabled {
		return key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "disable mouse (select text)"))
	}
	return key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "enable mouse"))
}

// keyMapForPanel returns the help.KeyMap appropriate for the currently focused panel.
func (m *Model) keyMapForPanel() help.KeyMap {
	switch m.focused {
	case PanelQueue:
		return queueKeyMap{m: m}
	case PanelCrucibles:
		return cruciblesKeyMap{m: m}
	case PanelReadyToMerge:
		return readyToMergeKeyMap{m: m}
	case PanelNeedsAttention:
		return needsAttentionKeyMap{m: m}
	case PanelWorkers:
		return workersKeyMap{m: m}
	case PanelUsage:
		return usageKeyMap{m: m}
	case PanelLiveActivity:
		return liveActivityKeyMap{m: m}
	case PanelEvents:
		return eventsKeyMap{m: m}
	default:
		return eventsKeyMap{m: m}
	}
}
