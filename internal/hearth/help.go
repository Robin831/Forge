package hearth

import (
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
)

// --- Key binding definitions ---

var (
	keyTab = key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "next panel"),
	)
	keyShiftTab = key.NewBinding(
		key.WithKeys("shift+tab"),
		key.WithHelp("shift+tab", "prev panel"),
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
	keyLabel = key.NewBinding(
		key.WithKeys("l"),
		key.WithHelp("l", "label for dispatch"),
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

type queueKeyMap struct{}

func (queueKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyEnter, keyLabel, keyTab, keyQuit}
}
func (queueKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyEnter, keyLabel},
		{keyTab, keyShiftTab, keyMouse, keyQuit},
	}
}

type cruciblesKeyMap struct{}

func (cruciblesKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyTab, keyQuit}
}
func (cruciblesKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll},
		{keyTab, keyShiftTab, keyMouse, keyQuit},
	}
}

type readyToMergeKeyMap struct{}

func (readyToMergeKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyMerge, keyTab, keyQuit}
}
func (readyToMergeKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyMerge},
		{keyTab, keyShiftTab, keyMouse, keyQuit},
	}
}

type needsAttentionKeyMap struct{}

func (needsAttentionKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyEnter, keyTab, keyQuit}
}
func (needsAttentionKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyEnter},
		{keyTab, keyShiftTab, keyMouse, keyQuit},
	}
}

type workersKeyMap struct{}

func (workersKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyKill, keyTab, keyQuit}
}
func (workersKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyKill},
		{keyTab, keyShiftTab, keyMouse, keyQuit},
	}
}

type usageKeyMap struct{}

func (usageKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyTab, keyQuit}
}
func (usageKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll},
		{keyTab, keyShiftTab, keyMouse, keyQuit},
	}
}

type liveActivityKeyMap struct{}

func (liveActivityKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyExpand, keyCollapse, keyFollow, keyTab, keyQuit}
}
func (liveActivityKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyExpand, keyCollapse, keyFollow},
		{keyTab, keyShiftTab, keyMouse, keyQuit},
	}
}

type eventsKeyMap struct{}

func (eventsKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyScroll, keyFollow, keyTab, keyQuit}
}
func (eventsKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyScroll, keyFollow},
		{keyTab, keyShiftTab, keyMouse, keyQuit},
	}
}

// keyMapForPanel returns the help.KeyMap appropriate for the currently focused panel.
func (m *Model) keyMapForPanel() help.KeyMap {
	switch m.focused {
	case PanelQueue:
		return queueKeyMap{}
	case PanelCrucibles:
		return cruciblesKeyMap{}
	case PanelReadyToMerge:
		return readyToMergeKeyMap{}
	case PanelNeedsAttention:
		return needsAttentionKeyMap{}
	case PanelWorkers:
		return workersKeyMap{}
	case PanelUsage:
		return usageKeyMap{}
	case PanelLiveActivity:
		return liveActivityKeyMap{}
	case PanelEvents:
		return eventsKeyMap{}
	default:
		return eventsKeyMap{}
	}
}
