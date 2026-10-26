//go:build darwin

package shell

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/sandbox"
	"github.com/stretchr/testify/require"
)

// sandboxFixture is a project with a .git folder, a folder outside the
// project, and the policy sandbox.New builds for it, minus the temp
// folders, where t.TempDir puts the outside folder.
type sandboxFixture struct {
	proj, out string
	policy    *sandbox.Policy
}

func newSandboxFixture(t *testing.T, projName string) sandboxFixture {
	t.Helper()
	if !sandbox.Available() {
		t.Skip("sandbox-exec is not available")
	}
	proj := filepathext.Canonical(filepath.Join(t.TempDir(), projName))
	out := filepathext.Canonical(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(proj, ".git", "hooks"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(out, "keep"), []byte("keep"), 0o644))
	policy := sandbox.New(sandbox.Settings{Mode: "workspace-write"}, proj, proj, filepath.Join(proj, ".tada"))
	require.NotNil(t, policy)
	var roots []string
	for _, root := range policy.WritableRoots {
		if root == policy.GoCache || root == proj || strings.HasPrefix(root, proj+"/") {
			roots = append(roots, root)
		}
	}
	policy.WritableRoots = roots
	return sandboxFixture{proj: proj, out: out, policy: policy}
}

func (f sandboxFixture) exec(t *testing.T, command string) (string, string, error) {
	t.Helper()
	sh := NewShell(&Options{WorkingDir: f.proj, Sandbox: f.policy, Env: os.Environ()})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	return sh.Exec(ctx, command)
}

func requireNoFile(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	require.True(t, os.IsNotExist(err), "%s should not exist", path)
}

func TestSandbox_DeniesWritesOutsideProject(t *testing.T) {
	t.Parallel()

	for name, command := range map[string]string{
		// Guarded by the open handler: the shell opens the file itself.
		"redirect": `echo x > "$OUT/f"`,
		"append":   `echo x >> "$OUT/f"`,
		// Guarded by sandbox-exec around spawned processes.
		"sh -c":     `/bin/sh -c 'echo x > "$OUT/f"'`,
		"cmd subst": `v=$(touch "$OUT/f")`,
		"hardlink":  `ln "$OUT/keep" hl && echo x >> hl`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newSandboxFixture(t, "proj")
			_, stderr, err := f.exec(t, "OUT="+f.out+"; "+command)
			require.Error(t, err, stderr)
			requireNoFile(t, filepath.Join(f.out, "f"))
			b, rerr := os.ReadFile(filepath.Join(f.out, "keep"))
			require.NoError(t, rerr)
			require.Equal(t, "keep", string(b))
		})
	}
}

func TestSandbox_RedirectErrorNamesSandbox(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, "proj")
	_, stderr, err := f.exec(t, "echo x > "+filepath.Join(f.out, "f"))
	require.Error(t, err)
	require.Contains(t, stderr, "sandbox: writing to")
}

func TestSandbox_ShebangScriptDenied(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, "proj")
	script := filepath.Join(f.proj, "w.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho x > \"$1\"\n"), 0o755))

	_, stderr, err := f.exec(t, "./w.sh "+filepath.Join(f.out, "f"))
	require.Error(t, err)
	require.Contains(t, stderr, "Operation not permitted")
	requireNoFile(t, filepath.Join(f.out, "f"))
}

func TestSandbox_RmOutsideDenied(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, "proj")
	_, _, err := f.exec(t, "rm "+filepath.Join(f.out, "keep"))
	require.Error(t, err)
	require.FileExists(t, filepath.Join(f.out, "keep"))
}

// Not parallel: it flips the package-wide coreutils switch.
func TestSandbox_GoCoreUtilsOffWhenSandboxed(t *testing.T) {
	old := useGoCoreUtils
	useGoCoreUtils = true
	t.Cleanup(func() { useGoCoreUtils = old })

	f := newSandboxFixture(t, "proj")
	_, _, err := f.exec(t, "rm "+filepath.Join(f.out, "keep"))
	require.Error(t, err)
	require.FileExists(t, filepath.Join(f.out, "keep"))
}

