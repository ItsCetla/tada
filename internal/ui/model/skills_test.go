package model

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/ui/common"
	uistyles "github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestSkillStatusItemsIncludesBuiltinSkills verifies sidebar skills include
// both runtime-discovered skill states and builtin skills that may not have
// emitted a SkillState event yet.
func TestSkillStatusItemsIncludesBuiltinSkills(t *testing.T) {
	t.Parallel()

	st := uistyles.CharmtonePantera()
	ui := &UI{
		com: &common.Common{Styles: &st},
		skillStates: []*skills.SkillState{
			{Name: "go-doc", Path: "/tmp/go-doc/SKILL.md", State: skills.StateNormal},
		},
	}

	items := ui.skillStatusItems()
	require.NotEmpty(t, items)

	var hasGoDoc bool
	for _, item := range items {
		if item.title == st.Resource.Name.Render("go-doc") {
			hasGoDoc = true
			break
		}
	}
	require.True(t, hasGoDoc)

	builtinSkills := skills.DiscoverBuiltin()
	require.NotEmpty(t, builtinSkills)

	var hasBuiltin bool
	for _, skill := range builtinSkills {
		if skill.Name == "go-doc" {
			continue
		}
		expected := st.Resource.Name.Render(skill.Name)
		for _, item := range items {
			if item.title == expected {
				hasBuiltin = true
				break
			}
		}
		if hasBuiltin {
			break
		}
	}
	require.True(t, hasBuiltin)
}

func TestSkillStatusItemsShowsDisabledSkillsMuted(t *testing.T) {
	t.Parallel()

	st := uistyles.CharmtonePantera()
	ui := &UI{
		com: &common.Common{
			Styles:    &st,
			Workspace: &testWorkspace{cfg: &config.Config{Options: &config.Options{DisabledSkills: []string{"go-doc", "tada-config", "broken"}}}},
		},
		skillStates: []*skills.SkillState{
			{Name: "go-doc", Path: "/tmp/go-doc/SKILL.md", State: skills.StateNormal},
			{Name: "broken", Path: "/tmp/broken/SKILL.md", State: skills.StateError},
		},
	}

	byName := map[string]skillStatusItem{}
	for _, item := range ui.skillStatusItems() {
		byName[item.name] = item
	}

	for _, name := range []string{"go-doc", "tada-config"} {
		item, ok := byName[name]
		require.True(t, ok, "%s is listed even though it is disabled", name)
		require.True(t, item.disabled)
		require.Equal(t, st.Resource.DisabledIcon.String(), item.icon)
		require.Equal(t, st.Resource.AdditionalText.Render(name), item.title)
	}

	broken := byName["broken"]
	require.Equal(t, st.Resource.ErrorIcon.String(), broken.icon, "error state keeps its icon")
	require.True(t, broken.disabled)

	for name, item := range byName {
		if name == "go-doc" || name == "tada-config" || name == "broken" {
			continue
		}
		require.False(t, item.disabled, name)
		require.Equal(t, st.Resource.OnlineIcon.String(), item.icon, name)
	}
}

func TestSkillStatusItemsLocalOverridesWin(t *testing.T) {
	t.Parallel()

	st := uistyles.CharmtonePantera()
	ui := &UI{
		com: &common.Common{
			Styles:    &st,
			Workspace: &testWorkspace{cfg: &config.Config{Options: &config.Options{DisabledSkills: []string{"go-doc"}}}},
		},
		skillStates: []*skills.SkillState{
			{Name: "go-doc", Path: "/tmp/go-doc/SKILL.md", State: skills.StateNormal},
			{Name: "jq-tools", Path: "/tmp/jq-tools/SKILL.md", State: skills.StateNormal},
		},
		skillLocalDisabled: []string{"jq-tools"},
		skillLocalEnabled:  []string{"go-doc"},
	}

	byName := map[string]skillStatusItem{}
	for _, item := range ui.skillStatusItems() {
		byName[item.name] = item
	}
	require.False(t, byName["go-doc"].disabled, "a local enable beats the global disable")
	require.True(t, byName["jq-tools"].disabled, "a local disable applies")
}

func TestSkillsCountText(t *testing.T) {
	t.Parallel()

	require.Equal(t, "3", skillsCountText([]skillStatusItem{{}, {}, {}}))
	require.Equal(t, "2 active · 1 off", skillsCountText([]skillStatusItem{{}, {disabled: true}, {}}))
}

func TestSkillSource(t *testing.T) {
	t.Parallel()

	require.Equal(t, "builtin", skillSource(skills.BuiltinPrefix+"jq/SKILL.md", "/repo"))
	require.Equal(t, "project", skillSource("/repo/.tada/skills/x/SKILL.md", "/repo"))
	require.Equal(t, "user", skillSource("/home/u/.config/tada/skills/x/SKILL.md", "/repo"))
}
