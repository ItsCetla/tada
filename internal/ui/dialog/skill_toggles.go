package dialog

import (
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/sahilm/fuzzy"
)

const (
	// SkillTogglesID is the identifier for the skill toggles dialog.
	SkillTogglesID              = "skill_toggles"
	skillTogglesDialogMaxWidth  = 64
	skillTogglesDialogMinHeight = 8
	skillTogglesDialogMaxHeight = 20
)

// SkillToggleScope selects which store a toggle affects.
type SkillToggleScope int

const (
	// SkillToggleScopeLocal persists repository-scoped overrides.
	SkillToggleScopeLocal SkillToggleScope = iota
	// SkillToggleScopeGlobal writes disabled_skills in the global config.
	SkillToggleScopeGlobal
)

// String returns the radio label for the scope.
func (s SkillToggleScope) String() string {
	if s == SkillToggleScopeGlobal {
		return "Global"
	}
	return "Local"
}

// SkillToggleItem describes one discovered skill in the toggles dialog.
type SkillToggleItem struct {
	Name string
	// Source says where the skill comes from, such as "builtin" or
	// "user". It is shown next to the state.
	Source string
	// Disabled reports a repository override that disables the skill.
	Disabled bool
	// ConfigDisabled reports that the skill is in the config's
	// options.disabled_skills.
	ConfigDisabled bool
	// EnabledOverride reports a repository override that enables the
	// skill even though the config disables it.
	EnabledOverride bool
	// Error reports that the skill failed to load.
	Error bool
}

// localDisabled reports the effective state for this repository: a local
// override wins over the config in both directions.
func (i SkillToggleItem) localDisabled() bool {
	return i.Disabled || (i.ConfigDisabled && !i.EnabledOverride)
}

// ActionToggleSkill is sent when the user toggles a skill. Global writes
// the config's disabled_skills; otherwise a repository override is stored.
type ActionToggleSkill struct {
	Name     string
	Disabled bool
	Global   bool
}

// SkillToggles lets the user enable and disable skills, either for the
// current repository (Local, the default) or in the config (Global).
// Changes apply from the agent's next turn.
type SkillToggles struct {
	com   *common.Common
	help  help.Model
	list  *list.FilterableList
	input textinput.Model
	items []*skillToggleListItem
	scope SkillToggleScope

	keyMap struct {
		Toggle   key.Binding
		Next     key.Binding
		Previous key.Binding
		UpDown   key.Binding
		Scope    key.Binding
		Close    key.Binding
	}
}

// skillToggleListItem is the list row for a skill.
type skillToggleListItem struct {
	*list.Versioned
	item    SkillToggleItem
	scope   *SkillToggleScope
	t       *styles.Styles
	m       fuzzy.Match
	cache   map[int]string
	focused bool
}

var (
	_ Dialog   = (*SkillToggles)(nil)
	_ ListItem = (*skillToggleListItem)(nil)
)

// NewSkillToggles creates a new skill toggles dialog.
func NewSkillToggles(com *common.Common, items []SkillToggleItem) *SkillToggles {
	m := &SkillToggles{com: com}

	m.help = help.New()
	m.help.Styles = com.Styles.DialogHelpStyles()

	m.list = list.NewFilterableList()
	m.list.Focus()

	m.input = textinput.New()
	m.input.SetVirtualCursor(false)
	m.input.Placeholder = "Type to filter"
	m.input.SetStyles(com.Styles.TextInput)
	m.input.Focus()

	m.keyMap.Toggle = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "toggle"),
	)
	m.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "ctrl+n"),
		key.WithHelp("↓", "next item"),
	)
	m.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	m.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑/↓", "choose"),
	)
	m.keyMap.Scope = key.NewBinding(
		key.WithKeys("tab", "shift+tab"),
		key.WithHelp("tab", "local/global"),
	)
	m.keyMap.Close = CloseKey

	m.items = make([]*skillToggleListItem, 0, len(items))
	listItems := make([]list.FilterableItem, 0, len(items))
	for _, it := range items {
		li := &skillToggleListItem{
			Versioned: list.NewVersioned(),
			item:      it,
			scope:     &m.scope,
			t:         com.Styles,
		}
		m.items = append(m.items, li)
		listItems = append(listItems, li)
	}
	m.list.SetItems(listItems...)
	m.list.SetSelected(0)

	return m
}