func TestSandbox_AllowsProjectAndDevWrites(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, "proj")
	stdout, stderr, err := f.exec(t, `echo a > in.txt
mkdir -p sub && /bin/sh -c 'echo b > sub/b.txt'
echo c > /dev/null
ls does-not-exist 2>/dev/null || true
/bin/sh -c 'ls does-not-exist 2>/dev/null' || true
echo d > /dev/stdout
echo e >> .git/objects-file
mkdir .git/objects && touch .git/objects/x`)
	require.NoError(t, err, stderr)
	require.Equal(t, "d\n", stdout)
	require.Empty(t, stderr)
	for _, p := range []string{"in.txt", "sub/b.txt", ".git/objects-file", ".git/objects/x"} {
		require.FileExists(t, filepath.Join(f.proj, p))
	}
}

func TestSandbox_ProtectsGitHooks(t *testing.T) {
	t.Parallel()
	for name, command := range map[string]string{
		"redirect":      `echo x > .git/hooks/pre-commit`,
		"sh -c":         `/bin/sh -c 'echo x > .git/hooks/pre-commit'`,
		"case":          `/bin/sh -c 'echo x > .GIT/HOOKS/pre-commit'`,
		"redirect case": `echo x > .GIT/HOOKS/pre-commit`,
		"dangling link": `ln -s .git/hooks/pre-commit link; echo x > link`,
		"rename .git":   `mv .git gx && mkdir -p gx/hooks && echo x > gx/hooks/pre-commit && mv gx .git`,
		"dir link":      `ln -s .git/hooks d && echo x > d/pre-commit`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newSandboxFixture(t, "proj")
			_, stderr, err := f.exec(t, command)
			require.Error(t, err, stderr)
			requireNoFile(t, filepath.Join(f.proj, ".git", "hooks", "pre-commit"))
			require.DirExists(t, filepath.Join(f.proj, ".git", "hooks"))
		})
	}
}

func TestSandbox_ProjectNameWithQuote(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, `we"ird) (allow file-write*`)
	_, stderr, err := f.exec(t, `/bin/sh -c 'echo a > in.txt' && echo b > in2.txt`)
	require.NoError(t, err, stderr)
	require.FileExists(t, filepath.Join(f.proj, "in.txt"))
	require.FileExists(t, filepath.Join(f.proj, "in2.txt"))

	_, _, err = f.exec(t, `/bin/sh -c 'echo x > "$0/f"' `+f.out)
	require.Error(t, err)
	requireNoFile(t, filepath.Join(f.out, "f"))
}

func TestSandbox_CancelKillsCommand(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, "proj")
	sh := NewShell(&Options{WorkingDir: f.proj, Sandbox: f.policy})
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := sh.Exec(ctx, "sleep 30")
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second)
}

func TestSandbox_BackgroundJobDenied(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, "proj")
	m := newBackgroundShellManager()
	bg, err := m.StartSandboxed(t.Context(), f.proj, nil, f.policy, "/bin/sh -c 'echo x > "+f.out+"/f'; echo y > "+f.out+"/g", "")
	require.NoError(t, err)
	bg.Wait()
	requireNoFile(t, filepath.Join(f.out, "f"))
	requireNoFile(t, filepath.Join(f.out, "g"))
}

func TestSandbox_NetworkOff(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, "proj")
	// Binding a non-loopback address fails; loopback works. Python is
	// avoided so the test only needs base system tools.
	if _, err := os.Stat("/usr/bin/nc"); err != nil {
		t.Skip("nc is not available")
	}
	_, stderr, err := f.exec(t, `/usr/bin/nc -z -G 2 1.1.1.1 443`)
	require.Error(t, err, stderr)

	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_, stderr, err = f.exec(t, `/usr/bin/nc -z -G 2 127.0.0.1 `+port)
	require.NoError(t, err, stderr)

	// Unix sockets reach other services, so they're off too. The socket
	// lives in the project, which keeps its path short enough.
	sock := filepath.Join(f.proj, "s")
	ul, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
	require.NoError(t, err)
	defer ul.Close()
	go func() {
		for {
			c, err := ul.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, stderr, err = f.exec(t, `/usr/bin/nc -U `+sock+` </dev/null`)
	require.Error(t, err, stderr)
	_, stderr, err = NewShell(&Options{WorkingDir: f.proj, Env: os.Environ()}).Exec(t.Context(), `/usr/bin/nc -U `+sock+` </dev/null`)
	require.NoError(t, err, stderr)
}

func TestSandbox_DeniesAppleEvents(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, "proj")
	// Without the deny rule osascript fails with -10004 or runs the event.
	_, stderr, err := f.exec(t, `/usr/bin/osascript -e 'tell application "Finder" to count windows'`)
	require.Error(t, err)
	require.Contains(t, stderr, "-600")
}

