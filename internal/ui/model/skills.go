package model

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/x/ansi"
)

type skillStatusItem struct {
	icon  string
	name  string
	title string
	// description is reserved for future use (e.g. showing error details).
	description string
	// disabled reports that the skill is turned off; it is still listed,
	// muted.
	disabled bool
}

var builtinSkillsCache struct {
	once   sync.Once
	skills []*skills.Skill
}

func cachedBuiltinSkills() []*skills.Skill {
	builtinSkillsCache.once.Do(func() {
		builtinSkillsCache.skills = skills.DiscoverBuiltin()
	})
	return builtinSkillsCache.skills
}

// skillsInfo renders the skill discovery status section showing loaded and
// invalid skills.
func (m *UI) skillsInfo(width, maxItems int, isSection bool) string {
	t := m.com.Styles

	title := t.Resource.Heading.Render("Skills")
	if isSection {
		title = common.Section(t, title, width)
	}

	items := m.skillStatusItems()
	if len(items) == 0 {
		list := t.Resource.AdditionalText.Render("None")
		return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
	}

	list := skillsList(t, items, width, maxItems)
	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
}

// effectiveDisabledSkills returns the config's disabled_skills merged
// with this repository's cached overrides, as a set.
func (m *UI) effectiveDisabledSkills() map[string]bool {
	set := make(map[string]bool)
	if m.com == nil || m.com.Workspace == nil {
		return set
	}
	var global []string
	if cfg := m.com.Config(); cfg != nil && cfg.Options != nil {
		global = cfg.Options.DisabledSkills
	}
	for _, name := range config.EffectiveDisabledSkills(global, m.skillLocalDisabled, m.skillLocalEnabled) {
		set[name] = true
	}
	return set
}

func (m *UI) skillStatusItems() []skillStatusItem {
	t := m.com.Styles
	var items []skillStatusItem
	stateNames := make(map[string]struct{}, len(m.skillStates))

	disabledSet := m.effectiveDisabledSkills()
	item := func(name string, failed bool) skillStatusItem {
		it := skillStatusItem{
			icon:  t.Resource.OnlineIcon.String(),
			name:  name,
			title: t.Resource.Name.Render(name),
		}
		switch {
		case failed:
			it.icon = t.Resource.ErrorIcon.String()
			it.disabled = disabledSet[name]
		case disabledSet[name]:
			it.icon = t.Resource.DisabledIcon.String()
			it.title = t.Resource.AdditionalText.Render(name)
			it.disabled = true
		}
		return it
	}

	states := slices.Clone(m.skillStates)
	slices.SortStableFunc(states, func(a, b *skills.SkillState) int {
		return strings.Compare(a.Path, b.Path)
	})
	for _, state := range states {
		name := state.Name
		if name == "" {
			name = filepath.Base(filepath.Dir(state.Path))
		}
		if _, exists := stateNames[name]; exists {
			continue
		}
		stateNames[name] = struct{}{}
		items = append(items, item(name, state.State == skills.StateError))
	}

	builtin := cachedBuiltinSkills()
	slices.SortStableFunc(builtin, func(a, b *skills.Skill) int {
		return strings.Compare(a.Name, b.Name)
	})
	for _, skill := range builtin {
		if _, ok := stateNames[skill.Name]; ok {
			continue
		}
		items = append(items, item(skill.Name, false))
	}

	slices.SortStableFunc(items, func(a, b skillStatusItem) int {
		return strings.Compare(a.name, b.name)
	})

	return items
}

// skillsCountText renders the landing heading's count: the number of
// skills, or "N active · M off" when some are turned off.
func skillsCountText(items []skillStatusItem) string {
	off := 0
	for _, it := range items {
		if it.disabled {
			off++
		}
	}
	if off == 0 {
		return fmt.Sprint(len(items))
	}
	return fmt.Sprintf("%d active · %d off", len(items)-off, off)
}

// skillOverridesLoadedMsg carries this repository's skill overrides.
type skillOverridesLoadedMsg struct {
	disabled []string
	enabled  []string
}

