package agent

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// cutMsg builds a message of roughly tokens estimated tokens.
func cutMsg(role message.MessageRole, at int64, tokens int, parts ...message.ContentPart) message.Message {
	if tokens > 0 {
		parts = append([]message.ContentPart{message.TextContent{Text: strings.Repeat("a", tokens*4)}}, parts...)
	}
	return message.Message{Role: role, CreatedAt: at, Parts: parts}
}

func TestCompactionCutPoint(t *testing.T) {
	t.Parallel()

	call := func(id string) message.ToolCall {
		return message.ToolCall{ID: id, Name: "view", Input: `{"file_path":"a.go"}`, Finished: true}
	}
	result := func(id string, tokens int) message.ToolResult {
		return message.ToolResult{ToolCallID: id, Name: "view", Content: strings.Repeat("r", tokens*4)}
	}

	for _, tc := range []struct {
		name       string
		msgs       []message.Message
		keepRecent int64
		// maxKept caps the kept estimate; 0 means no cap.
		maxKept   int64
		skipFirst bool
		want      int
	}{
		{
			name: "cuts at a user turn",
			msgs: []message.Message{
				cutMsg(message.User, 1, 1000),
				cutMsg(message.Assistant, 2, 1000),
				cutMsg(message.User, 3, 1000),
				cutMsg(message.Assistant, 4, 1000),
				cutMsg(message.Assistant, 5, 1000),
			},
			keepRecent: 2500,
			want:       2,
		},
		{
			name: "prefers the next user turn over an assistant",
			msgs: []message.Message{
				cutMsg(message.User, 1, 1000),
				cutMsg(message.Assistant, 2, 1000),
				cutMsg(message.Assistant, 3, 1000),
				cutMsg(message.User, 4, 1000),
				cutMsg(message.Assistant, 5, 1000),
			},
			keepRecent: 2500,
			want:       3,
		},
		{
			name: "falls back to an assistant message",
			msgs: []message.Message{
				cutMsg(message.User, 1, 1000),
				cutMsg(message.Assistant, 2, 1000),
				cutMsg(message.Assistant, 3, 1000),
				cutMsg(message.Assistant, 4, 1000),
			},
			keepRecent: 1500,
			want:       2,
		},
		{
			name: "never starts at a tool result",
			msgs: []message.Message{
				cutMsg(message.User, 1, 100),
				cutMsg(message.Assistant, 2, 10, call("c1")),
				cutMsg(message.Tool, 3, 0, result("c1", 3000)),
				cutMsg(message.Assistant, 4, 100),
			},
			keepRecent: 2000,
			want:       3,
		},
		{
			name: "keeps a call with a result that comes later",
			msgs: []message.Message{
				cutMsg(message.User, 1, 100),
				cutMsg(message.Assistant, 2, 10, call("c1")),
				cutMsg(message.User, 3, 1000),
				cutMsg(message.Tool, 4, 0, result("c1", 10)),
				cutMsg(message.Assistant, 5, 1000),
			},
			keepRecent: 1500,
			want:       4,
		},
		{
			name: "cuts within a second",
			msgs: []message.Message{
				cutMsg(message.User, 1, 1000),
				cutMsg(message.Assistant, 2, 1000),
				cutMsg(message.User, 2, 1000),
				cutMsg(message.Assistant, 3, 1000),
			},
			keepRecent: 1500,
			want:       2,
		},
		{
			name: "keeps the newer half when everything fits",
			msgs: []message.Message{
				cutMsg(message.User, 1, 1000),
				cutMsg(message.Assistant, 2, 1000),
				cutMsg(message.User, 3, 1000),
				cutMsg(message.Assistant, 4, 1000),
			},
			keepRecent: 100_000,
			want:       2,
		},
		{
			name: "the previous summary is not summarized alone",
			msgs: []message.Message{
				cutMsg(message.User, 1, 1000),
				cutMsg(message.User, 2, 1000),
			},
			keepRecent: 100_000,
			skipFirst:  true,
			want:       2,
		},
		{
			name: "summarizes the summary with what follows it",
			msgs: []message.Message{
				cutMsg(message.User, 1, 1000),
				cutMsg(message.User, 2, 1000),
				cutMsg(message.Assistant, 3, 1000),
				cutMsg(message.User, 4, 1000),
			},
			keepRecent: 1500,
			skipFirst:  true,
			want:       3,
		},
		{
			name: "no valid cut summarizes everything",
			msgs: []message.Message{
				cutMsg(message.User, 1, 1000),
				cutMsg(message.Assistant, 2, 10, call("c1")),
				cutMsg(message.Tool, 3, 0, result("c1", 1000)),
			},
			keepRecent: 500,
			want:       3,
		},
		{
			name: "shrinks the kept tail to the budget",
			msgs: []message.Message{
				cutMsg(message.User, 1, 1000),
				cutMsg(message.Assistant, 2, 1000),
				cutMsg(message.User, 3, 1000),
				cutMsg(message.Assistant, 4, 1000),
			},
			keepRecent: 2500,
			maxKept:    1500,
			want:       3,
		},
		{
			name: "no budget keeps nothing",
			msgs: []message.Message{
				cutMsg(message.User, 1, 1000),
				cutMsg(message.Assistant, 2, 1000),
				cutMsg(message.User, 3, 1000),
			},
			keepRecent: 1500,
			maxKept:    -1,
			want:       3,
		},
		{
			name:       "keep nothing",
			msgs:       []message.Message{cutMsg(message.User, 1, 10), cutMsg(message.User, 2, 10)},
			keepRecent: 0,
			want:       2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			maxKept := tc.maxKept
			if maxKept == 0 {
				maxKept = math.MaxInt64
			}
			require.Equal(t, tc.want, compactionCutPoint(tc.msgs, tc.keepRecent, maxKept, tc.skipFirst))
		})
	}
}