func TestSandbox_ProtectsGitMetadata(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{
		".git/config",
		".git/commondir",
		".git/config.worktree",
		".git/info/attributes",
		".git/modules/x/hooks/y",
		".git/modules/x/HOOKS/y",
		".git/worktrees/w/Config",
		".git/modules/x/config",
		".git/worktrees/w/gitdir",
	} {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			f := newSandboxFixture(t, "proj")
			require.NoError(t, os.MkdirAll(filepath.Join(f.proj, filepath.Dir(rel)), 0o755))
			for _, command := range []string{
				`echo x > ` + rel,
				`/bin/sh -c 'echo x > ` + rel + `'`,
			} {
				_, stderr, err := f.exec(t, command)
				require.Error(t, err, command+": "+stderr)
				requireNoFile(t, filepath.Join(f.proj, rel))
			}
		})
	}
}

func TestSandbox_GitWorks(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("git is not available")
	}
	f := newSandboxFixture(t, "proj")
	require.NoError(t, os.RemoveAll(filepath.Join(f.proj, ".git")))
	// Git init has to run outside the sandbox, which keeps .git/config
	// read-only.
	initGit := NewShell(&Options{WorkingDir: f.proj, Env: os.Environ()})
	_, stderr, err := initGit.Exec(t.Context(), "git init -q")
	require.NoError(t, err, stderr)

	_, stderr, err = f.exec(t, `echo a > a.txt && git add a.txt && git -c user.name=t -c user.email=t@t -c commit.gpgsign=false commit -q -m a && git log --oneline > log.txt`)
	require.NoError(t, err, stderr)
	require.FileExists(t, filepath.Join(f.proj, "log.txt"))
}

func TestSandbox_GoCacheEnv(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, "proj")
	require.NotEmpty(t, f.policy.GoCache)
	sh := NewShell(&Options{WorkingDir: f.proj, Sandbox: f.policy, Env: append(os.Environ(), "GOCACHE=/elsewhere")})
	stdout, stderr, err := sh.Exec(t.Context(), `/bin/sh -c 'echo $GOCACHE'; echo $GOCACHE`)
	require.NoError(t, err, stderr)
	require.Equal(t, f.policy.GoCache+"\n"+f.policy.GoCache+"\n", stdout)
}

// TestSandbox_OpenRechecksParent swaps a folder for a link to .git/hooks
// after the open handler's first check, by calling openInRoot directly.
func TestSandbox_OpenRechecksParent(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, "proj")
	// A relative link, since os.Root refuses absolute ones anyway.
	require.NoError(t, os.Symlink(filepath.Join(".git", "hooks"), filepath.Join(f.proj, "d")))
	_, err := openInRoot(f.policy, f.proj, filepath.Join(f.proj, "d", "pre-commit"), os.O_WRONLY|os.O_CREATE, 0o644)
	require.Error(t, err)
	requireNoFile(t, filepath.Join(f.proj, ".git", "hooks", "pre-commit"))

	// A link as the file itself isn't followed.
	require.NoError(t, os.Symlink(filepath.Join(".git", "hooks", "x"), filepath.Join(f.proj, "l")))
	_, err = openInRoot(f.policy, f.proj, filepath.Join(f.proj, "l"), os.O_WRONLY|os.O_CREATE, 0o644)
	require.Error(t, err)
	requireNoFile(t, filepath.Join(f.proj, ".git", "hooks", "x"))

	file, err := openInRoot(f.policy, f.proj, filepath.Join(f.proj, "ok.txt"), os.O_WRONLY|os.O_CREATE, 0o644)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.FileExists(t, filepath.Join(f.proj, "ok.txt"))
}

