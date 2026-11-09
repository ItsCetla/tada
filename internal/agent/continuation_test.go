package agent

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestIsSilentOverflow(t *testing.T) {
	t.Parallel()

	const cw, threshold = 100_000, 20_000
	empty := func(usage fantasy.Usage, content ...fantasy.Content) fantasy.StepResult {
		return fantasy.StepResult{Response: fantasy.Response{
			Content:      content,
			FinishReason: fantasy.FinishReasonLength,
			Usage:        usage,
		}}
	}
	for _, tc := range []struct {
		name      string
		step      fantasy.StepResult
		estimated int64
		want      bool
	}{
		{"reported prompt", empty(fantasy.Usage{InputTokens: 90_000}), 0, true},
		{"cached prompt", empty(fantasy.Usage{CacheReadTokens: 90_000}), 0, true},
		{"no usage, small prompt", empty(fantasy.Usage{}), 10_000, false},
		{"no usage, full prompt", empty(fantasy.Usage{}), 85_000, true},
		{"no usage, unknown window", empty(fantasy.Usage{}), 0, false},
		{"reasoning only", empty(fantasy.Usage{InputTokens: 90_000}, fantasy.ReasoningContent{Text: "hmm"}), 0, false},
		{"text", empty(fantasy.Usage{InputTokens: 90_000}, fantasy.TextContent{Text: "hi"}), 0, false},
		{"output tokens", empty(fantasy.Usage{InputTokens: 90_000, OutputTokens: 1}), 0, false},
		{"normal stop", fantasy.StepResult{Response: fantasy.Response{
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 90_000},
		}}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			window := int64(cw)
			if tc.name == "no usage, unknown window" {
				window = 0
			}
			require.Equal(t, tc.want, isSilentOverflow(tc.step, tc.estimated, window, threshold))
		})
	}
}

func TestContinuationPromptDoesNotNest(t *testing.T) {
	t.Parallel()

	once := continuationPrompt(interruptedPromptPrefix, "fix the bug")
	require.Equal(t, interruptedPromptPrefix+"`fix the bug`", once)
	require.Equal(t, once, continuationPrompt(interruptedPromptPrefix, once))
	require.Equal(t, once, continuationPrompt(overflowRetryPromptPrefix, once))

	retry := continuationPrompt(overflowRetryPromptPrefix, "fix the bug")
	require.Equal(t, retry, continuationPrompt(interruptedPromptPrefix, retry))
	require.Equal(t, 1, strings.Count(retry, "fix the bug"))
}

func TestHasUserTextMessageIgnoresHidden(t *testing.T) {
	t.Parallel()

	hidden := message.Message{Role: message.User, Parts: []message.ContentPart{
		message.TextContent{Text: "git status", Hidden: true},
	}}
	require.False(t, hasUserTextMessage([]message.Message{hidden}))

	visible := message.Message{Role: message.User, Parts: []message.ContentPart{
		message.TextContent{Text: "hello"},
	}}
	require.True(t, hasUserTextMessage([]message.Message{hidden, visible}))
}

func TestTodoReminderNeedsTodosTool(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := &finishStreamModel{text: "done"}
	msgs := []message.Message{{Role: message.User, Parts: []message.ContentPart{
		message.TextContent{Text: "hello"},
	}}}

	without := testSessionAgent(env, model, model, "system").(*sessionAgent)
	history, _ := without.preparePrompt(msgs, false)
	require.Len(t, history, 1)

	with := testSessionAgent(env, model, model, "system", &fakeTool{name: tools.TodosToolName}).(*sessionAgent)
	history, _ = with.preparePrompt(msgs, false)
	require.Len(t, history, 2)
	text, ok := fantasy.AsMessagePart[fantasy.TextPart](history[0].Content[0])
	require.True(t, ok)
	require.Contains(t, text.Text, "todo list is currently empty")
}
