package agent

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestSummarizeThresholdReservesOutput(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name             string
		cw, out, reserve int64
		want             int64
	}{
		// 256k window, 32k max_tokens: the reported overflow case. The
		// request needs room for the reply on top of the reserve.
		{"reserve plus reply", 262_144, 32_768, defaultCompactionReserveTokens, defaultCompactionReserveTokens + 32_768},
		{"no max_tokens", 262_144, 0, defaultCompactionReserveTokens, defaultCompactionReserveTokens},
		{"custom reserve", 262_144, 0, 40_000, 40_000},
		// A huge max_tokens or reserve is capped so it can't summarize
		// every step.
		{"capped reply", 32_768, 30_000, defaultCompactionReserveTokens, 16_384},
		{"capped reserve", 32_768, 0, 100_000, 16_384},
		{"negative inputs", 262_144, -1, -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, summarizeThreshold(tc.cw, tc.out, tc.reserve))
		})
	}
	// At 229k prompt tokens in a 256k window with a 32k reply, the
	// conversation must already be summarized.
	require.LessOrEqual(t, int64(262_144-229_377), summarizeThreshold(262_144, 32_768, defaultCompactionReserveTokens))
}

func TestMagicContextLimitReservesOutput(t *testing.T) {
	t.Parallel()

	require.Equal(t, 262_144-32_768, magicContextLimit(262_144, 32_768))
	require.Equal(t, 262_144, magicContextLimit(262_144, 0))
	require.Equal(t, 0, magicContextLimit(0, 32_768))
	// A reply over half the window is a config problem; don't shrink.
	require.Equal(t, 32_768, magicContextLimit(32_768, 20_000))
}

func TestDefaultCompactionReserve(t *testing.T) {
	t.Parallel()

	// Small windows reserve a fifth so a summary leaves room to work.
	require.Equal(t, int64(6553), defaultCompactionReserve(32_768))
	require.Equal(t, int64(defaultCompactionReserveTokens), defaultCompactionReserve(200_000))
	require.Equal(t, int64(defaultCompactionReserveTokens), defaultCompactionReserve(0))
}

func TestSummarizeFreesSpace(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		cw, history int64
		want        bool
	}{
		// First turn on a 32k model: the system prompt and tools trip
		// the threshold, but there is next to nothing to summarize.
		{"first turn, small window", 32_768, 1_000, false},
		// Right after a summary the history is about one summary.
		{"just summarized", 32_768, summaryTokenEstimate + 500, false},
		// A long turn full of tool output is still summarized.
		{"long history, small window", 32_768, 12_000, true},
		{"long history, large window", 200_000, 140_000, true},
		{"quarter window plus summary", 40_000, 10_000 + summaryTokenEstimate, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, summarizeFreesSpace(tc.cw, tc.history))
		})
	}
}

func TestConversationTokensSkipsSystemPrompt(t *testing.T) {
	t.Parallel()

	system := fantasy.NewSystemMessage(strings.Repeat("s", 80_000))
	user := fantasy.NewUserMessage(strings.Repeat("u", 400))
	reply := fantasy.Message{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: strings.Repeat("a", 400)}},
	}
	got := conversationTokens([]fantasy.Message{system, user}, []fantasy.StepResult{{Messages: []fantasy.Message{reply}}})
	require.Equal(t, estimateMessageTokens([]fantasy.Message{user, reply}), got)
	require.Less(t, got, int64(1_000))
}

// TestShouldAutoSummarizeSmallWindow walks a 32k window with a 20.5k
// system prompt and tools up one token of history at a time: the first
// turn must not summarize, and summarizing must start while a full reply
// still fits.
func TestShouldAutoSummarizeSmallWindow(t *testing.T) {
	t.Parallel()

	const cw, fixed, maxOut int64 = 32_768, 20_500, 4_096
	reserve := defaultCompactionReserve(cw)
	threshold := summarizeThreshold(cw, maxOut, reserve)
	reply := replyBudget(maxOut, reserve)
	require.Equal(t, maxOut, reply)

	// The first turn trips the threshold but has nothing to summarize.
	require.LessOrEqual(t, cw-(fixed+2_000), threshold)
	require.False(t, shouldAutoSummarize(cw, fixed+2_000, threshold, reply, 2_000))

	var fired int64
	for history := int64(0); fixed+history <= cw; history++ {
		if shouldAutoSummarize(cw, fixed+history, threshold, reply, history) {
			fired = fixed + history
			break
		}
	}
	require.NotZero(t, fired, "never summarized before the window filled")
	require.GreaterOrEqual(t, cw-fired, reply, "summarized only after a reply no longer fits")

	// With no max_tokens the reserve is the reply budget.
	require.Equal(t, reserve, replyBudget(0, reserve))
	// Below the threshold nothing summarizes, however long the history.
	require.False(t, shouldAutoSummarize(cw, cw-threshold-1, threshold, reply, 20_000))
}
