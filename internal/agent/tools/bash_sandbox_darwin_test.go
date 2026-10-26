//go:build darwin

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/sandbox"
	"github.com/stretchr/testify/require"
)

func newSandboxedBashTool(t *testing.T, settings sandbox.Settings) (string, func(BashParams) (string, int)) {
	t.Helper()
	if !sandbox.Available() {
		t.Skip("sandbox-exec is not available")
	}
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755))
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
	}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	tool := NewBashTool(perms, nil, dir, filepath.Join(dir, ".tada"), attribution, "test-model", settings)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")
	return dir, func(p BashParams) (string, int) {
		before := perms.requestCount
		resp := runBashTool(t, tool, ctx, p)
		return resp.Content, perms.requestCount - before
	}
}

// The sandbox is an extra layer: every mode still asks before running a
// command, just as it does with the sandbox off.
func TestBashTool_SandboxStillPrompts(t *testing.T) {
	t.Parallel()
	for _, settings := range []sandbox.Settings{
		{Mode: "off"},
		{Mode: "workspace-write"},
		{Mode: "workspace-write", Network: true},
		{Mode: "read-only"},
	} {
		dir, run := newSandboxedBashTool(t, settings)
		_, asked := run(BashParams{Command: "echo hi > f.txt"})
		require.Equal(t, 1, asked, settings.Mode)
		require.FileExists(t, filepath.Join(dir, "f.txt"), settings.Mode)
	}
}

func TestBashTool_SandboxReadOnlyBlocksWrites(t *testing.T) {
	t.Parallel()
	_, run := newSandboxedBashTool(t, sandbox.Settings{Mode: "read-only"})

	// /Library is root-owned, so without the sandbox the write fails with
	// "Permission denied" instead and creates nothing.
	out, asked := run(BashParams{Command: "/bin/sh -c 'echo hi > /Library/tada-sandbox-probe'"})
	require.Equal(t, 1, asked)
	require.Contains(t, out, "Operation not permitted")
}

func TestBashTool_SandboxKeepsBannedCommands(t *testing.T) {
	t.Parallel()
	_, run := newSandboxedBashTool(t, sandbox.Settings{Mode: "workspace-write"})
	out, asked := run(BashParams{Command: "curl https://example.com"})
	require.Zero(t, asked)
	require.Contains(t, out, "not allowed for security reasons")
}

func TestBashTool_SandboxNoteOnDenial(t *testing.T) {
	t.Parallel()
	dir, run := newSandboxedBashTool(t, sandbox.Settings{Mode: "workspace-write"})

	for _, command := range []string{
		"/bin/sh -c 'echo x > .git/hooks/pre-commit'",
		"echo x > .git/hooks/pre-commit",
	} {
		out, _ := run(BashParams{Command: command})
		require.Contains(t, out, sandboxNote, command)
		require.Contains(t, out, "the network is off", command)
		require.NoFileExists(t, filepath.Join(dir, ".git", "hooks", "pre-commit"))
	}

	out, _ := run(BashParams{Command: "echo fine"})
	require.NotContains(t, out, sandboxNote)
}
