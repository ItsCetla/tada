package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"

	"github.com/stretchr/testify/require"
)

// TestGenerateTitle_ClaudeCodeUsesPrompt verifies that a Claude Code
// session is titled from its first prompt without asking the small
// model, so the prompt never goes to another provider for a title.
func TestGenerateTitle_ClaudeCodeUsesPrompt(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	a := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	// The small model has no LanguageModel: calling it would panic.
	a.SetModels(Model{ClaudeCode: true}, Model{})

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	a.GenerateTitle(t.Context(), sess.ID, "  fix the\nlogin   bug  ")

	got, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "fix the login bug", got.Title)
}

func TestPromptTitle(t *testing.T) {
	t.Parallel()

	require.Empty(t, promptTitle(" \n "))
	require.Equal(t, "a b", promptTitle("a\n\tb"))
	long := promptTitle("word 0123456789 0123456789 0123456789 0123456789 0123456789")
	require.LessOrEqual(t, len([]rune(long)), 50)
	require.Contains(t, long, "…")
}

// thinkingTitleModel streams like a local reasoning model whose config
// doesn't say it reasons: it thinks for thinkTokens before it answers,
// so a smaller output budget ends mid-thought with no title text.
type thinkingTitleModel struct {
	finishStreamModel
	thinkTokens int64
	budgets     []int64
}

func (m *thinkingTitleModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	budget := *call.MaxOutputTokens
	m.budgets = append(m.budgets, budget)
	return func(yield func(fantasy.StreamPart) bool) {
		parts := []fantasy.StreamPart{
			{Type: fantasy.StreamPartTypeReasoningStart, ID: "r"},
			{Type: fantasy.StreamPartTypeReasoningDelta, ID: "r", Delta: "We need a title for the user."},
			{Type: fantasy.StreamPartTypeReasoningEnd, ID: "r"},
		}
		finish := fantasy.FinishReasonLength
		if budget > m.thinkTokens {
			parts = append(parts,
				fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t"},
				fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: "Slug package with tests"},
				fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t"},
			)
			finish = fantasy.FinishReasonStop
		}
		parts = append(parts, fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: finish})
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}, nil
}

// TestGenerateTitle_RetriesWhenReasoningEatsBudget verifies that a model
// which thinks without being configured as a reasoning model still gets
// a generated title: the short budget runs out mid-thought, so the
// title is retried on the same model with room to finish.
func TestGenerateTitle_RetriesWhenReasoningEatsBudget(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := &thinkingTitleModel{thinkTokens: 300}
	a := testSessionAgent(env, model, model, "system").(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	a.GenerateTitle(t.Context(), sess.ID, "create package slug with Slugify and tests")

	got, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "Slug package with tests", got.Title)
	// One short attempt, then one retry on the same (small) model.
	require.Equal(t, []int64{40, 10000}, model.budgets)
}

func TestThoughtWithoutAnswer(t *testing.T) {
	t.Parallel()

	result := func(reason fantasy.FinishReason, content ...fantasy.Content) *fantasy.AgentResult {
		return &fantasy.AgentResult{Response: fantasy.Response{Content: content, FinishReason: reason}}
	}
	reasoning := fantasy.ReasoningContent{Text: "thinking"}

	require.True(t, thoughtWithoutAnswer(result(fantasy.FinishReasonLength, reasoning)))
	require.True(t, thoughtWithoutAnswer(result(fantasy.FinishReasonLength, fantasy.TextContent{Text: "<think>still going"})))
	require.False(t, thoughtWithoutAnswer(result(fantasy.FinishReasonStop, reasoning)))
	require.False(t, thoughtWithoutAnswer(result(fantasy.FinishReasonLength, fantasy.TextContent{Text: "a long rambling title"})))
	require.False(t, thoughtWithoutAnswer(result(fantasy.FinishReasonLength, reasoning, fantasy.TextContent{Text: "Partial title"})))
	require.False(t, thoughtWithoutAnswer(nil))
}
