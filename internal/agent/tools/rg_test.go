package tools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func notFound(string) (string, error) { return "", errors.New("not found") }

func noEnv(string) string { return "" }

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func writeFakeRg(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), mode))
}

func TestFindRg(t *testing.T) {
	t.Parallel()

	t.Run("prefers PATH", func(t *testing.T) {
		t.Parallel()
		lookPath := func(string) (string, error) { return "/on/path/rg", nil }
		require.Equal(t, "/on/path/rg", findRg(lookPath, noEnv, nil))
	})

	t.Run("honors env var", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rg := filepath.Join(dir, "custom-rg")
		writeFakeRg(t, rg, 0o755)
		getenv := func(key string) string {
			if key == rgEnvVar {
				return rg
			}
			return ""
		}
		require.Equal(t, rg, findRg(notFound, getenv, nil))
	})

	t.Run("ignores env var pointing nowhere", func(t *testing.T) {
		t.Parallel()
		getenv := func(string) string { return filepath.Join(t.TempDir(), "missing") }
		require.Empty(t, findRg(notFound, getenv, nil))
	})

	t.Run("first existing candidate wins", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		second := filepath.Join(dir, "b", "rg")
		third := filepath.Join(dir, "c", "rg")
		writeFakeRg(t, second, 0o755)
		writeFakeRg(t, third, 0o755)
		candidates := []string{filepath.Join(dir, "a", "rg"), second, third}
		require.Equal(t, second, findRg(notFound, noEnv, candidates))
	})

	t.Run("glob picks lexically last match", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFakeRg(t, filepath.Join(dir, "versions", "1.0.0", "rg"), 0o755)
		newest := filepath.Join(dir, "versions", "2.0.0", "rg")
		writeFakeRg(t, newest, 0o755)
		candidates := []string{filepath.Join(dir, "versions", "*", "rg")}
		require.Equal(t, newest, findRg(notFound, noEnv, candidates))
	})

	t.Run("skips non-executable files and directories", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("Windows has no executable bit")
		}
		dir := t.TempDir()
		plain := filepath.Join(dir, "plain", "rg")
		writeFakeRg(t, plain, 0o644)
		asDir := filepath.Join(dir, "dir", "rg")
		require.NoError(t, os.MkdirAll(asDir, 0o755))
		require.Empty(t, findRg(notFound, noEnv, []string{plain, asDir}))
	})
}

func TestRgCandidatesIncludeHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	candidates := rgCandidates(home)
	require.Contains(t, candidates, "/opt/homebrew/bin/rg")
	require.Contains(t, candidates, filepath.Join(home, ".cargo", "bin", "rg"+exeSuffix()))
}
