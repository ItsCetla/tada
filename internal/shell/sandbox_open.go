package shell

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/sandbox"
	"mvdan.cc/sh/v3/interp"
)

// maxLinkHops bounds how many symlinks a sandboxed write follows.
const maxLinkHops = 40

// writeFlags are the open flags that can change a file.
const writeFlags = os.O_WRONLY | os.O_RDWR | os.O_CREATE | os.O_TRUNC | os.O_APPEND

// sandboxOpenHandler returns an open handler that applies policy to the
// files the shell opens itself, such as the target of `echo x > f`. These
// writes happen inside Ta-Da's process, which sandbox-exec doesn't cover.
// Reads pass straight through.
func sandboxOpenHandler(policy *sandbox.Policy) interp.OpenHandlerFunc {
	def := interp.DefaultOpenHandler()
	return func(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
		if flag&writeFlags == 0 {
			return def(ctx, path, flag, perm)
		}
		hc := interp.HandlerCtx(ctx)
		abs := filepathext.SmartJoin(hc.Dir, path)

		if strings.HasPrefix(filepath.Clean(abs), "/dev/") {
			switch filepath.Clean(abs) {
			case "/dev/null":
				return def(ctx, "/dev/null", flag, perm)
			// These name Ta-Da's own descriptors in this process, so they
			// are mapped to the command's output instead.
			case "/dev/stdout", "/dev/fd/1":
				return writeOnly{hc.Stdout}, nil
			case "/dev/stderr", "/dev/fd/2":
				return writeOnly{hc.Stderr}, nil
			}
			return nil, sandboxDenied(path)
		}

		target, ok := resolveLinks(abs)
		if !ok {
			return nil, sandboxDenied(path)
		}
		root, ok := policy.WritableRoot(target)
		if !ok {
			return nil, sandboxDenied(path)
		}
		f, err := openInRoot(policy, root, filepathext.Canonical(target), flag, perm)
		if err != nil {
			return nil, err
		}
		if flag&os.O_TRUNC != 0 {
			if err := f.Truncate(0); err != nil {
				f.Close()
				return nil, err
			}
		}
		return f, nil
	}
}

// openInRoot opens target through an os.Root at root, so no link can take
// the open outside root. A link could still be swapped in after the
// caller's check, so it opens the parent folder first, checks where that
// folder really is, and only then opens the file in it without following
// a link. O_TRUNC is left to the caller.
func openInRoot(policy *sandbox.Policy, root, target string, flag int, perm os.FileMode) (*os.File, error) {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, sandboxDenied(target)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	dir, err := r.Open(filepath.Dir(rel))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	base := filepath.Base(rel)
	if real, ok := openedPath(dir); ok && (real == "" || !policy.AllowsWrite(filepath.Join(real, base))) {
		return nil, sandboxDenied(target)
	}
	f, err := openAt(r, dir, rel, flag&^os.O_TRUNC, perm)
	if errors.Is(err, syscall.ELOOP) {
		return nil, sandboxDenied(target)
	}
	return f, err
}

// resolveLinks follows symlinks at the end of path, including ones that
// point at files that don't exist yet, so a write through a dangling link
// is checked against where it would land. It reports false when there are
// too many links.
func resolveLinks(path string) (string, bool) {
	for range maxLinkHops {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return path, true
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return path, false
		}
		path = filepathext.SmartJoin(filepath.Dir(path), dest)
	}
	return path, false
}

// sandboxDenied is the error for a blocked write. It is an *os.PathError so
// the shell prints it and fails the command instead of stopping.
func sandboxDenied(path string) error {
	return &os.PathError{Op: "sandbox: writing to", Path: path, Err: errSandboxDenied}
}

var errSandboxDenied = errors.New("not allowed")

// writeOnly adapts a writer to the handler's io.ReadWriteCloser; closing it
// leaves the writer open.
type writeOnly struct{ io.Writer }

func (writeOnly) Read([]byte) (int, error) { return 0, io.EOF }
func (writeOnly) Close() error             { return nil }
