package claudecode_test

import (
	"context"
	"os"
	"testing"

	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/claudecode/fakecli"
	"github.com/stretchr/testify/require"
)

// inputTypes counts the initialize requests and user messages the fake
// read. Each process sends initialize once.
func inputTypes(t *testing.T, dir string) (inits, users int) {
	t.Helper()
	for _, m := range fakecli.Input(t, dir) {
		switch m["type"] {
		case "user":
			users++
		case "control_request":
			if m["request"].(map[string]any)["subtype"] == "initialize" {
				inits++
			}
		}
	}
	return inits, users
}

func TestPoolReusesProcess(t *testing.T) {
	t.Parallel()
	bin, dir := fakecli.Write(t, []string{
		`{"type":"system","subtype":"init","session_id":"s-1","model":"claude-fake"}`,
		`{"type":"result","subtype":"success","result":"one","session_id":"s-1"}`,
	}, []string{
		// Turn two: a permission prompt, then the result.
		`{"type":"control_request","request_id":"perm-2","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"tu-2"}}`,
	}, []string{
		`{"type":"result","subtype":"success","result":"two","session_id":"s-1"}`,
	})
	pool := claudecode.NewPool()
	defer pool.Close()
	opts := claudecode.TurnOptions{Binary: bin, WorkDir: dir, Model: "haiku", Prompt: "first", SessionID: "s-1", Env: os.Environ()}
	require.False(t, pool.Live(opts))

	var asked1, asked2 []string
	handler := func(asked *[]string) claudecode.Handler {
		return claudecode.Handler{CanUseTool: func(_ context.Context, req claudecode.PermissionRequest) claudecode.Decision {
			*asked = append(*asked, req.ToolUseID)
			return claudecode.Decision{Allow: true}
		}}
	}
	res, err := pool.RunTurn(t.Context(), opts, handler(&asked1))
	require.NoError(t, err)
	require.Equal(t, "one", res.Result.Result)
	require.True(t, pool.Live(opts))

	// Resume is per spawn, not part of the launch options.
	opts.Prompt, opts.Resume = "second", true
	require.True(t, pool.Live(opts))
	res, err = pool.RunTurn(t.Context(), opts, handler(&asked2))
	require.NoError(t, err)
	require.Equal(t, "two", res.Result.Result)
	// The CLI did not send system/init again; the process's is kept.
	require.Equal(t, "claude-fake", res.Model)
	require.Empty(t, asked1)
	require.Equal(t, []string{"tu-2"}, asked2)

	inits, users := inputTypes(t, dir)
	require.Equal(t, 1, inits, "one process served both turns")
	require.Equal(t, 2, users)
	in := fakecli.Input(t, dir)
	require.Equal(t, "second", in[len(in)-2]["message"].(map[string]any)["content"])
	require.Equal(t, "perm-2", in[len(in)-1]["response"].(map[string]any)["request_id"])
}

func TestPoolRespawnsWhenOptionsChange(t *testing.T) {
	t.Parallel()
	bin, dir := fakecli.Write(t, []string{
		`{"type":"result","subtype":"success","result":"ok","session_id":"s-1"}`,
	}, []string{
		`{"type":"result","subtype":"success","result":"never","session_id":"s-1"}`,
	})
	pool := claudecode.NewPool()
	defer pool.Close()
	opts := claudecode.TurnOptions{Binary: bin, WorkDir: dir, Model: "haiku", Prompt: "first", SessionID: "s-1", Env: os.Environ()}
	_, err := pool.RunTurn(t.Context(), opts, claudecode.Handler{})
	require.NoError(t, err)
	require.Contains(t, fakecli.Args(t, dir), "--model haiku --session-id s-1")

	opts.Model, opts.Prompt, opts.Resume = "opus", "second", true
	require.False(t, pool.Live(opts))
	res, err := pool.RunTurn(t.Context(), opts, claudecode.Handler{})
	require.NoError(t, err)
	// The new process ran the script from its first step.
	require.Equal(t, "ok", res.Result.Result)
	require.Contains(t, fakecli.Args(t, dir), "--model opus --resume s-1")
	inits, users := inputTypes(t, dir)
	require.Equal(t, 2, inits)
	require.Equal(t, 2, users)
}

func TestPoolReplacesExitedProcess(t *testing.T) {
	t.Parallel()
	// The fake exits after its only result, so the second turn finds the
	// pooled process gone and must start a new one.
	bin, dir := fakecli.Write(t, []string{
		`{"type":"result","subtype":"success","result":"ok","session_id":"s-1"}`,
	})
	pool := claudecode.NewPool()
	defer pool.Close()
	opts := claudecode.TurnOptions{Binary: bin, WorkDir: dir, Prompt: "first", SessionID: "s-1", Env: os.Environ()}
	_, err := pool.RunTurn(t.Context(), opts, claudecode.Handler{})
	require.NoError(t, err)
	opts.Prompt, opts.Resume = "second", true
	res, err := pool.RunTurn(t.Context(), opts, claudecode.Handler{})
	require.NoError(t, err)
	require.Equal(t, "ok", res.Result.Result)
	require.Contains(t, fakecli.Args(t, dir), "--resume s-1")
}

func TestPoolDropsCancelledProcess(t *testing.T) {
	t.Parallel()
	bin, dir := fakecli.Write(t, []string{
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"working"}]}}`,
	}, []string{
		`{"type":"result","subtype":"error_during_execution","is_error":true}`,
	}, []string{
		`{"type":"result","subtype":"success","result":"never"}`,
	})
	pool := claudecode.NewPool()
	defer pool.Close()
	opts := claudecode.TurnOptions{Binary: bin, WorkDir: dir, Prompt: "hi", SessionID: "s-1", Env: os.Environ()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := pool.RunTurn(ctx, opts, claudecode.Handler{OnEvent: func(ev claudecode.Event) error {
		if ev.Type == "assistant" {
			cancel()
		}
		return nil
	}})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, pool.Live(opts))
}
