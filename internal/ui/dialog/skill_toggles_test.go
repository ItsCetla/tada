package dialog

import (
	"image"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"
)

func newSkillTogglesForTest(items []SkillToggleItem) *SkillToggles {
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s}
	return NewSkillToggles(com, items)
}

func drawSkillToggles(m *SkillToggles) string {
	scr := uv.NewScreenBuffer(100, 30)
	m.Draw(scr, image.Rect(0, 0, 100, 30))
	return scr.String()
}

func typeSkillFilter(m *SkillToggles, s string) {
	for _, r := range s {
		m.HandleMsg(keyMsg(r))
	}
}

func toggleSelected(t *testing.T, m *SkillToggles) ActionToggleSkill {
	t.Helper()
	action, ok := m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionToggleSkill)
	require.True(t, ok)
	return action
}

func TestSkillToggles_Toggle(t *testing.T) {
	t.Parallel()

	m := newSkillTogglesForTest([]SkillToggleItem{
		{Name: "jq", Source: "builtin"},
		{Name: "tada-config", Source: "builtin", Disabled: true},
	})
	require.Equal(t, SkillToggleScopeLocal, m.Scope(), "local is the default scope")

	require.Equal(t, ActionToggleSkill{Name: "jq", Disabled: true}, toggleSelected(t, m))
	require.True(t, m.Items()[0].Disabled, "the row flips optimistically")

	require.Nil(t, m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown}))
	require.Equal(t, ActionToggleSkill{Name: "tada-config", Disabled: false}, toggleSelected(t, m))
	require.False(t, m.Items()[1].Disabled)
	require.True(t, m.Items()[1].EnabledOverride)
}

func TestSkillToggles_NavigationWrapsAndClose(t *testing.T) {
	t.Parallel()

	m := newSkillTogglesForTest([]SkillToggleItem{{Name: "a"}, {Name: "b"}})
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, "b", toggleSelected(t, m).Name, "up from the top wraps to the bottom")

	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, "a", toggleSelected(t, m).Name, "down from the bottom wraps to the top")

	require.IsType(t, ActionClose{}, m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape}))
}

func TestSkillToggles_EmptyStates(t *testing.T) {
	t.Parallel()

	m := newSkillTogglesForTest(nil)
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Nil(t, m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Contains(t, drawSkillToggles(m), "No skills found.")

	m = newSkillTogglesForTest([]SkillToggleItem{{Name: "jq"}})
	typeSkillFilter(m, "zzz")
	out := drawSkillToggles(m)
	require.Contains(t, out, "No matching skills.")
	require.NotContains(t, out, "jq")
	require.Nil(t, m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}), "nothing to toggle when nothing matches")
}

func TestSkillToggles_FilterByName(t *testing.T) {
	t.Parallel()

	m := newSkillTogglesForTest([]SkillToggleItem{
		{Name: "jq", Source: "builtin"},
		{Name: "tada-config", Source: "builtin"},
		{Name: "pdf-tools", Source: "user"},
	})
	typeSkillFilter(m, "pdf")
	require.Len(t, m.list.FilteredItems(), 1)
	out := drawSkillToggles(m)
	require.Contains(t, out, "pdf-tools")
	require.NotContains(t, out, "tada-config")
}

func TestSkillToggles_ToggleFilteredItem(t *testing.T) {
	t.Parallel()

	m := newSkillTogglesForTest([]SkillToggleItem{
		{Name: "jq"},
		{Name: "tada-config"},
		{Name: "tada-release"},
	})
	typeSkillFilter(m, "tada")
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	toggled := toggleSelected(t, m)
	require.Equal(t, ActionToggleSkill{Name: "tada-release", Disabled: true}, toggled)
	require.False(t, m.Items()[0].Disabled, "jq is untouched")
	require.True(t, m.Items()[2].Disabled)
}

func TestSkillToggles_ScopeFlipWhileFiltered(t *testing.T) {
	t.Parallel()

	m := newSkillTogglesForTest([]SkillToggleItem{
		{Name: "jq"},
		{Name: "pdf-tools"},
	})
	typeSkillFilter(m, "pdf")
	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, SkillToggleScopeGlobal, m.Scope())
	require.Equal(t, "pdf", m.input.Value(), "tab must not reach the filter")

	toggled := toggleSelected(t, m)
	require.Equal(t, ActionToggleSkill{Name: "pdf-tools", Disabled: true, Global: true}, toggled)
	require.True(t, m.Items()[1].ConfigDisabled)
	require.False(t, m.Items()[1].Disabled, "a global toggle leaves local overrides alone")

	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, SkillToggleScopeLocal, m.Scope())
}

func TestSkillToggles_LocalOverridesGlobalDisplay(t *testing.T) {
	t.Parallel()

	m := newSkillTogglesForTest([]SkillToggleItem{
		{Name: "jq", Source: "builtin", ConfigDisabled: true, EnabledOverride: true},
	})
	out := drawSkillToggles(m)
	require.Contains(t, out, "builtin · on", "local enable wins over the config")

	m.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	out = drawSkillToggles(m)
	require.Contains(t, out, "builtin · off", "global shows the raw config state")

	// Locally disabling a config-disabled skill keeps it off.
	m = newSkillTogglesForTest([]SkillToggleItem{{Name: "jq", ConfigDisabled: true}})
	require.Contains(t, drawSkillToggles(m), "off")
	toggled := toggleSelected(t, m)
	require.Equal(t, ActionToggleSkill{Name: "jq", Disabled: false}, toggled, "local enable over a global disable")
	require.True(t, m.Items()[0].EnabledOverride)
}

func TestSkillItemStatus(t *testing.T) {
	t.Parallel()

	require.Equal(t, "builtin · on", skillItemStatus(SkillToggleItem{Source: "builtin"}, false))
	require.Equal(t, "user · off", skillItemStatus(SkillToggleItem{Source: "user", Error: true}, true))
	require.Equal(t, "error", skillItemStatus(SkillToggleItem{Error: true}, false))
}
