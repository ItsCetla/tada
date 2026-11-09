package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/fleet"
	"github.com/stretchr/testify/require"
)

// claudeCodeServers returns the server names in an --mcp-config= flag, and
// whether --strict-mcp-config was passed.
func claudeCodeServers(t *testing.T, args []string) (names []string, strict bool) {
	t.Helper()
	for _, a := range args {
		if a == "--strict-mcp-config" {
			strict = true
			continue
		}
		raw, ok := strings.CutPrefix(a, "--mcp-config=")
		require.True(t, ok, "unexpected arg %q", a)
		var mc struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &mc))
		require.NotNil(t, mc.MCPServers)
		for name := range mc.MCPServers {
			names = append(names, name)
		}
	}
	return names, strict
}

func TestClaudeCodeArgsMachinesServer(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	// Start from a known state, whatever the global config holds.
	cfg.Config().Machines = nil
	cfg.Config().Options.EnabledTools = nil
	cfg.Config().Options.Artifacts = &config.ArtifactsOptions{Disabled: true}
	coord := &coordinator{cfg: cfg}

	require.Empty(t, coord.claudeCodeArgs(false)("s1"), "no machines, no server")

	cfg.Config().Options.EnabledTools = []string{"machines"}
	names, _ := claudeCodeServers(t, coord.claudeCodeArgs(false)("s1"))
	require.Equal(t, []string{fleet.MCPServerName}, names, "enabled_tools turns it on")
	cfg.Config().Options.EnabledTools = nil

	cfg.Config().Machines = config.Machines{"m1": {}}
	names, _ = claudeCodeServers(t, coord.claudeCodeArgs(false)("s1"))
	require.Equal(t, []string{fleet.MCPServerName}, names)
	require.Empty(t, coord.claudeCodeArgs(true)("s1"), "sub-agents get no machines server")
}

func TestClaudeCodeArgsStrictMCP(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	// Start from a known state, whatever the global config holds.
	cfg.Config().Machines = nil
	opts := cfg.Config().Options
	opts.EnabledTools = nil
	opts.Artifacts = &config.ArtifactsOptions{Disabled: true}
	coord := &coordinator{cfg: cfg}

	opts.ClaudeCodeStrictMCP = true
	args := coord.claudeCodeArgs(false)("s1")
	require.Equal(t, []string{"--strict-mcp-config", `--mcp-config={"mcpServers":{}}`}, args)

	cfg.Config().Machines = config.Machines{"m1": {}}
	names, strict := claudeCodeServers(t, coord.claudeCodeArgs(false)("s1"))
	require.True(t, strict)
	require.Equal(t, []string{fleet.MCPServerName}, names)

	opts.ClaudeCodeStrictMCP = false
	names, strict = claudeCodeServers(t, coord.claudeCodeArgs(false)("s1"))
	require.False(t, strict)
	require.Equal(t, []string{fleet.MCPServerName}, names)
}
