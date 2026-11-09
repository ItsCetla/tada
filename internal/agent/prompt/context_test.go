package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestContextFilesKeepConfiguredOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	names := []string{"Z.md", "A.md", "M.md", "B.md", "Y.md", "C.md"}
	for _, n := range names {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644))
	}
	store := config.NewTestStore(&config.Config{Options: &config.Options{
		ContextPaths:   names,
		DisabledSkills: []string{},
	}})
	p, err := NewPrompt("t", "{{range .ContextFiles}}{{.Content}},{{end}}", WithWorkingDir(dir))
	require.NoError(t, err)

	want := strings.Join(names, ",") + ","
	for range 5 {
		got, err := p.Build(t.Context(), "p", "m", store)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func TestContextFilesDedupeCaseInsensitive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("rules"), 0o644))
	store := config.NewTestStore(&config.Config{Options: &config.Options{
		ContextPaths:   []string{"AGENTS.md", "agents.MD", "AGENTS.md"},
		DisabledSkills: []string{},
	}})
	p, err := NewPrompt("t", "{{range .ContextFiles}}[{{.Content}}]{{end}}", WithWorkingDir(dir))
	require.NoError(t, err)

	got, err := p.Build(t.Context(), "p", "m", store)
	require.NoError(t, err)
	require.Equal(t, "[rules]", got)
}

func TestProcessFileTruncatesLargeFiles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", maxContextFileBytes+100)), 0o644))

	cf := processFile(path)
	require.NotNil(t, cf)
	marker := "\n\n[Ta-Da: truncated " + path + " after 65536 bytes]"
	require.True(t, strings.HasSuffix(cf.Content, marker))
	require.Equal(t, strings.Repeat("x", maxContextFileBytes), strings.TrimSuffix(cf.Content, marker))

	small := filepath.Join(t.TempDir(), "small.md")
	require.NoError(t, os.WriteFile(small, []byte("short"), 0o644))
	require.Equal(t, "short", processFile(small).Content)
}

func TestUnreferencedFieldsAreSkipped(t *testing.T) {
	t.Parallel()
	repo := newRepo(t, "feature")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("rules"), 0o644))
	store := config.NewTestStore(&config.Config{Options: &config.Options{
		ContextPaths:       []string{"AGENTS.md"},
		GlobalContextPaths: []string{"AGENTS.md"},
		DisabledSkills:     []string{},
	}})

	// A template that never reads context files gets none.
	p, err := NewPrompt("t", "{{.IsGitRepo}}", WithWorkingDir(repo))
	require.NoError(t, err)
	d, err := p.promptData(t.Context(), "p", "m", store)
	require.NoError(t, err)
	require.True(t, d.IsGitRepo)
	require.Empty(t, d.ContextFiles)
	require.Empty(t, d.GlobalContextFiles)

	// A template that reads them still gets them.
	p, err = NewPrompt("t", "{{.ContextFiles}}{{.GlobalContextFiles}}", WithWorkingDir(repo))
	require.NoError(t, err)
	d, err = p.promptData(t.Context(), "p", "m", store)
	require.NoError(t, err)
	require.Len(t, d.ContextFiles, 1)
	require.Len(t, d.GlobalContextFiles, 1)
}

func TestGitStatusOutsideRepo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewTestStore(&config.Config{Options: &config.Options{DisabledSkills: []string{}}})
	for _, tpl := range []string{"x", "[{{.Worktree}}]"} {
		p, err := NewPrompt("t", tpl, WithWorkingDir(dir))
		require.NoError(t, err)
		d, err := p.promptData(t.Context(), "p", "m", store)
		require.NoError(t, err)
		require.False(t, d.IsGitRepo)
		require.Empty(t, d.Worktree)
	}
	require.Empty(t, GitStatusSnapshot(t.Context(), dir))
}

func TestGitStatusSnapshot(t *testing.T) {
	t.Parallel()
	repo := newRepo(t, "feature")
	got := GitStatusSnapshot(t.Context(), repo)
	require.True(t, strings.HasPrefix(got, "<git_status snapshot=\"conversation start\">\n"), got)
	require.True(t, strings.HasSuffix(got, "\n</git_status>"), got)
	require.Contains(t, got, "Current branch: feature")
}
