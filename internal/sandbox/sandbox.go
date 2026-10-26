// Package sandbox confines the shell commands the agent runs. On macOS it
// wraps each spawned process in sandbox-exec with a generated profile; other
// platforms have no sandbox yet and run commands as before.
package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/charmbracelet/crush/internal/filepathext"
)

// Mode is how much a sandboxed command may write.
type Mode string

const (
	// ModeOff runs commands without a sandbox.
	ModeOff Mode = "off"
	// ModeReadOnly lets commands write only to temp folders.
	ModeReadOnly Mode = "read-only"
	// ModeWorkspaceWrite lets commands write to the project, temp folders
	// and a private Go build cache.
	ModeWorkspaceWrite Mode = "workspace-write"
)

// ParseMode turns a config value into a Mode. An empty value means off.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeOff:
		return ModeOff, nil
	case ModeReadOnly, ModeWorkspaceWrite:
		return Mode(s), nil
	}
	return ModeOff, fmt.Errorf("sandbox: unknown mode %q (want off, read-only or workspace-write)", s)
}

// Settings is the user's sandbox config.
type Settings struct {
	Mode    string
	Network bool
}

// Policy is what a sandboxed command may do. A nil Policy means no sandbox.
type Policy struct {
	Mode Mode
	// WritableRoots are canonical folders commands may write inside.
	WritableRoots []string
	// ProtectedPaths are canonical files and folders inside the writable
	// roots that stay read-only, along with everything below them.
	ProtectedPaths []string
	// ProtectedEntries are canonical paths that can't be written, removed
	// or renamed themselves, though what is inside them can. They keep the
	// .git folder in place so its protected hooks can't be swapped out.
	ProtectedEntries []string
	// Network allows network access beyond loopback.
	Network bool
	// GoCache is a Go build cache only sandboxed commands use, so what
	// they put in it can't reach builds run outside the sandbox. Shells
	// set GOCACHE to it.
	GoCache string
}

var warnOnce sync.Once

// New builds the policy for commands run in workingDir. It returns nil when
// the sandbox is off, and also when this platform has no sandbox, logging
// a warning the first time.
func New(s Settings, projectRoot, workingDir, dataDir string) *Policy {
	mode, err := ParseMode(s.Mode)
	if err != nil {
		slog.Warn("Ignoring invalid sandbox mode", "error", err)
		return nil
	}
	if mode == ModeOff {
		return nil
	}
	if !Available() {
		warnOnce.Do(func() {
			slog.Warn("Sandbox is not available on this platform; shell commands run unconfined", "mode", mode)
		})
		return nil
	}
	if projectRoot == "" {
		projectRoot = workingDir
	}

	p := &Policy{Mode: mode, Network: s.Network}
	p.WritableRoots = tempRoots()
	if mode == ModeWorkspaceWrite {
		p.WritableRoots = append(p.WritableRoots, projectRoot, workingDir)
	}
	if c := goCacheDir(projectRoot); c != "" {
		p.GoCache = c
		p.WritableRoots = append(p.WritableRoots, c)
	}

	var protected, entries []string
	for _, dir := range []string{projectRoot, workingDir} {
		for _, name := range protectedNames {
			protected = append(protected, filepath.Join(dir, name))
		}
		entries = append(entries, filepath.Join(dir, ".git"))
		gitDirs, writable := gitDirs(dir)
		if mode == ModeWorkspaceWrite {
			// Git folders that live outside the project, as with a linked
			// worktree, still need writing to for commits.
			p.WritableRoots = append(p.WritableRoots, writable...)
		}
		for _, gitDir := range gitDirs {
			entries = append(entries, gitDir)
			// gitMetaRE covers these too, but only below a folder named
			// .git, which a linked worktree's git folder may not be.
			for _, name := range gitMetaNames {
				protected = append(protected, filepath.Join(gitDir, name))
			}
		}
	}
	if dataDir != "" {
		protected = append(protected, dataDir)
	}
	p.WritableRoots = canonicalSet(p.WritableRoots)
	p.ProtectedPaths = canonicalSet(protected)
	p.ProtectedEntries = canonicalSet(entries)
	return p
}

// protectedNames are project paths that decide what runs later or with more
// trust: git metadata, CI workflows, editor tasks, agent settings and Ta-Da's
// own config. They mirror the paths the permission service always asks
// about, except .git, where only gitMetaNames are protected so commits
// still work.
var protectedNames = []string{
	".tada", ".claude", ".github", ".vscode", ".idea", ".husky",
	".tadarc", "tadarc", ".tada.json", "tada.json", ".mcp.json", ".envrc",
}

// gitMetaNames are the files and folders in a git folder that decide what
// git runs or which folder it uses.
var gitMetaNames = []string{"config", "config.worktree", "commondir", "gitdir", "hooks", "info"}

// GitMetaPattern matches gitMetaNames in any .git folder, including those
// of submodules and linked worktrees kept inside it. It holds no paths, so
// the profile uses it as is.
const GitMetaPattern = `/\.git/((modules/.+/)|(worktrees/[^/]+/))?(config|config\.worktree|commondir|gitdir|hooks|info)(/|$)`

var gitMetaRE = regexp.MustCompile("(?i)" + GitMetaPattern)

// Active reports whether p confines commands.
func (p *Policy) Active() bool {
	return p != nil && p.Mode != ModeOff && p.Mode != ""
}

// AllowsWrite reports whether a command may write to path, which should be
// absolute.
func (p *Policy) AllowsWrite(path string) bool {
	_, ok := p.WritableRoot(path)
	return ok
}