// ID implements Dialog.
func (m *SkillToggles) ID() string {
	return SkillTogglesID
}

// Items returns the current items.
func (m *SkillToggles) Items() []SkillToggleItem {
	out := make([]SkillToggleItem, len(m.items))
	for i, li := range m.items {
		out[i] = li.item
	}
	return out
}

// Scope returns the selected toggle scope.
func (m *SkillToggles) Scope() SkillToggleScope {
	return m.scope
}

// HandleMsg implements Dialog.
func (m *SkillToggles) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, m.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, m.keyMap.Previous):
			m.list.Focus()
			if m.list.IsSelectedFirst() {
				m.list.SelectLast()
				m.list.ScrollToBottom()
				break
			}
			m.list.SelectPrev()
			m.list.ScrollToSelected()
		case key.Matches(msg, m.keyMap.Next):
			m.list.Focus()
			if m.list.IsSelectedLast() {
				m.list.SelectFirst()
				m.list.ScrollToTop()
				break
			}
			m.list.SelectNext()
			m.list.ScrollToSelected()
		case key.Matches(msg, m.keyMap.Scope):
			if m.scope == SkillToggleScopeLocal {
				m.scope = SkillToggleScopeGlobal
			} else {
				m.scope = SkillToggleScopeLocal
			}
			// The state column depends on the scope.
			for _, li := range m.items {
				li.invalidate()
			}
		case key.Matches(msg, m.keyMap.Toggle):
			li, ok := m.list.SelectedItem().(*skillToggleListItem)
			if !ok || li == nil {
				break
			}
			return m.toggle(li)
		default:
			prevValue := m.input.Value()
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			value := m.input.Value()
			if value != prevValue {
				m.list.SetFilter(value)
				m.list.ScrollToTop()
				m.list.SetSelected(0)
			}
			return ActionCmd{cmd}
		}
	}
	return nil
}

// toggle flips the item's state for the active scope, optimistically
// updating the row, and returns the action that persists it.
func (m *SkillToggles) toggle(li *skillToggleListItem) Action {
	global := m.scope == SkillToggleScopeGlobal
	if global {
		li.item.ConfigDisabled = !li.item.ConfigDisabled
	} else {
		disabled := !li.item.localDisabled()
		li.item.Disabled = disabled
		li.item.EnabledOverride = !disabled
	}
	li.invalidate()
	disabled := li.disabled()
	return ActionToggleSkill{Name: li.item.Name, Disabled: disabled, Global: global}
}

// Cursor returns the cursor position relative to the dialog.
func (m *SkillToggles) Cursor() *tea.Cursor {
	return InputCursor(m.com.Styles, m.input.Cursor())
}

