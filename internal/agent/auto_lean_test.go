package agent

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestShouldAutoLean(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		profile  string
		cw, full int64
		want     bool
	}{
		// The reported case: a 20.5k prompt on a 32k local model.
		{"small window", "", 32_768, 20_500, true},
		{"large window", "", 200_000, 20_500, false},
		{"exactly half", "", 41_000, 20_500, false},
		{"unknown window", "", 0, 20_500, false},
		{"explicit full", "full", 32_768, 20_500, false},
		{"explicit lean", config.ToolProfileLean, 32_768, 20_500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, shouldAutoLean(tc.profile, tc.cw, tc.full))
		})
	}
}

func TestEstimatePromptTokensCountsTools(t *testing.T) {
	t.Parallel()

	prompt := strings.Repeat("x", 4000)
	require.Equal(t, int64(1000), estimatePromptTokens(prompt, nil))
	withTool := estimatePromptTokens(prompt, []fantasy.AgentTool{&fakeTool{name: "bash"}})
	require.Greater(t, withTool, int64(1000))
}

// mcpFakeTool is a fakeTool that belongs to an MCP server.
type mcpFakeTool struct {
	fakeTool
	server string
}

func (m *mcpFakeTool) MCP() string { return m.server }

func TestLeanToolSet(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Options: &config.Options{EnabledTools: []string{"todos", "mcp_docs"}}}
	in := []fantasy.AgentTool{
		&fakeTool{name: "bash"},
		&fakeTool{name: "sourcegraph"},
		&fakeTool{name: "todos"},
		&fakeTool{name: "ctx_search"},
		&mcpFakeTool{fakeTool: fakeTool{name: "mcp_docs_search"}, server: "docs"},
		&mcpFakeTool{fakeTool: fakeTool{name: "mcp_github_list_issues"}, server: "github"},
		// A hooked MCP tool is still recognized as MCP.
		newHookedTool(&mcpFakeTool{fakeTool: fakeTool{name: "mcp_github_get_issue"}, server: "github"}, nil),
	}
	var names []string
	for _, tool := range leanToolSet(cfg, in) {
		names = append(names, tool.Info().Name)
	}
	require.Equal(t, []string{"bash", "todos", "ctx_search", "mcp_docs_search"}, names)
}

func TestStickyLeanKeepsFirstDecision(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{}
	require.True(t, a.stickyLean("lean", func() bool { return true }))
	// Later runs keep lean even if the prompt now looks small.
	require.True(t, a.stickyLean("lean", func() bool { return false }))

	require.False(t, a.stickyLean("full", func() bool { return false }))
	// A session that ran full never flips to lean, say once MCP servers
	// connect and the prompt grows.
	calls := 0
	require.False(t, a.stickyLean("full", func() bool { calls++; return true }))
	require.Zero(t, calls)
}