func TestSandbox_PolicyFromNew(t *testing.T) {
	t.Parallel()
	if !sandbox.Available() {
		t.Skip("sandbox-exec is not available")
	}
	proj := filepathext.Canonical(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(proj, ".git", "hooks"), 0o755))
	data := filepath.Join(proj, ".tada")

	p := sandbox.New(sandbox.Settings{Mode: "workspace-write"}, proj, proj, data)
	require.True(t, p.Active())
	require.Contains(t, p.WritableRoots, proj)
	require.Contains(t, p.WritableRoots, filepathext.Canonical(os.TempDir()))
	require.Contains(t, p.WritableRoots, p.GoCache)
	require.DirExists(t, p.GoCache)
	if home, err := os.UserHomeDir(); err == nil {
		require.NotContains(t, p.WritableRoots, filepathext.Canonical(filepath.Join(home, ".cache")))
	}
	require.Contains(t, p.ProtectedPaths, filepath.Join(proj, ".git", "hooks"))
	require.Contains(t, p.ProtectedPaths, filepath.Join(proj, ".tadarc"))
	require.Contains(t, p.ProtectedEntries, filepath.Join(proj, ".git"))
	require.True(t, p.AllowsWrite(filepath.Join(proj, "main.go")))
	require.False(t, p.AllowsWrite(filepath.Join(proj, ".git", "hooks", "x")))
	require.False(t, p.AllowsWrite(filepath.Join(proj, ".git")))
	require.True(t, p.AllowsWrite(filepath.Join(proj, ".git", "index")))
	require.True(t, p.AllowsWrite(filepath.Join(proj, ".git", "refs", "heads", "main")))
	require.False(t, p.AllowsWrite(filepath.Join(proj, ".git", "config")))
	require.False(t, p.AllowsWrite(filepath.Join(proj, ".git", "modules", "x", "hooks", "y")))

	ro := sandbox.New(sandbox.Settings{Mode: "read-only"}, proj, proj, data)
	require.NotContains(t, ro.WritableRoots, proj)
	require.Nil(t, sandbox.New(sandbox.Settings{Mode: "off"}, proj, proj, data))

	// Real commands under the full policy can still use the project.
	sh := NewShell(&Options{WorkingDir: proj, Sandbox: p})
	_, stderr, err := sh.Exec(t.Context(), `/bin/sh -c 'echo a > a.txt' && /bin/sh -c 'echo x > .git/hooks/x'`)
	require.Error(t, err)
	require.True(t, strings.Contains(stderr, "Operation not permitted"), stderr)
	require.FileExists(t, filepath.Join(proj, "a.txt"))
}

func TestSandbox_RedirectThroughLinksInProject(t *testing.T) {
	t.Parallel()
	f := newSandboxFixture(t, "proj")
	require.NoError(t, os.Mkdir(filepath.Join(f.proj, "sub"), 0o755))
	_, stderr, err := f.exec(t, `ln -s a.txt l && echo x > l && ln -s sub/b.txt l3 && echo x > l3 && ln -s c.txt l1 && ln -s l1 l2 && echo x > l2`)
	require.NoError(t, err, stderr)
	for _, p := range []string{"a.txt", "sub/b.txt", "c.txt"} {
		require.FileExists(t, filepath.Join(f.proj, p))
	}
}

// TestSandbox_CaseVariantsOfAbsentProtectedPaths creates protected paths
// that don't exist yet under a different letter case, which a
// case-insensitive volume would treat as the same file.
func TestSandbox_CaseVariantsOfAbsentProtectedPaths(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{".TADARC", ".Envrc", ".git/COMMONDIR", ".git/Info/attributes", ".GIT/hooks/x"} {
		for _, sh := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s sh=%v", rel, sh), func(t *testing.T) {
				t.Parallel()
				f := newSandboxFixture(t, "proj")
				require.NoError(t, os.RemoveAll(filepath.Join(f.proj, ".git", "hooks")))
				command := `mkdir -p "$(dirname ` + rel + `)"; echo x > ` + rel
				if sh {
					command = `/bin/sh -c '` + command + `'`
				}
				_, stderr, err := f.exec(t, command)
				require.Error(t, err, stderr)
				requireNoFile(t, filepath.Join(f.proj, rel))
			})
		}
	}
}
