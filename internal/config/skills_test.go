package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/env"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSetSkillDisabled(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "tada.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"options":{"disabled_skills":["jq"]}}`), 0o600))

	// The merged in-memory list also carries an entry from another
	// config layer ("from-project"), which must never leak into this
	// scope's file.
	store := &ConfigStore{
		config:         &Config{Options: &Options{DisabledSkills: []string{"jq", "from-project"}}},
		globalDataPath: configPath,
		resolver:       NewShellVariableResolver(env.New()),
	}

	fileList := func() []string {
		data, err := os.ReadFile(configPath)
		require.NoError(t, err)
		var out []string
		for _, v := range gjson.GetBytes(data, "options.disabled_skills").Array() {
			out = append(out, v.String())
		}
		return out
	}

	require.NoError(t, store.SetSkillDisabled(ScopeGlobal, "tada-config", true))
	require.Equal(t, []string{"jq", "tada-config"}, fileList())
	require.ElementsMatch(t, []string{"jq", "from-project", "tada-config"}, store.Config().Options.DisabledSkills)

	// Disabling twice does not duplicate the entry.
	require.NoError(t, store.SetSkillDisabled(ScopeGlobal, "tada-config", true))
	require.Equal(t, []string{"jq", "tada-config"}, fileList())

	require.NoError(t, store.SetSkillDisabled(ScopeGlobal, "jq", false))
	require.Equal(t, []string{"tada-config"}, fileList())
	require.ElementsMatch(t, []string{"from-project", "tada-config"}, store.Config().Options.DisabledSkills)

	require.Error(t, store.SetSkillDisabled(ScopeGlobal, "", true))
}

func TestEffectiveDisabledSkills(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		global, local, enabled []string
		want                   []string
	}{
		{name: "global only", global: []string{"jq"}, want: []string{"jq"}},
		{name: "local disable adds", global: []string{"jq"}, local: []string{"pdf"}, want: []string{"jq", "pdf"}},
		{name: "local enable wins over global", global: []string{"jq", "pdf"}, enabled: []string{"jq"}, want: []string{"pdf"}},
		{name: "local disable wins over global enable", local: []string{"jq"}, want: []string{"jq"}},
		{name: "duplicates collapse", global: []string{"jq"}, local: []string{"jq"}, want: []string{"jq"}},
		{name: "empty", want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, EffectiveDisabledSkills(tt.global, tt.local, tt.enabled))
		})
	}
}

func TestConfigStoreEffectiveDisabledSkills(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{config: &Config{Options: &Options{DisabledSkills: []string{"jq", "pdf"}}}}
	require.Equal(t, []string{"jq", "pdf"}, store.EffectiveDisabledSkills())

	store.SetLocalSkillOverrides([]string{"docx"}, []string{"jq"})
	require.Equal(t, []string{"docx", "pdf"}, store.EffectiveDisabledSkills())
}
