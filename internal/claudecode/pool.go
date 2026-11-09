package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/proc"
)

// IdleTimeout is how long a pooled CLI process may sit without a turn
// before the pool shuts it down.
const IdleTimeout = 10 * time.Minute

// cliProc is a running CLI. One goroutine reads its stdout and hands the
// lines to whichever turn is running; between turns they wait in lines.
type cliProc struct {
	cmd    *exec.Cmd
	stderr *cappedBuffer

	writeMu sync.Mutex
	stdin   io.WriteCloser
	closed  bool
	seq     int

	// lines is closed when stdout ends, after readErr is set.
	lines   chan []byte
	readErr error
	// quit makes the reader drop output nobody will read.
	quit     chan struct{}
	quitOnce sync.Once
	// done is closed once the process has been waited for.
	done    chan struct{}
	waitErr error

	// What the CLI last reported in system/init. Only the running
	// turn touches these.
	sessionID string
	model     string
}

// normalize fills in the binary and environment RunTurn would default to,
// so a launch signature means the same thing however opts were built.
func normalize(opts TurnOptions) TurnOptions {
	if opts.Binary == "" {
		opts.Binary = FindBinary()
	}
	if opts.Env == nil {
		opts.Env = ChildEnv(os.Environ(), false)
	}
	return opts
}

// startProc starts the CLI for opts and its stdout reader.
func startProc(opts TurnOptions) (*cliProc, error) {
	bin := opts.Binary
	if bin == "" {
		return nil, errors.New("claude code: the `claude` CLI was not found on PATH")
	}
	// Not CommandContext: cancelling a turn must interrupt first, then kill.
	cmd := exec.Command(bin, BuildArgs(opts)...) //nolint:noctx // cancellation handled by hand
	cmd.Dir = opts.WorkDir
	proc.HideConsole(cmd)
	// Bounds Wait if a process the CLI started outlives it holding stderr.
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = opts.Env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &cliProc{
		cmd:    cmd,
		stderr: &cappedBuffer{max: 64 << 10},
		stdin:  stdin,
		lines:  make(chan []byte, 256),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude code: start %s: %w", bin, err)
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			select {
			case p.lines <- line:
			case <-p.quit:
			}
		}
		p.readErr = sc.Err()
		close(p.lines)
		// Wait only after stdout is fully read.
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *cliProc) nextID() string {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.seq++
	return fmt.Sprintf("tada-%d", p.seq)
}

func (p *cliProc) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.closed {
		return io.ErrClosedPipe
	}
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

func (p *cliProc) closeStdin() {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if !p.closed {
		p.closed = true
		_ = p.stdin.Close()
	}
}

func (p *cliProc) stdinClosed() bool {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.closed
}

// kill ends the whole tree: on Windows `claude` is often a .cmd shim, and
// killing only the shim leaves node holding stdout, so reads never end.
func (p *cliProc) kill() { _ = proc.KillTree(p.cmd.Process) }