// TestCompactionLeavesHeadroom checks that the prompt right after a
// summary (system prompt and tools, the summary, the kept tail) leaves
// the summarize threshold plus a quarter of the window free, so a small
// window isn't summarized again a step or two later.
func TestCompactionLeavesHeadroom(t *testing.T) {
	t.Parallel()

	var msgs []message.Message
	for i := range 60 {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		msgs = append(msgs, cutMsg(role, int64(i), 1000))
	}
	const fixed = 3500

	for _, cw := range []int64{32_768, 65_536, 131_072, 200_000} {
		for _, maxOut := range []int64{8192, 16_384} {
			t.Run(fmt.Sprintf("%d/%d", cw, maxOut), func(t *testing.T) {
				t.Parallel()
				threshold := summarizeThreshold(cw, maxOut, defaultCompactionReserve(cw))
				maxKept := compactionKeepBudget(cw, threshold, fixed, summaryTokenEstimate)
				keep := compactionKeepRecent(defaultCompactionKeepRecentTokens, cw)
				cut := compactionCutPoint(msgs, keep, maxKept, false)
				require.Less(t, cut, len(msgs), "something should be kept")
				var kept int64
				for _, m := range msgs[cut:] {
					kept += estimateMessageTokens(m.ToAIMessage())
				}
				prompt := int64(fixed) + summaryTokenEstimate + kept
				require.GreaterOrEqual(t, cw-prompt, threshold+cw/4)
			})
		}
	}

	t.Run("a huge system prompt keeps nothing", func(t *testing.T) {
		t.Parallel()
		cw := int64(32_768)
		threshold := summarizeThreshold(cw, 16_384, defaultCompactionReserve(cw))
		maxKept := compactionKeepBudget(cw, threshold, 12_000, summaryTokenEstimate)
		require.Negative(t, maxKept)
		cut := compactionCutPoint(msgs, compactionKeepRecent(defaultCompactionKeepRecentTokens, cw), maxKept, false)
		require.Equal(t, len(msgs), cut)
	})

	t.Run("an unknown window doesn't cap", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, int64(math.MaxInt64), compactionKeepBudget(0, 0, 3500, summaryTokenEstimate))
	})
}

func TestCompactionBackdates(t *testing.T) {
	t.Parallel()

	older := []message.Message{
		cutMsg(message.User, 5, 1),
		cutMsg(message.Assistant, 8, 1),
		cutMsg(message.User, 9, 1),
		cutMsg(message.Assistant, 10, 1),
	}
	require.Equal(t, []int{2, 3}, compactionBackdates(older, 10))
	require.Empty(t, compactionBackdates(older, 20))
	require.Empty(t, compactionBackdates(nil, 10))
}

