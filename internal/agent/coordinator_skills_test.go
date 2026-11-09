package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/stretchr/testify/require"
)

// TestCoordinatorSyncDisabledSkills checks that a skill toggled in
// options.disabled_skills mid-session reaches the main agent's system
// prompt, the active skill list, and the tracker on the next turn.
func TestCoordinatorSyncDisabledSkills(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	opts := cfg.Config().Options
	opts.SkillsPaths = nil
	opts.DisabledSkills = nil

	p, err := prompt.NewPrompt("skills", "{{.AvailSkillXML}}", prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)
	sa := testSessionAgentWithMC(nil, nil, &mcRecordingModel{}, nil, nil)
	all := skills.DiscoverBuiltin()
	require.NotEmpty(t, all)
	name := all[0].Name

	c := &coordinator{
		cfg:          cfg,
		agents:       map[string]SessionAgent{config.AgentCoder: sa},
		prompts:      map[string]*prompt.Prompt{config.AgentCoder: p},
		allSkills:    all,
		activeSkills: all,
		skillTracker: skills.NewTracker(all),
	}

	// Unchanged list: the prompt is left alone.
	require.NoError(t, c.syncDisabledSkills(t.Context()))
	require.Equal(t, "system", sa.systemPrompt.Get())

	opts.DisabledSkills = []string{name}
	require.NoError(t, c.syncDisabledSkills(t.Context()))
	require.NotContains(t, sa.systemPrompt.Get(), `name="`+name+`"`)
	require.Len(t, c.currentActiveSkills(), len(all)-1)
	c.skillTracker.MarkLoaded(name)
	require.False(t, c.skillTracker.IsLoaded(name), "a disabled skill is not tracked")

	opts.DisabledSkills = nil
	require.NoError(t, c.syncDisabledSkills(t.Context()))
	require.Contains(t, sa.systemPrompt.Get(), `name="`+name+`"`)
	require.Len(t, c.currentActiveSkills(), len(all))
}

// TestCoordinatorSyncLocalSkillOverrides checks that repository-scoped
// overrides reach the prompt on the next turn and win over the config.
func TestCoordinatorSyncLocalSkillOverrides(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	opts := cfg.Config().Options
	opts.SkillsPaths = nil
	all := skills.DiscoverBuiltin()
	require.NotEmpty(t, all)
	name := all[0].Name
	opts.DisabledSkills = []string{name}

	p, err := prompt.NewPrompt("skills", "{{.AvailSkillXML}}", prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)
	sa := testSessionAgentWithMC(nil, nil, &mcRecordingModel{}, nil, nil)
	c := &coordinator{
		cfg:          cfg,
		agents:       map[string]SessionAgent{config.AgentCoder: sa},
		prompts:      map[string]*prompt.Prompt{config.AgentCoder: p},
		allSkills:    all,
		activeSkills: all,
		skillTracker: skills.NewTracker(all),
	}

	require.NoError(t, c.syncDisabledSkills(t.Context()))
	require.NotContains(t, sa.systemPrompt.Get(), `name="`+name+`"`)

	// A local enable beats the global disable.
	cfg.SetLocalSkillOverrides(nil, []string{name})
	require.NoError(t, c.syncDisabledSkills(t.Context()))
	require.Contains(t, sa.systemPrompt.Get(), `name="`+name+`"`)
	require.Len(t, c.currentActiveSkills(), len(all))

	// A local disable applies with no config entry.
	opts.DisabledSkills = nil
	cfg.SetLocalSkillOverrides([]string{name}, nil)
	require.NoError(t, c.syncDisabledSkills(t.Context()))
	require.NotContains(t, sa.systemPrompt.Get(), `name="`+name+`"`)
	require.Len(t, c.currentActiveSkills(), len(all)-1)
}

func TestDisabledSkillsKeyIgnoresOrder(t *testing.T) {
	t.Parallel()
	require.Equal(t, disabledSkillsKey([]string{"b", "a", "a"}), disabledSkillsKey([]string{"a", "b"}))
	require.NotEqual(t, disabledSkillsKey([]string{"a"}), disabledSkillsKey(nil))
}
