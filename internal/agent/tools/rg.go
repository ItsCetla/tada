package tools

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/proc"
)

var getRg = sync.OnceValue(func() string {
	if testing.Testing() {
		return ""
	}
	home, _ := os.UserHomeDir()
	path := findRg(exec.LookPath, os.Getenv, rgCandidates(home))
	if path == "" && log.Initialized() {
		slog.Warn("Ripgrep (rg) not found; grep and glob fall back to a slower search. Install ripgrep or set TADA_RG_PATH to its binary.")
	}
	return path
})

// rgEnvVar names the environment variable that points at an rg binary.
const rgEnvVar = "TADA_RG_PATH"

// findRg returns the first usable ripgrep binary: rg on $PATH, then the
// binary named by TADA_RG_PATH, then the first executable candidate.
// Candidates may be glob patterns; the lexically last match is used.
func findRg(lookPath func(string) (string, error), getenv func(string) string, candidates []string) string {
	if path, err := lookPath("rg"); err == nil {
		return path
	}
	if path := getenv(rgEnvVar); path != "" && isExecutableFile(path) {
		return path
	}
	for _, candidate := range candidates {
		matches, err := filepath.Glob(candidate)
		if err != nil {
			continue
		}
		for i := len(matches) - 1; i >= 0; i-- {
			if isExecutableFile(matches[i]) {
				return matches[i]
			}
		}
	}
	return ""
}

// rgCandidates lists well-known places ripgrep is installed when it is not
// on $PATH, such as copies bundled with editors and other coding agents.
func rgCandidates(home string) []string {
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	goos := runtime.GOOS
	if goos == "windows" {
		goos = "win32"
	}
	// Claude Code's npm package ships rg under vendor/ripgrep/<arch>-<os>.
	claudeVendor := filepath.Join("@anthropic-ai", "claude-code", "vendor", "ripgrep", arch+"-"+goos, "rg"+exe)

	candidates := []string{
		"/opt/homebrew/bin/rg",
		"/opt/homebrew/opt/ripgrep/bin/rg",
		"/usr/local/bin/rg",
		"/home/linuxbrew/.linuxbrew/bin/rg",
	}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".cargo", "bin", "rg"+exe),
			filepath.Join(home, ".local", "bin", "rg"+exe),
			filepath.Join(home, ".claude", "local", "node_modules", claudeVendor),
			filepath.Join(home, ".npm-global", "lib", "node_modules", claudeVendor),
		)
	}
	candidates = append(candidates,
		filepath.Join("/opt/homebrew/lib/node_modules", claudeVendor),
		filepath.Join("/usr/local/lib/node_modules", claudeVendor),
		"/Applications/Visual Studio Code.app/Contents/Resources/app/node_modules/@vscode/ripgrep/bin/rg",
		"/Applications/Cursor.app/Contents/Resources/app/node_modules/@vscode/ripgrep/bin/rg",
	)
	return candidates
}

// isExecutableFile reports whether path is a regular file that can be run.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0
}

func getRgCmd(ctx context.Context, globPattern string) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	// Note: we intentionally do not pass -L (follow symlinks). Following
	// symlinks lets rg escape the search root (into module caches, the nix
	// store, $HOME, etc.) and chase cycles, which pins all cores and can
	// hang. This keeps glob scoped to the tree it was pointed at, matching
	// the grep search command.
	args := []string{"--files", "--null"}
	if globPattern != "" {
		if !filepath.IsAbs(globPattern) && !strings.HasPrefix(globPattern, "/") {
			globPattern = "/" + globPattern
		}
		args = append(args, "--glob", globPattern)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	proc.HideConsole(cmd)
	return cmd
}

func getRgSearchCmd(ctx context.Context, pattern, path, include string) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	// Use -n to show line numbers, -0 for null separation to handle Windows paths
	args := []string{"--json", "-H", "-n", "-0", pattern}
	if include != "" {
		args = append(args, "--glob", include)
	}
	args = append(args, path)

	cmd := exec.CommandContext(ctx, name, args...)
	proc.HideConsole(cmd)
	return cmd
}