func (p *cliProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// close closes stdin so the CLI exits, kills it if it is still up after
// grace, and waits for it. Only call it when no turn is reading.
func (p *cliProc) close(grace time.Duration) {
	p.closeStdin()
	p.quitOnce.Do(func() { close(p.quit) })
	select {
	case <-p.done:
		return
	case <-time.After(grace):
	}
	p.kill()
	<-p.done
}

// poolEntry is a pooled process and its bookkeeping, guarded by Pool.mu.
type poolEntry struct {
	p    *cliProc
	sig  string
	busy bool
	idle *time.Timer
	// gen tells a stale idle timer, one that fired as a turn began, apart
	// from the current one.
	gen int
}

// Pool keeps one CLI process alive per Claude Code session and sends it
// each new turn over stdin, so later turns skip starting the CLI, its
// MCP handshakes and reloading the session. A process is replaced when
// the turn's launch options change, dropped when a turn is cancelled or
// fails, and shut down after IdleTimeout without a turn.
type Pool struct {
	mu     sync.Mutex
	procs  map[string]*poolEntry
	closed bool
	idle   time.Duration
}

// NewPool returns an empty pool.
func NewPool() *Pool {
	return &Pool{procs: map[string]*poolEntry{}, idle: IdleTimeout}
}

// DefaultPool is the pool the agent runs Claude Code turns through.
var DefaultPool = NewPool()

// signature is everything about how the CLI was launched, apart from
// whether it resumed. Two turns with the same signature can share a
// process. opts must be normalized.
func signature(opts TurnOptions) string {
	opts.Resume = false
	parts := append([]string{opts.Binary, opts.WorkDir}, BuildArgs(opts)...)
	parts = append(parts, "\x01")
	parts = append(parts, opts.Env...)
	return strings.Join(parts, "\x00")
}

// Live reports whether the next RunTurn for opts would reuse a running
// process, which already holds the conversation up to its last turn.
func (pl *Pool) Live(opts TurnOptions) bool {
	opts = normalize(opts)
	pl.mu.Lock()
	defer pl.mu.Unlock()
	e := pl.procs[opts.SessionID]
	return e != nil && !e.busy && e.sig == signature(opts) && e.p.alive()
}

// RunTurn runs one turn like RunTurn, on the session's live process when
// its launch options match, else on a new process that stays up for the
// next turn. A process that died while idle is replaced transparently.
// While a turn runs on the session's process, another turn for it runs
// on a one-off process, as it would without the pool.
func (pl *Pool) RunTurn(ctx context.Context, opts TurnOptions, h Handler) (*TurnResult, error) {
	opts = normalize(opts)
	sig := signature(opts)

	pl.mu.Lock()
	e := pl.procs[opts.SessionID]
	if pl.closed || opts.SessionID == "" || e != nil && e.busy {
		pl.mu.Unlock()
		return RunTurn(ctx, opts, h)
	}
	var stale *cliProc
	if e != nil {
		e.stopIdle()
		if e.sig != sig || !e.p.alive() {
			delete(pl.procs, opts.SessionID)
			stale, e = e.p, nil
		} else {
			e.busy = true
		}
	}
	pl.mu.Unlock()

	if stale != nil {
		// Stop it before the new one takes over the session.
		stale.close(exitGrace)
	}
	if e != nil {
		res, keep, err := e.p.runTurn(ctx, opts, h, false)
		pl.release(opts.SessionID, e, keep)
		if !errors.Is(err, errProcGone) {
			return res, err
		}
	}

	p, err := startProc(opts)
	if err != nil {
		return nil, err
	}
	ne := &poolEntry{p: p, sig: sig, busy: true}
	pl.mu.Lock()
	pooled := !pl.closed && pl.procs[opts.SessionID] == nil
	if pooled {
		pl.procs[opts.SessionID] = ne
	}
	pl.mu.Unlock()

	res, keep, err := p.runTurn(ctx, opts, h, true)
	if pooled {
		pl.release(opts.SessionID, ne, keep)
	} else if keep {
		p.close(exitGrace)
	}
	return res, err
}

// release ends e's turn. A process that is not fit for another turn has
// already been shut down by runTurn and only leaves the pool.
func (pl *Pool) release(id string, e *poolEntry, keep bool) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	e.busy = false
	if !keep || pl.closed || pl.procs[id] != e {
		if pl.procs[id] == e {
			delete(pl.procs, id)
		}
		if keep {
			go e.p.close(exitGrace)
		}
		return
	}
	gen := e.gen
	e.idle = time.AfterFunc(pl.idle, func() { pl.reap(id, e, gen) })
}

// reap shuts e down if it is still idle in the pool.
func (pl *Pool) reap(id string, e *poolEntry, gen int) {
	pl.mu.Lock()
	if e.busy || e.gen != gen || pl.procs[id] != e {
		pl.mu.Unlock()
		return
	}
	delete(pl.procs, id)
	pl.mu.Unlock()
	e.p.close(exitGrace)
}

func (e *poolEntry) stopIdle() {
	e.gen++
	if e.idle != nil {
		e.idle.Stop()
		e.idle = nil
	}
}

// Close shuts down every idle process and stops pooling; processes with a
// turn running are shut down when it ends.
func (pl *Pool) Close() {
	pl.mu.Lock()
	pl.closed = true
	var idle []*cliProc
	for id, e := range pl.procs {
		if e.busy {
			continue
		}
		e.stopIdle()
		delete(pl.procs, id)
		idle = append(idle, e.p)
	}
	pl.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range idle {
		wg.Go(func() { p.close(exitGrace) })
	}
	wg.Wait()
}