// loadSkillOverrides reads the repository's skill overrides so the
// sidebar and landing can show the effective state without querying the
// workspace on every render.
func (m *UI) loadSkillOverrides() tea.Cmd {
	if m.com == nil || m.com.Workspace == nil {
		return nil
	}
	ws := m.com.Workspace
	return func() tea.Msg {
		ctx := context.Background()
		disabled, err := ws.SkillsDisabled(ctx)
		if err != nil {
			slog.Warn("Failed to load disabled skill overrides", "error", err)
			return nil
		}
		enabled, err := ws.SkillsEnabled(ctx)
		if err != nil {
			slog.Warn("Failed to load enabled skill overrides", "error", err)
			return nil
		}
		return skillOverridesLoadedMsg{disabled: disabled, enabled: enabled}
	}
}

// openSkillTogglesDialog opens the skill toggles dialog.
func (m *UI) openSkillTogglesDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.SkillTogglesID) {
		m.dialog.BringToFront(dialog.SkillTogglesID)
		return nil
	}
	items, err := m.skillToggleItems()
	if err != nil {
		return util.ReportError(err)
	}
	m.dialog.OpenDialog(dialog.NewSkillToggles(m.com, items))
	return nil
}

// skillToggleItems builds the dialog items from the discovered and
// builtin skills, the config's disabled_skills, and the repository's
// overrides.
func (m *UI) skillToggleItems() ([]dialog.SkillToggleItem, error) {
	ctx := context.Background()
	localDisabled, err := m.com.Workspace.SkillsDisabled(ctx)
	if err != nil {
		return nil, err
	}
	localEnabled, err := m.com.Workspace.SkillsEnabled(ctx)
	if err != nil {
		return nil, err
	}
	m.skillLocalDisabled, m.skillLocalEnabled = localDisabled, localEnabled

	toSet := func(names []string) map[string]bool {
		set := make(map[string]bool, len(names))
		for _, n := range names {
			set[n] = true
		}
		return set
	}
	var configDisabled []string
	if cfg := m.com.Config(); cfg != nil && cfg.Options != nil {
		configDisabled = cfg.Options.DisabledSkills
	}
	cfgSet, offSet, onSet := toSet(configDisabled), toSet(localDisabled), toSet(localEnabled)

	seen := make(map[string]bool)
	var items []dialog.SkillToggleItem
	add := func(name, source string, failed bool) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		items = append(items, dialog.SkillToggleItem{
			Name:            name,
			Source:          source,
			Disabled:        offSet[name],
			ConfigDisabled:  cfgSet[name],
			EnabledOverride: onSet[name],
			Error:           failed,
		})
	}

	states := slices.Clone(m.skillStates)
	slices.SortStableFunc(states, func(a, b *skills.SkillState) int {
		return strings.Compare(a.Path, b.Path)
	})
	for _, state := range states {
		name := state.Name
		if name == "" {
			name = filepath.Base(filepath.Dir(state.Path))
		}
		add(name, skillSource(state.Path, m.com.Workspace.WorkingDir()), state.State == skills.StateError)
	}
	for _, skill := range cachedBuiltinSkills() {
		add(skill.Name, "builtin", false)
	}
	slices.SortStableFunc(items, func(a, b dialog.SkillToggleItem) int {
		return strings.Compare(a.Name, b.Name)
	})
	return items, nil
}

// skillSource labels where a discovered skill lives: builtin skills,
// skills under the working directory ("project"), or anything else
// ("user").
func skillSource(path, workingDir string) string {
	if strings.HasPrefix(path, skills.BuiltinPrefix) {
		return "builtin"
	}
	if workingDir != "" && strings.HasPrefix(path, workingDir+string(filepath.Separator)) {
		return "project"
	}
	return "user"
}