// WritableRoot returns the writable root that holds path, after resolving
// symlinks, or false when path is outside every root or protected.
func (p *Policy) WritableRoot(path string) (string, bool) {
	if !p.Active() {
		return "", true
	}
	c := filepathext.Canonical(path)
	// Protected paths are compared without case: macOS file systems usually
	// ignore it, and guarding too much on a case-sensitive one is harmless.
	lc := strings.ToLower(c)
	for _, prot := range p.ProtectedPaths {
		if within(strings.ToLower(prot), lc) {
			return "", false
		}
	}
	for _, e := range p.ProtectedEntries {
		if strings.ToLower(e) == lc {
			return "", false
		}
	}
	if gitMetaRE.MatchString(c) {
		return "", false
	}
	best := ""
	for _, root := range p.WritableRoots {
		if within(root, c) && len(root) > len(best) {
			best = root
		}
	}
	return best, best != ""
}

// within reports whether p is root or inside it.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// tempRoots are the temp folders every sandboxed command may write.
func tempRoots() []string {
	roots := []string{os.TempDir(), "/tmp"}
	if t := os.Getenv("TMPDIR"); t != "" {
		roots = append(roots, t)
	}
	// macOS keeps a per-user cache folder ("C") next to the per-user temp
	// folder ("T"); compilers and xcrun write there.
	tmp := filepathext.Canonical(os.TempDir())
	if filepath.Base(tmp) == "T" {
		roots = append(roots, filepath.Join(filepath.Dir(tmp), "C"))
	}
	return roots
}

// goCacheDir returns the private Go build cache for projectRoot, creating
// it, or "" when it can't be made. It lives under the user cache folder,
// which sandboxed commands can't otherwise write to.
func goCacheDir(projectRoot string) string {
	base, err := os.UserCacheDir()
	if err != nil || projectRoot == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(filepathext.Canonical(projectRoot)))
	dir := filepath.Join(base, "tada", "sandbox-gocache", hex.EncodeToString(sum[:6]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("Failed to create sandbox Go cache", "dir", dir, "error", err)
		return ""
	}
	return dir
}

// gitDirs returns the git folders for dir: its .git folder or the one its
// .git file points to, and for a linked worktree the shared git folder too.
// Commands may write to the second list. A .git or commondir file can name
// any folder, so a folder it points to is only writable when it has the
// layout git itself would create.
func gitDirs(dir string) (dirs, writable []string) {
	dotGit := filepath.Join(dir, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return nil, nil
	}
	gitDir := dotGit
	trusted := true
	if !info.IsDir() {
		b, err := os.ReadFile(dotGit)
		if err != nil {
			return nil, nil
		}
		rest, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
		if !ok {
			return nil, nil
		}
		gitDir = filepathext.SmartJoin(dir, strings.TrimSpace(rest))
		trusted = trustedGitDir(dir, gitDir)
	}
	dirs = []string{gitDir}
	if trusted {
		writable = append(writable, gitDir)
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return dirs, writable
	}
	common := filepathext.SmartJoin(gitDir, strings.TrimSpace(string(b)))
	dirs = append(dirs, common)
	// Git puts a linked worktree's folder at <common>/worktrees/<name>.
	wt := filepathext.Canonical(gitDir)
	if trusted && filepath.Base(filepath.Dir(wt)) == "worktrees" &&
		filepathext.Canonical(common) == filepath.Dir(filepath.Dir(wt)) &&
		isRepoDir(common) && safeGitRoot(common) {
		writable = append(writable, common)
	} else {
		slog.Warn("Sandbox not allowing writes to untrusted git commondir", "dir", dir, "commondir", common)
	}
	return dirs, writable
}

// trustedGitDir reports whether gitDir, named by dir's .git file, is a
// linked worktree's git folder that points back to dir, or a submodule's
// git folder inside the .git/modules of a repo that contains dir.
func trustedGitDir(dir, gitDir string) bool {
	real := filepathext.Canonical(gitDir)
	ok := false
	switch {
	case !safeGitRoot(real):
	case filepath.Base(filepath.Dir(real)) == "worktrees":
		// Git checks the same backlink before using a worktree.
		b, err := os.ReadFile(filepath.Join(real, "gitdir"))
		ok = err == nil && isFile(filepath.Join(real, "HEAD")) &&
			filepathext.Canonical(strings.TrimSpace(string(b))) == filepathext.Canonical(filepath.Join(dir, ".git"))
	default:
		top, after, found := strings.Cut(real, modulesSep)
		ok = found && after != "" && within(top, filepathext.Canonical(dir)) && isRepoDir(real)
	}
	if !ok {
		slog.Warn("Sandbox not allowing writes to untrusted git folder", "dir", dir, "gitdir", gitDir)
	}
	return ok
}

// modulesSep is where git keeps submodules' git folders.
const modulesSep = string(filepath.Separator) + ".git" + string(filepath.Separator) + "modules" + string(filepath.Separator)

// isRepoDir reports whether dir looks like a git folder with its own
// objects.
func isRepoDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && info.IsDir() && isFile(filepath.Join(dir, "HEAD"))
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// safeGitRoot reports whether dir is a folder commands could be let write
// to: not the file system root, the home folder, or a folder above it.
func safeGitRoot(dir string) bool {
	dir = filepathext.Canonical(dir)
	if dir == filepath.Dir(dir) {
		return false
	}
	if home, err := os.UserHomeDir(); err == nil && within(dir, filepathext.Canonical(home)) {
		return false
	}
	return true
}

// canonicalSet resolves each path and drops empties and duplicates.
func canonicalSet(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		c := filepathext.Canonical(p)
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
