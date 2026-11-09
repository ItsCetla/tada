package agent

import (
	"testing"

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