// applySkillToggle persists a skill toggle. Local toggles write a
// repository-scoped override; global toggles write the config's
// disabled_skills. The change applies from the agent's next turn. The
// cached overrides are refreshed afterwards for the sidebar.
func (m *UI) applySkillToggle(msg dialog.ActionToggleSkill) tea.Cmd {
	name := msg.Name
	disable := msg.Disabled
	status := "enabled"
	if disable {
		status = "disabled"
	}
	ws := m.com.Workspace
	apply := func() tea.Msg {
		if msg.Global {
			if err := ws.SetSkillDisabled(context.TODO(), name, disable); err != nil {
				return util.NewErrorMsg(err)
			}
			return util.NewInfoMsg(fmt.Sprintf("Skill %q %s globally", name, status))
		}
		if err := ws.SkillSetLocalDisabled(context.TODO(), name, disable); err != nil {
			return util.NewErrorMsg(err)
		}
		return util.NewInfoMsg(fmt.Sprintf("Skill %q %s for this repository", name, status))
	}
	return tea.Sequence(apply, m.loadSkillOverrides())
}

func skillsList(t *styles.Styles, items []skillStatusItem, width, maxItems int) string {
	if maxItems <= 0 {
		return ""
	}

	if len(items) > maxItems {
		visibleItems := items[:maxItems-1]
		remaining := len(items) - (maxItems - 1)
		items = append(visibleItems, skillStatusItem{
			name:  "more",
			title: t.Resource.AdditionalText.Render(fmt.Sprintf("…and %d more", remaining)),
		})
	}

	renderedItems := make([]string, 0, len(items))
	for _, item := range items {
		renderedItems = append(renderedItems, common.Status(t, common.StatusOpts{
			Icon:        item.icon,
			Title:       item.title,
			Description: item.description,
		}, width))
	}
	return lipgloss.JoinVertical(lipgloss.Left, renderedItems...)
}

// skillsGrid renders the landing page's skills section: a heading with the
// skill count, then the skills flowed top-to-bottom into as many columns as
// fit in width. At most height lines are used; overflow ends in "…and N more".
func (m *UI) skillsGrid(width, height int) string {
	t := m.com.Styles
	items := m.skillStatusItems()

	title := t.Resource.Heading.Render("Skills")
	if len(items) == 0 {
		return lipgloss.NewStyle().Width(width).Render(title + "\n\n" + t.Resource.AdditionalText.Render("None"))
	}
	title += " " + t.Resource.AdditionalText.Render(skillsCountText(items))

	rows := height - 2 // heading + blank line
	if rows <= 0 {
		return lipgloss.NewStyle().Width(width).Render(title)
	}
	return lipgloss.NewStyle().Width(width).Render(title + "\n\n" + skillsGridBody(t, items, width, rows))
}

// skillsGridBody lays items out column-major in a grid width cells wide and
// rows lines tall.
func skillsGridBody(t *styles.Styles, items []skillStatusItem, width, rows int) string {
	const (
		minColWidth = 16
		maxColWidth = 30
		gap         = 2
	)
	longest := 0
	for _, it := range items {
		longest = max(longest, lipgloss.Width(it.name))
	}
	// icon + space + name + gap
	colWidth := min(max(longest+2+gap, minColWidth), maxColWidth, max(width, 1))
	cols := max(1, width/colWidth)

	if capacity := rows * cols; len(items) > capacity {
		shown := capacity - 1
		items = append(items[:shown:shown], skillStatusItem{
			name:  "more",
			title: t.Resource.AdditionalText.Render(fmt.Sprintf("…and %d more", len(items)-shown)),
		})
	}
	usedRows := min(rows, (len(items)+cols-1)/cols)

	columns := make([]string, 0, cols)
	for c := 0; c*usedRows < len(items); c++ {
		end := min((c+1)*usedRows, len(items))
		cells := make([]string, 0, usedRows)
		for _, it := range items[c*usedRows : end] {
			cell := common.Status(t, common.StatusOpts{Icon: it.icon, Title: it.title}, colWidth)
			cell = ansi.Truncate(cell, colWidth-gap, "…")
			cells = append(cells, lipgloss.NewStyle().Width(colWidth).Render(cell))
		}
		columns = append(columns, lipgloss.JoinVertical(lipgloss.Left, cells...))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, columns...)
}
