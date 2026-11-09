package agent

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSummaryInputMessages(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", summaryInputToolResultLimit+500)
	msgs := []message.Message{
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ReasoningContent{Thinking: "secret thoughts"},
				message.TextContent{Text: "hello"},
			},
		},
		{
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: "1", Content: long},
				message.ToolResult{ToolCallID: "2", Content: "short"},
			},
		},
	}

	got := summaryInputMessages(msgs)
	require.Len(t, got, 2)

	require.Len(t, got[0].Parts, 1)
	require.Equal(t, message.TextContent{Text: "hello"}, got[0].Parts[0])

	tr := got[1].Parts[0].(message.ToolResult)
	require.Equal(t, strings.Repeat("a", summaryInputToolResultLimit)+"\n[... 500 more chars omitted for summary]", tr.Content)
	require.Equal(t, "short", got[1].Parts[1].(message.ToolResult).Content)

	// The original messages are untouched.
	require.Len(t, msgs[0].Parts, 2)
	require.IsType(t, message.ReasoningContent{}, msgs[0].Parts[0])
	require.Equal(t, long, msgs[1].Parts[0].(message.ToolResult).Content)
}

func TestBuildSummaryPromptSections(t *testing.T) {
	t.Parallel()

	for _, h := range []string{"## Goal", "## Constraints & Preferences", "## Progress", "### Blocked", "## Key Decisions", "## Next Steps", "## Critical Context"} {
		require.Contains(t, summaryPrompt, h)
	}
	// File lists are tracked in code, not written by the model.
	require.NotContains(t, summaryPrompt, "Files Read")
	require.NotContains(t, buildSummaryPrompt(nil), "Files Modified")
	require.Contains(t, buildSummaryPrompt(nil), "Critical Context")
}

func TestSummaryInputMessagesStripsFileLists(t *testing.T) {
	t.Parallel()

	summary := "## Goal\nFix it." + formatSummaryFileLists([]string{"a.go"}, []string{"b.go"})
	msgs := []message.Message{
		{Role: message.Assistant, IsSummaryMessage: true, Parts: []message.ContentPart{message.TextContent{Text: summary}}},
		{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: summary}}},
	}
	out := summaryInputMessages(msgs)
	require.Equal(t, "## Goal\nFix it.", out[0].Content().Text)
	// Only a summary loses its lists, and msgs is left alone.
	require.Equal(t, summary, out[1].Content().Text)
	require.Equal(t, summary, msgs[0].Content().Text)
}

// TestPlaceSummarySameSecond covers a summary made while the summarized
// and kept messages share one created_at second: the summary must still
// load with exactly the kept messages after it.
func TestPlaceSummarySameSecond(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)
	a := &sessionAgent{messages: messages}

	sess, err := sessions.Create(t.Context(), "same second")
	require.NoError(t, err)
	var msgs []message.Message
	for i, role := range []message.MessageRole{message.User, message.Assistant, message.User, message.Assistant} {
		m, err := messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  role,
			Parts: []message.ContentPart{message.TextContent{Text: string(rune('a' + i))}},
		})
		require.NoError(t, err)
		require.NoError(t, messages.SetCreatedAt(t.Context(), m.ID, 1000))
		m.CreatedAt = 1000
		msgs = append(msgs, m)
	}
	summary, err := messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:             message.Assistant,
		Parts:            []message.ContentPart{message.TextContent{Text: "summary"}},
		IsSummaryMessage: true,
	})
	require.NoError(t, err)

	require.NoError(t, a.placeSummary(t.Context(), summary.ID, msgs[:2], msgs[2:]))

	got, err := messages.ListFromSummary(t.Context(), sess.ID, summary.ID)
	require.NoError(t, err)
	ids := make([]string, len(got))
	for i, m := range got {
		ids[i] = m.ID
	}
	require.Equal(t, []string{summary.ID, msgs[2].ID, msgs[3].ID}, ids)
	for _, m := range msgs[:2] {
		older, err := messages.Get(t.Context(), m.ID)
		require.NoError(t, err)
		require.Less(t, older.CreatedAt, got[0].CreatedAt)
	}
}
