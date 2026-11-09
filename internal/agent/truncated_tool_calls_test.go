package agent

import (
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestTruncatedToolCallsOnlyOnLength(t *testing.T) {
	t.Parallel()

	content := fantasy.ResponseContent{
		fantasy.ToolCallContent{ToolCallID: "a", ToolName: "write", Input: `{"path":"x","conte`},
		fantasy.ToolCallContent{ToolCallID: "b", ToolName: "web_search", ProviderExecuted: true},
	}
	step := func(reason fantasy.FinishReason) fantasy.StepResult {
		return fantasy.StepResult{Response: fantasy.Response{Content: content, FinishReason: reason}}
	}

	require.Empty(t, truncatedToolCalls(step(fantasy.FinishReasonToolCalls)))
	require.Empty(t, truncatedToolCalls(step(fantasy.FinishReasonStop)))

	calls := truncatedToolCalls(step(fantasy.FinishReasonLength))
	require.Len(t, calls, 1)
	require.Equal(t, "a", calls[0].ToolCallID)
}

func TestFailTruncatedToolCallsRecordsErrorResult(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	sess, err := sessions.Create(t.Context(), "truncated")
	require.NoError(t, err)
	assistant, err := messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{ID: "a", Name: "write"}},
	})
	require.NoError(t, err)

	a := &sessionAgent{messages: messages}
	step := fantasy.StepResult{Response: fantasy.Response{
		FinishReason: fantasy.FinishReasonLength,
		Content: fantasy.ResponseContent{
			fantasy.ToolCallContent{ToolCallID: "a", ToolName: "write", Input: `{"path":"x","conte`},
		},
	}}
	require.NoError(t, a.failTruncatedToolCalls(t.Context(), &assistant, step))

	// The cut-off call is closed with valid JSON so history stays sendable.
	calls := assistant.ToolCalls()
	require.Len(t, calls, 1)
	require.True(t, calls[0].Finished)
	require.Equal(t, "{}", calls[0].Input)

	msgs, err := messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var results []message.ToolResult
	for _, m := range msgs {
		results = append(results, m.ToolResults()...)
	}
	require.Len(t, results, 1)
	require.Equal(t, "a", results[0].ToolCallID)
	require.True(t, results[0].IsError)
	require.Equal(t, truncatedToolCallError, results[0].Content)
}
