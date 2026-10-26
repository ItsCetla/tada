//go:build windows

package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/proc"
	"github.com/charmbracelet/crush/internal/sandbox"
	"golang.org/x/sys/windows"
	"mvdan.cc/sh/v3/interp"
)

// defaultKillTimeout matches mvdan's DefaultExecHandler default.
const defaultKillTimeout = 2 * time.Second

// isolateProcess gives the child its own hidden console instead of sharing
// Ta-Da's. Without this, a child that reads the console directly (git
// credential prompts, `pause`, getpass) steals the keys meant for Ta-Da, so
// typing, Esc and paste all seem dead, and a child that writes to the
// console directly draws over the TUI. Grandchildren inherit the hidden
// console, so they are isolated too.
func isolateProcess(cmd *exec.Cmd) {
	proc.HideConsole(cmd)
}

// processGroupExecHandler is the Windows counterpart of the Unix handler.
//
// mvdan's DefaultExecHandler only kills the direct child on cancel and, on
// Windows, never sets WaitDelay. When the child had started its own
// children (npm -> node, cmd /c ...), those keep the output pipes open and
// cmd.Wait blocks forever: the tool stays on "Waiting for tool response"
// and cancelling does nothing. This handler puts the child in a job object
// so cancel ends the whole tree, and sets WaitDelay so Wait always returns.
// policy is ignored: Windows has no sandbox yet.
func processGroupExecHandler(killTimeout time.Duration, policy *sandbox.Policy) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}

		cmd := exec.Cmd{
			Path:   path,
			Args:   args,
			Env:    execEnvList(hc.Env),
			Dir:    hc.Dir,
			Stdin:  hc.Stdin,
			Stdout: hc.Stdout,
			Stderr: hc.Stderr,
		}
		isolateProcess(&cmd)
		// Bounds Wait when a process exits (or is killed) but an orphaned
		// grandchild still holds the output pipes.
		cmd.WaitDelay = max(killTimeout, time.Second)

		var killed atomic.Bool
		err = cmd.Start()
		if err == nil {
			job := newProcessJob(cmd.Process.Pid)
			stopf := context.AfterFunc(ctx, func() {
				killed.Store(true)
				killProcessTree(job, cmd.Process)
			})
			err = cmd.Wait()
			stopf()
			if job != 0 {
				_ = windows.CloseHandle(job)
			}
		}

		return windowsExitStatus(ctx, hc.Stderr, err, killed.Load())
	}
}

// newProcessJob puts pid in a fresh job object so that every process it
// starts can be ended together. It returns 0 when that is not possible; the
// caller then falls back to taskkill. The job is not kill-on-close, so a
// command that deliberately starts a long-lived program keeps it running
// after it exits, as it would on Unix.
func newProcessJob(pid int) windows.Handle {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	defer windows.CloseHandle(proc) //nolint:errcheck
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	return job
}

// killProcessTree ends the child and everything it started. taskkill /T
// (in proc.KillTree) runs first, while the child is still alive to walk its
// tree from; the job object then catches processes whose parent already
// exited.
func killProcessTree(job windows.Handle, p *os.Process) {
	_ = proc.KillTree(p)
	if job != 0 {
		_ = windows.TerminateJobObject(job, 1)
	}
}

// windowsExitStatus translates an exec error into an interp exit status,
// following interp.DefaultExecHandler's conventions.
func windowsExitStatus(ctx context.Context, stderr io.Writer, err error, killed bool) error {
	if err == nil {
		return nil
	}
	if killed && ctx.Err() != nil {
		return ctx.Err()
	}
	var exitErr *exec.ExitError
	var execErr *exec.Error
	switch {
	case errors.As(err, &exitErr):
		return interp.ExitStatus(uint8(exitErr.ExitCode())) //nolint:gosec
	case errors.As(err, &execErr):
		fmt.Fprintf(stderr, "%v\n", err)
		return interp.ExitStatus(127)
	case errors.Is(err, exec.ErrWaitDelay):
		// The command exited, but an orphaned subprocess kept the output
		// pipes open past WaitDelay. Treat it like mvdan does: success.
		return nil
	default:
		return err
	}
}
