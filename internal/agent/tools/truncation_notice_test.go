package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBashTruncationNotice(t *testing.T) {
	t.Parallel()

	require.Equal(t,
		"[Showing first and last 15000 chars of 42 lines. Full output: /tmp/out.txt. Use the grep or view tool on it; don't rerun the command.]",
		bashTruncationNotice(15000, 42, "/tmp/out.txt"))
	require.Equal(t,
		"[Showing first and last 15000 chars of 42 lines. The middle could not be saved; narrow the command's output with grep or a line range if you need it.]",
		bashTruncationNotice(15000, 42, ""))
}

func TestTruncateOutputNoticeNamesSpillFile(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("line of output\n", MaxOutputLength)
	out := TruncateOutput(content, t.TempDir())
	require.Contains(t, out, "[Showing first and last 15000 chars of")
	require.Contains(t, out, "Full output: ")
	require.Contains(t, out, "don't rerun the command.]")
	require.Equal(t, 1, strings.Count(out, "[Showing"), "exactly one notice")
}

func TestViewContinuationNotice(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "long.txt")
	lines := make([]string, 300)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runViewTool(t, tool, ctx, ViewParams{FilePath: filePath})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "[Showing lines 1-200 of 300. Use offset=200 to continue.]")

	// Following the notice must pick up at the next unseen line.
	resp = runViewTool(t, tool, ctx, ViewParams{FilePath: filePath, Offset: 200, Limit: 50})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "   201|line 201")
	require.Contains(t, resp.Content, "[Showing lines 201-250 of 300. Use offset=250 to continue.]")

	// Reading to the end leaves no notice.
	resp = runViewTool(t, tool, ctx, ViewParams{FilePath: filePath, Offset: 250, Limit: 100})
	require.NotContains(t, resp.Content, "[Showing")

	require.Equal(t, "[Showing lines 1-5; more follow. Use offset=5 to continue.]", viewContinuationNotice(1, 5, -1))
}

func TestViewOversizedSectionSuggestsLimit(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "wide.txt")
	lines := make([]string, DefaultReadLimit)
	for i := range lines {
		lines[i] = strings.Repeat("a", MaxLineLength)
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runViewTool(t, tool, ctx, ViewParams{FilePath: filePath})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Use a smaller limit (limit=")
	require.Contains(t, resp.Content, "with offset=0, or grep for what you need.")
}

func TestGrepTruncationNotice(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for i := range 150 {
		name := filepath.Join(root, fmt.Sprintf("f%03d.txt", i))
		require.NoError(t, os.WriteFile(name, []byte("needle\n"), 0o644))
	}

	matches, total, err := searchFilesWithTotal(context.Background(), "needle", root, "", 100)
	require.NoError(t, err)
	require.Len(t, matches, 100)
	require.Equal(t, 150, total)
	require.Equal(t,
		"[Showing first 100 of 150 matches. Narrow the pattern or path, or set include, to see the rest.]",
		grepTruncationNotice(len(matches), total))
}

func TestGlobTruncationNotice(t *testing.T) {
	t.Parallel()

	require.Equal(t,
		"[Showing first 100 matches; more exist. Narrow the pattern or path to see the rest.]",
		globTruncationNotice(100))
}

func TestLSTruncationNotice(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for i := range 20 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.txt", i)), nil, 0o644))
	}

	maxItems := 5
	out, meta, err := ListDirectoryTree(root, LSParams{Path: root}, config.ToolLs{MaxItems: &maxItems})
	require.NoError(t, err)
	require.True(t, meta.Truncated)
	require.True(t, strings.HasSuffix(strings.TrimSpace(out),
		"[Showing first 5 entries; more exist. List a subdirectory, add ignore patterns, or use glob to see the rest.]"))
}

func TestGrepByteCapNotice(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", maxGrepLineLength+100)
	matches := make([]grepMatch, maxGrepMatches)
	for i := range matches {
		matches[i] = grepMatch{path: fmt.Sprintf("f%03d.txt", i), lineNum: 1, lineText: long}
	}

	var out strings.Builder
	shown := writeGrepMatches(&out, matches, maxGrepOutputBytes)
	require.Less(t, shown, len(matches))
	require.Positive(t, shown)
	require.LessOrEqual(t, out.Len(), maxGrepOutputBytes)
	require.Contains(t, out.String(), lineTruncatedSuffix)
	require.NotContains(t, out.String(), strings.Repeat("x", maxGrepLineLength+1))
	require.Equal(t,
		fmt.Sprintf("[Showing first %d of 150 matches; output capped at 50KB. Narrow the pattern or path, or set include, to see the rest.]", shown),
		grepByteCapNotice(shown, 150, maxGrepOutputBytes))

	small := matches[:3]
	out.Reset()
	require.Equal(t, 3, writeGrepMatches(&out, small, maxGrepOutputBytes))
}

func TestTruncateLine(t *testing.T) {
	t.Parallel()

	require.Equal(t, "short", truncateLine("short", 10))
	require.Equal(t, "abc"+lineTruncatedSuffix, truncateLine("abcdef", 3))
	// A cut inside a multi-byte rune drops the partial rune.
	require.Equal(t, "a"+lineTruncatedSuffix, truncateLine("aéé", 2))
}

func TestToolDescriptionsTakeLimitsFromConstants(t *testing.T) {
	t.Parallel()

	view := viewDescription()
	require.Contains(t, view, fmt.Sprint(MaxLineLength))
	grep := grepDescription()
	require.Contains(t, grep, fmt.Sprint(maxGrepMatches))
	require.Contains(t, grep, fmt.Sprint(maxGrepLineLength))
	require.Contains(t, grep, fmt.Sprintf("%dKB", maxGrepOutputBytes/1024))
	require.Contains(t, globDescription(), fmt.Sprint(maxGlobResults))
	bash := bashDescription(&config.Attribution{}, "")
	require.Contains(t, bash, fmt.Sprintf("default %d", DefaultAutoBackgroundAfter))

	// The templates themselves carry no literal numbers.
	for _, name := range []string{"view.md.tpl", "grep.md.tpl", "glob.md.tpl", "ls.md.tpl", "bash.md.tpl"} {
		raw, err := os.ReadFile(name)
		require.NoError(t, err)
		outside := regexp.MustCompile(`\{\{[^}]*\}\}`).ReplaceAllString(string(raw), "")
		require.NotRegexp(t, `\b\d{2,}\b`, outside, name)
	}
}