// Draw implements Dialog.
func (m *SkillToggles) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := m.com.Styles
	width := max(0, min(skillTogglesDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()

	m.input.SetWidth(dialogInputTextWidth(t, m.input, innerWidth))

	visibleCount := len(m.list.FilteredItems())

	// Size the dialog to fit the list content, clamped to min/max bounds.
	listTotalHeight := max(1, m.list.TotalHeight())
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.InputPrompt.GetVerticalFrameSize() + inputContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() +
		t.Dialog.View.GetVerticalFrameSize()
	desiredHeight := heightOffset + listTotalHeight
	maxAvailable := area.Dy() - t.Dialog.View.GetVerticalBorderSize()
	height := max(skillTogglesDialogMinHeight, min(skillTogglesDialogMaxHeight, desiredHeight, maxAvailable))

	listHeight, listTotalHeight, _ := sizeDialogList(t, m.list, innerWidth, height)

	rc := NewRenderContext(t, width)
	rc.Title = "Toggle Skills"
	rc.TitleInfo = m.scopeRadioView(t)
	rc.AddPart(t.Dialog.InputPrompt.Render(m.input.View()))

	if visibleCount == 0 {
		msg := "No matching skills."
		if len(m.items) == 0 {
			msg = "No skills found."
		}
		rc.AddPart(t.Dialog.SecondaryText.
			Width(innerWidth).
			Padding(0, 1).
			Render(msg))
	} else {
		if m.list.Height() >= visibleCount {
			m.list.ScrollToTop()
		} else {
			m.list.ScrollToSelected()
		}
		listView := t.Dialog.List.Height(m.list.Height()).Render(m.list.Render())
		listView = joinScrollbar(t, listView, listHeight, listTotalHeight, listHeight, m.list.Offset())
		rc.AddPart(listView)
	}
	rc.Help = renderDialogHelp(t, &m.help, m, innerWidth)

	view := rc.Render()

	cur := m.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// scopeRadioView renders the Local/Global radio selector, mirroring
// MCPToggles.
func (m *SkillToggles) scopeRadioView(t *styles.Styles) string {
	radio := func(s SkillToggleScope) string {
		bullet := t.Radio.Off
		if s == m.scope {
			bullet = t.Radio.On
		}
		return bullet.Render() + t.Radio.Label.Padding(0, 1).Render(s.String())
	}
	return " " + radio(SkillToggleScopeLocal) + " " + radio(SkillToggleScopeGlobal)
}

// ShortHelp implements help.KeyMap.
func (m *SkillToggles) ShortHelp() []key.Binding {
	return []key.Binding{m.keyMap.UpDown, m.keyMap.Toggle, m.keyMap.Scope, m.keyMap.Close}
}

// FullHelp implements help.KeyMap.
func (m *SkillToggles) FullHelp() [][]key.Binding {
	return [][]key.Binding{{m.keyMap.Toggle, m.keyMap.Next, m.keyMap.Previous, m.keyMap.Scope}, {m.keyMap.Close}}
}

// disabled reports the item's state for the active scope.
func (li *skillToggleListItem) disabled() bool {
	if *li.scope == SkillToggleScopeGlobal {
		return li.item.ConfigDisabled
	}
	return li.item.localDisabled()
}

func (li *skillToggleListItem) invalidate() {
	li.cache = nil
	li.Bump()
}

// Finished implements list.Item.
func (li *skillToggleListItem) Finished() bool {
	return true
}

// Filter implements list.FilterableItem.
func (li *skillToggleListItem) Filter() string {
	return li.item.Name
}

// ID implements ListItem.
func (li *skillToggleListItem) ID() string {
	return li.item.Name
}

// SetFocused implements list.Focusable.
func (li *skillToggleListItem) SetFocused(focused bool) {
	if li.focused == focused {
		return
	}
	li.focused = focused
	li.invalidate()
}

// SetMatch implements list.MatchSettable.
func (li *skillToggleListItem) SetMatch(m fuzzy.Match) {
	if sameFuzzyMatch(li.m, m) {
		return
	}
	li.m = m
	li.invalidate()
}

// Render implements list.Item.
func (li *skillToggleListItem) Render(width int) string {
	styles := ListItemStyles{
		ItemBlurred:     li.t.Dialog.NormalItem,
		ItemFocused:     li.t.Dialog.SelectedItem,
		InfoTextBlurred: li.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: li.t.Dialog.ListItem.InfoFocused,
	}
	return renderItem(styles, li.item.Name, skillItemStatus(li.item, li.disabled()), li.focused, width, li.cache, &li.m)
}

// skillItemStatus returns the right-hand label for an item: its source
// and its state.
func skillItemStatus(item SkillToggleItem, disabled bool) string {
	state := "on"
	switch {
	case disabled:
		state = "off"
	case item.Error:
		state = "error"
	}
	if item.Source == "" {
		return state
	}
	return item.Source + " · " + state
}
