package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

const testSummaryBody = "## Goal\nFix the parser.\n\n## Progress\n- Read lexer.go\n\n## Next Steps\n- Patch tokenizer"

func newSummaryItem(t *testing.T, finished bool) *AssistantMessageItem {
	t.Helper()
	sty := styles.CharmtonePantera()
	parts := []message.ContentPart{message.TextContent{Text: testSummaryBody}}
	if finished {
		parts = append(parts, message.Finish{Reason: message.FinishReasonEndTurn, Time: testFinishTime})
	}
	msg := &message.Message{
		ID:               "summary-1",
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts:            parts,
	}
	return NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
}

// TestSummaryRendersCollapsed asserts a finished compaction summary
// renders as one muted line, and that toggling reveals the full body.
func TestSummaryRendersCollapsed(t *testing.T) {
	t.Parallel()
	item := newSummaryItem(t, true)

	out := ansi.Strip(item.Render(80))
	require.Equal(t, 1, strings.Count(out, "\n")+1, "collapsed summary must be one line")
	require.Contains(t, out, "Context summarized")
	require.Contains(t, out, "(8 lines hidden) [click or space to expand]")
	require.NotContains(t, out, "Fix the parser.")

	require.True(t, item.HandleMouseClick(ansi.MouseLeft, 0, 0),
		"collapsed summary must be clickable")
	require.True(t, item.ToggleExpanded())
	out = ansi.Strip(item.Render(80))
	require.Contains(t, out, "Fix the parser.")
	require.NotContains(t, out, "Context summarized")

	require.False(t, item.ToggleExpanded())
	require.Contains(t, ansi.Strip(item.Render(80)), "Context summarized")
}

// TestSummaryStreamingNotCollapsed asserts the summary stays visible
// while it streams; only a finished summary folds.
func TestSummaryStreamingNotCollapsed(t *testing.T) {
	t.Parallel()
	item := newSummaryItem(t, false)
	out := ansi.Strip(item.Render(80))
	require.NotContains(t, out, "Context summarized")
	require.Contains(t, out, "Fix the parser.")
}

// TestJSONOnlyReplyRendersAsCodeBlock asserts a reply that is only a
// JSON object is pretty-printed in a json code block.
func TestJSONOnlyReplyRendersAsCodeBlock(t *testing.T) {
	t.Parallel()
	require.Equal(t,
		"```json\n{\n  \"command\": \"grep -rn foo .\",\n  \"description\": \"Find foo\"\n}\n```",
		jsonAsCodeBlock("  {\"command\": \"grep -rn foo .\", \"description\": \"Find foo\"}\n"))

	for _, in := range []string{
		"Here is a call: {\"a\": 1}",
		"{not json}",
		"[1, 2]",
		"plain text",
	} {
		require.Equal(t, in, jsonAsCodeBlock(in), in)
	}

	sty := styles.CharmtonePantera()
	msg := &message.Message{
		ID:   "json-1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: `{"command": "grep -rn foo .", "description": "Find foo"}`},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: testFinishTime},
		},
	}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
	out := ansi.Strip(item.Render(80))
	require.Contains(t, out, `"command": "grep -rn foo ."`)
	require.Contains(t, out, `"description": "Find foo"`)
	require.Greater(t, strings.Count(out, "\n"), 2, "pretty-printed JSON spans lines")
}

// TestSummaryClickOnThinkingTogglesThinking asserts a click on the
// thinking box of a summary toggles the thinking, while a click below
// it (or space) toggles the summary body.
func TestSummaryClickOnThinkingTogglesThinking(t *testing.T) {
	t.Parallel()
	sty := styles.CharmtonePantera()
	msg := &message.Message{
		ID:               "summary-2",
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "thinking about the summary"},
			message.TextContent{Text: testSummaryBody},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: testFinishTime},
		},
	}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
	item.Render(80)
	require.Positive(t, item.thinkingBoxHeight)

	require.True(t, item.HandleMouseClick(ansi.MouseLeft, 0, 0))
	require.True(t, item.ToggleExpanded())
	require.NotEqual(t, thinkingCollapsed, item.thinkingViewMode)
	require.False(t, item.summaryExpanded, "thinking click must not unfold the summary")

	item.Render(80)
	require.True(t, item.HandleMouseClick(ansi.MouseLeft, 0, item.thinkingBoxHeight+1))
	require.True(t, item.ToggleExpanded())
	require.True(t, item.summaryExpanded, "click below thinking must unfold the summary")

	// Space has no position and toggles the summary.
	mode := item.thinkingViewMode
	require.False(t, item.ToggleExpanded())
	require.Equal(t, mode, item.thinkingViewMode)
}
