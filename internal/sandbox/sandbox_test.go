//go:build !windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/stretchr/testify/require"
)

// fakeRepo makes dir look like a git folder with its own objects.
func fakeRepo(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "objects"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
}

func writeDotGit(t *testing.T, dir, target string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+target+"\n"), 0o644))
}

func canon(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepathext.Canonical(p)
	}
	return out
}

func TestGitDirs_DotGitFileToHomeNotWritable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeRepo(t, home)
	proj := filepath.Join(home, "proj")
	writeDotGit(t, proj, home)

	dirs, writable := gitDirs(proj)
	require.Equal(t, []string{home}, dirs)
	require.Empty(t, writable)

	// A folder above home is refused too.
	writeDotGit(t, proj, filepath.Dir(home))
	_, writable = gitDirs(proj)
	require.Empty(t, writable)

	// So is a home folder laid out as a worktree git folder that links
	// back to the project.
	home = filepath.Join(t.TempDir(), "worktrees", "h")
	t.Setenv("HOME", home)
	proj = filepath.Join(home, "proj")
	fakeRepo(t, home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "gitdir"), []byte(filepath.Join(proj, ".git")), 0o644))
	writeDotGit(t, proj, home)
	_, writable = gitDirs(proj)
	require.Empty(t, writable)
}

func TestGitDirs_SubmoduleNeedsRepoLayout(t *testing.T) {
	t.Parallel()
	top := t.TempDir()
	mod := filepath.Join(top, ".git", "modules", "sub")
	require.NoError(t, os.MkdirAll(mod, 0o755))
	proj := filepath.Join(top, "sub")
	writeDotGit(t, proj, "../.git/modules/sub")

	_, writable := gitDirs(proj)
	require.Empty(t, writable, "no HEAD or objects")

	fakeRepo(t, mod)
	_, writable = gitDirs(proj)
	require.Equal(t, []string{filepathext.Canonical(mod)}, canon(writable))

	// The same layout in another repo's .git/modules isn't trusted.
	other := filepath.Join(t.TempDir(), ".git", "modules", "sub")
	fakeRepo(t, other)
	writeDotGit(t, proj, other)
	_, writable = gitDirs(proj)
	require.Empty(t, writable)
}

func newWorktree(t *testing.T) (repo, wt string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	root := filepathext.Canonical(t.TempDir())
	repo = filepath.Join(root, "repo")
	wt = filepath.Join(root, "wt")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "a"},
		{"-C", repo, "worktree", "add", "-q", wt},
	} {
		out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	return repo, wt
}

func TestGitDirs_RealWorktreeWritable(t *testing.T) {
	t.Parallel()
	repo, wt := newWorktree(t)
	_, writable := gitDirs(wt)
	require.Equal(t, []string{
		filepath.Join(repo, ".git", "worktrees", "wt"),
		filepath.Join(repo, ".git"),
	}, canon(writable))
}

func TestGitDirs_WorktreeWithoutBacklinkNotWritable(t *testing.T) {
	t.Parallel()
	repo, wt := newWorktree(t)
	wtGit := filepath.Join(repo, ".git", "worktrees", "wt")
	require.NoError(t, os.WriteFile(filepath.Join(wtGit, "gitdir"), []byte(filepath.Join(t.TempDir(), ".git")+"\n"), 0o644))
	_, writable := gitDirs(wt)
	require.Empty(t, writable)
}

func TestGitDirs_CommondirOutsideLayoutNotWritable(t *testing.T) {
	t.Parallel()
	repo, wt := newWorktree(t)
	other := t.TempDir()
	fakeRepo(t, other)
	wtGit := filepath.Join(repo, ".git", "worktrees", "wt")
	require.NoError(t, os.WriteFile(filepath.Join(wtGit, "commondir"), []byte(other+"\n"), 0o644))

	dirs, writable := gitDirs(wt)
	require.Contains(t, dirs, other, "still protected")
	require.Equal(t, []string{filepathext.Canonical(wtGit)}, canon(writable))

	// A commondir in a plain .git folder is never trusted, even one with
	// the right shape two levels up.
	fakeRepo(t, filepath.Dir(repo))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "commondir"), []byte("../..\n"), 0o644))
	_, writable = gitDirs(repo)
	require.Equal(t, []string{filepath.Join(repo, ".git")}, canon(writable))
}

func TestGitDirs_CommondirWithoutObjectsNotWritable(t *testing.T) {
	t.Parallel()
	common := filepathext.Canonical(t.TempDir())
	wtGit := filepath.Join(common, "worktrees", "w")
	proj := filepath.Join(t.TempDir(), "w")
	require.NoError(t, os.MkdirAll(wtGit, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wtGit, "HEAD"), []byte("x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wtGit, "gitdir"), []byte(filepath.Join(proj, ".git")+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o644))
	writeDotGit(t, proj, wtGit)

	_, writable := gitDirs(proj)
	require.Equal(t, []string{wtGit}, canon(writable))

	fakeRepo(t, common)
	_, writable = gitDirs(proj)
	require.Equal(t, []string{wtGit, common}, canon(writable))
}