func TestCompactionKeepRecent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		keep, cw, want int64
	}{
		{"large window", 20_000, 200_000, 20_000},
		{"small window", 20_000, 32_768, 8192},
		{"unknown window", 20_000, 0, 20_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, compactionKeepRecent(tc.keep, tc.cw))
		})
	}
}

func TestSummaryFileLists(t *testing.T) {
	t.Parallel()

	tool := func(name, input string) message.ToolCall {
		return message.ToolCall{ID: name + input, Name: name, Input: input, Finished: true}
	}
	msgs := []message.Message{
		cutMsg(message.Assistant, 1, 0,
			tool("view", `{"file_path":"/a.go"}`),
			tool("view", `{"file_path":"/b.go"}`),
			tool("edit", `{"file_path":"/b.go","old_string":"x","new_string":"y"}`),
			tool("bash", `{"command":"cat /c.go"}`),
			tool("view", `not json`),
		),
		cutMsg(message.Assistant, 2, 0,
			tool("write", `{"file_path":"/d.go","content":""}`),
			tool("multiedit", `{"file_path":"/e.go","edits":[]}`),
			tool("view", `{"file_path":"/a.go"}`),
		),
	}

	for _, tc := range []struct {
		name         string
		msgs         []message.Message
		prev         string
		wantRead     []string
		wantModified []string
	}{
		{
			name:         "from tool calls",
			msgs:         msgs,
			wantRead:     []string{"/a.go"},
			wantModified: []string{"/b.go", "/d.go", "/e.go"},
		},
		{
			name:         "carries the previous summary forward",
			msgs:         msgs,
			prev:         "## Goal\nx" + formatSummaryFileLists([]string{"/old.go", "/d.go"}, []string{"/z.go"}),
			wantRead:     []string{"/a.go", "/old.go"},
			wantModified: []string{"/b.go", "/d.go", "/e.go", "/z.go"},
		},
		{
			name: "nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			read, modified := summaryFileLists(tc.msgs, tc.prev)
			require.Equal(t, tc.wantRead, read)
			require.Equal(t, tc.wantModified, modified)
		})
	}
}

func TestSummaryFileListsRoundTrip(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		read, modified []string
		want           string
	}{
		{"both", []string{"/a"}, []string{"/b", "/c"}, "\n\n" + summaryFileListsMarker + "\n\nFiles read:\n- /a\n\nFiles modified:\n- /b\n- /c"},
		{"read only", []string{"/a"}, nil, "\n\n" + summaryFileListsMarker + "\n\nFiles read:\n- /a"},
		{"modified only", nil, []string{"/b"}, "\n\n" + summaryFileListsMarker + "\n\nFiles modified:\n- /b"},
		{"none", nil, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatSummaryFileLists(tc.read, tc.modified)
			require.Equal(t, tc.want, got)
			read, modified := parseSummaryFileLists("## Goal\n- not a file\n" + got)
			require.Equal(t, tc.read, read)
			require.Equal(t, tc.modified, modified)
		})
	}
}

func TestSummaryFileListsNeedTheMarker(t *testing.T) {
	t.Parallel()

	// A list the model wrote itself, without the marker, is not parsed.
	body := "## Goal\nx\n\nFiles read:\n- /model.go\n\nFiles modified:\n- /other.go"
	read, modified := parseSummaryFileLists(body)
	require.Nil(t, read)
	require.Nil(t, modified)

	// Only the block after the last marker counts.
	summary := body + formatSummaryFileLists([]string{"/a"}, []string{"/b"})
	read, modified = parseSummaryFileLists(summary)
	require.Equal(t, []string{"/a"}, read)
	require.Equal(t, []string{"/b"}, modified)
}

func TestStripSummaryFileLists(t *testing.T) {
	t.Parallel()

	body := "## Goal\nx\n\nFiles read:\n- /model.go"
	require.Equal(t, body, stripSummaryFileLists(body+formatSummaryFileLists([]string{"/a"}, []string{"/b"})))
	require.Equal(t, body, stripSummaryFileLists(body))
	require.Empty(t, stripSummaryFileLists(""))
}
