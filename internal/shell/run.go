package shell

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/charmbracelet/crush/internal/sandbox"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// RunOptions configures a single stateless shell execution via [Run].
//
// The zero value is not useful; at minimum Command must be set. Stdin,
// Stdout, and Stderr may be nil (nil readers/writers are treated as
// empty/discard). BlockFuncs may be nil to disable block-list enforcement —
// hooks use this to run user-authored commands with the same trust level as
// a shell alias.
type RunOptions struct {
	// Command is the shell source to parse and execute.
	Command string
	// Cwd is the working directory for the execution. Required: callers
	// must supply a non-empty value. Run does not silently fall back to
	// the Ta-Da process cwd — hooks and the bash tool have different
	// notions of "default" and each owns that decision.
	Cwd string
	// Env is the full environment visible to the command. The caller is
	// responsible for inheriting from os.Environ() if that's desired.
	Env []string
	// Stdin is the command's standard input. nil is equivalent to an empty
	// input stream.
	Stdin io.Reader
	// Stdout receives the command's standard output. nil discards output.
	Stdout io.Writer
	// Stderr receives the command's standard error. nil discards output.
	Stderr io.Writer
	// BlockFuncs is an optional list of deny-list matchers applied before
	// each command reaches the exec layer. nil disables blocking entirely.
	BlockFuncs []BlockFunc
	// TermWidth is the terminal width in columns for PTY execution.
	// Zero uses a default of 200.
	TermWidth int
}

// Run parses and executes a shell command using the same mvdan.cc/sh
// interpreter stack that the stateful [Shell] type uses (builtins,
// optional block list, optional Go coreutils). It is safe to call
// concurrently from multiple goroutines: each call builds its own
// [interp.Runner] and shares no state with other callers or with any
// [Shell] instance.
//
// Errors returned from the command itself (non-zero exit, context
// cancellation, parse failures) follow the same conventions as
// [Shell.Exec]: inspect with [IsInterrupt] and [ExitCode].
func Run(ctx context.Context, opts RunOptions) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("command execution panic: %v", r)
		}
	}()

	if opts.Cwd == "" {
		return fmt.Errorf("shell.Run: Cwd is required")
	}

	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	line, err := syntax.NewParser().Parse(strings.NewReader(opts.Command), "")
	if err != nil {
		return fmt.Errorf("could not parse command: %w", err)
	}

	runner, err := newRunner(opts.Cwd, opts.Env, opts.Stdin, stdout, stderr, opts.BlockFuncs, nil)
	if err != nil {
		return fmt.Errorf("could not run command: %w", err)
	}

	return runner.Run(ctx, line)
}

// CaptureResult holds the combined output and exit code from a
// captured shell execution.
type CaptureResult struct {
	Output   string
	ExitCode int
}

// PersistFunc is a callback that persists a shell command result.
// Used by RunAndPersist to decouple execution from storage.
type PersistFunc func(command, output string, exitCode int) error

// RunAndPersist executes a shell command via PTY and optionally
// persists the result through the provided callback. This unifies
// the run-and-save pattern used by both AppWorkspace and Backend.
func RunAndPersist(ctx context.Context, opts RunOptions, persist PersistFunc) (CaptureResult, error) {
	result, err := RunAndCapturePTY(ctx, opts)
	if err != nil {
		return CaptureResult{}, err
	}

	if persist != nil {
		if persistErr := persist(opts.Command, result.Output, result.ExitCode); persistErr != nil {
			slog.Error("Failed to persist shell command output", "error", persistErr, "command", opts.Command)
		}
	}

	return result, nil
}

// RunAndCapture executes a shell command and returns its combined
// stdout/stderr output along with the exit code. It inherits the
// current process environment when opts.Env is nil.
func RunAndCapture(ctx context.Context, opts RunOptions) (CaptureResult, error) {
	if opts.Env == nil {
		opts.Env = os.Environ()
	}

	var stdout, stderr bytes.Buffer
	opts.Stdout = &stdout
	opts.Stderr = &stderr

	runErr := Run(ctx, opts)

	exitCode := 0
	if runErr != nil {
		exitCode = ExitCode(runErr)
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	return CaptureResult{
		Output:   output,
		ExitCode: exitCode,
	}, nil
}

// ptyColorEnvVars force color output for programs running inside a
// PTY. These are only applied in RunAndCapturePTY, not in the plain
// Run/RunAndCapture paths where ANSI codes would be noise.
var ptyColorEnvVars = []string{
	"COLORTERM=truecolor",
	"CLICOLOR_FORCE=1",
	"FORCE_COLOR=1",
}

// RunAndCapturePTY executes a shell command through the mvdan.cc/sh
// interpreter with color-forcing environment variables set. Programs
// that respect FORCE_COLOR or CLICOLOR_FORCE (git, cargo, npm, eza,
// bat, ripgrep, etc.) will emit ANSI color sequences even without a
// real PTY. This approach is fully cross-platform — no /bin/sh or
// unix PTY required.
//
// The name is preserved for API compatibility; the PTY path has been
// replaced by the portable interpreter + env-var approach.
func RunAndCapturePTY(ctx context.Context, opts RunOptions) (CaptureResult, error) {
	if opts.Env == nil {
		opts.Env = os.Environ()
	}
	opts.Env = append(opts.Env, ptyColorEnvVars...)
	return RunAndCapture(ctx, opts)
}

// newRunner constructs an [interp.Runner] configured with the standard
// Ta-Da handler stack. Shared by the stateless [Run] entrypoint and the
// stateful [Shell] so the two surfaces cannot drift. A nil policy means no
// sandbox.
func newRunner(cwd string, env []string, stdin io.Reader, stdout, stderr io.Writer, blockFuncs []BlockFunc, policy *sandbox.Policy) (*interp.Runner, error) {
	env = withNonInteractiveEnv(env)
	if policy.Active() && policy.GoCache != "" {
		// The sandbox can't write the usual Go cache, and a cache shared
		// with unsandboxed builds could be poisoned.
		env = append(env, "GOCACHE="+policy.GoCache)
	}
	return interp.New(
		interp.StdIO(stdin, stdout, stderr),
		interp.Interactive(false),
		interp.Env(expand.ListEnviron(env...)),
		interp.Dir(cwd),
		execHandlerOption(blockFuncs, policy),
	)
}

// execHandlerOption returns an interp.RunnerOption that installs the
// standard Ta-Da middleware chain (builtins, script dispatch, block list)
// on top of a process-group-isolated base exec handler.
//
// We use interp.ExecHandler (singular) with a manually-built chain rather
// than interp.ExecHandlers because the latter always appends
// interp.DefaultExecHandler as the final handler, which lacks process group
// isolation. Without isolation, shells like zsh that set up job control
// when sourcing framework files can send SIGINT/SIGTERM to Ta-Da's process
// group and crash the parent.
//
// When policy is active, spawned processes run inside the sandbox and an
// open handler holds the shell's own redirections to the same write rules.
func execHandlerOption(blockFuncs []BlockFunc, policy *sandbox.Policy) interp.RunnerOption {
	base := processGroupExecHandler(defaultKillTimeout, policy)
	handler := base
	for _, mw := range slices.Backward(standardHandlers(blockFuncs, policy)) {
		handler = mw(handler)
	}
	// ExecHandlers always appends DefaultExecHandler which lacks process
	// group isolation, so we use the deprecated ExecHandler instead.
	execOpt := interp.ExecHandler(handler)
	if !policy.Active() {
		return execOpt
	}
	openOpt := interp.OpenHandler(sandboxOpenHandler(policy))
	return func(r *interp.Runner) error {
		if err := execOpt(r); err != nil {
			return err
		}
		return openOpt(r)
	}
}

// nonInteractiveEnvVars are forced on every shell execution to prevent
// commands from hanging on a nonexistent TTY. These are always applied
// regardless of the caller's environment because Ta-Da shells are never
// interactive — preserving user preferences like EDITOR=nvim only causes
// hangs, not useful behavior.
var nonInteractiveEnvVars = []string{
	"TERM=xterm-256color",
	"GIT_EDITOR=false",
	"EDITOR=false",
	"VISUAL=false",
	"JJ_EDITOR=false",
	"JJ_PAGER=cat",
	"GIT_PAGER=cat",
	"PAGER=cat",
}

// withNonInteractiveEnv returns env with nonInteractiveEnvVars forced in,
// replacing any existing values for those keys. The returned slice is a
// new allocation safe to use concurrently with the input.
func withNonInteractiveEnv(env []string) []string {
	// Build a set of override keys for fast lookup.
	overrideKeys := make(map[string]bool, len(nonInteractiveEnvVars))
	for _, kv := range nonInteractiveEnvVars {
		if key, _, ok := strings.Cut(kv, "="); ok {
			overrideKeys[key] = true
		}
	}

	// Copy env, filtering out any keys we will override.
	result := make([]string, 0, len(env)+len(nonInteractiveEnvVars))
	for _, e := range env {
		if key, _, ok := strings.Cut(e, "="); ok && overrideKeys[key] {
			continue
		}
		result = append(result, e)
	}

	return append(result, nonInteractiveEnvVars...)
}

// herdrEnvVars are the environment variables herdr injects into panes
// so agents can report state over its Unix socket API. Subprocesses
// must not inherit these: a child process that calls herdr.Init()
// would attach to the parent's pane and, on exit, release its agent
// authority — making the status vanish. Stripping them here closes
// that gap for every command the bash tool runs.
var herdrEnvVars = []string{
	"HERDR_ENV",
	"HERDR_SOCKET_PATH",
	"HERDR_PANE_ID",
}

// withoutHerdrEnv returns env with all HERDR_* variables removed.
// The returned slice is a new allocation safe to use concurrently
// with the input.
func withoutHerdrEnv(env []string) []string {
	strip := make(map[string]bool, len(herdrEnvVars))
	for _, k := range herdrEnvVars {
		strip[k] = true
	}
	result := make([]string, 0, len(env))
	for _, e := range env {
		if key, _, ok := strings.Cut(e, "="); ok && strip[key] {
			continue
		}
		result = append(result, e)
	}
	return result
}

// providerKeyEnvVars are the LLM provider credentials removed from the agent
// shell's environment, so a command the agent runs cannot read them. Other
// credentials, such as GH_TOKEN or AWS access keys, are left in place because
// the agent's tools legitimately use them.
var providerKeyEnvVars = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY",
	"AZURE_OPENAI_API_KEY",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GOOGLE_GENERATIVE_AI_API_KEY",
	"OPENROUTER_API_KEY",
	"GROQ_API_KEY",
	"XAI_API_KEY",
	"DEEPSEEK_API_KEY",
	"MISTRAL_API_KEY",
	"CEREBRAS_API_KEY",
	"TOGETHER_API_KEY",
	"FIREWORKS_API_KEY",
	"HF_TOKEN",
	"HUGGINGFACE_API_KEY",
	"VERCEL_API_KEY",
	"AI_GATEWAY_API_KEY",
	"ZAI_API_KEY",
	"MOONSHOT_API_KEY",
	"DASHSCOPE_API_KEY",
	"OPENCODE_API_KEY",
	"AWS_BEARER_TOKEN_BEDROCK",
}

var (
	scrubbedEnvMu   sync.RWMutex
	scrubbedEnvVars []string
)

// SetScrubbedEnvVars sets extra variable names, such as those the configured
// providers read their API keys from, to remove from the agent shell's
// environment along with the built-in provider key list. Hooks, MCP servers
// and user-run shell commands keep the full environment.
func SetScrubbedEnvVars(names []string) {
	scrubbedEnvMu.Lock()
	defer scrubbedEnvMu.Unlock()
	scrubbedEnvVars = slices.Clone(names)
}

// withoutProviderKeys returns a new slice of env without provider API key
// variables.
func withoutProviderKeys(env []string) []string {
	scrubbedEnvMu.RLock()
	defer scrubbedEnvMu.RUnlock()
	strip := make(map[string]bool, len(providerKeyEnvVars)+len(scrubbedEnvVars))
	for _, k := range slices.Concat(providerKeyEnvVars, scrubbedEnvVars) {
		strip[envKey(k)] = true
	}
	result := make([]string, 0, len(env))
	for _, e := range env {
		if key, _, ok := strings.Cut(e, "="); ok && strip[envKey(key)] {
			continue
		}
		result = append(result, e)
	}
	return result
}

// envKey normalizes an environment variable name for comparison. Names are
// case-insensitive on Windows.
func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// execMiddleware wraps a base [interp.ExecHandlerFunc], composing like HTTP
// middleware: each layer either handles a command itself or delegates to the
// next handler in the chain.
type execMiddleware = func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc

// standardHandlers returns the exec-handler middleware chain used by both
// [Run] and [Shell]. Order matters:
//  1. builtins first (so Ta-Da's in-process jq wins over any PATH binary);
//  2. script dispatch (shebang / binary / shell-source for path-prefixed
//     argv[0], no-op for bare commands) — runs before the block list so
//     that deny rules see the already-resolved argv of anything the
//     script exec's rather than the outer path-prefixed wrapper;
//  3. block list;
//  4. optional Go coreutils (only when useGoCoreUtils is on and there is
//     no sandbox, since they would write from Ta-Da's own process).
func standardHandlers(blockFuncs []BlockFunc, policy *sandbox.Policy) []execMiddleware {
	handlers := []execMiddleware{
		builtinHandler(),
		scriptDispatchHandler(blockFuncs, policy),
		blockHandler(blockFuncs),
	}
	if useGoCoreUtils && coreUtilsExecHandler != nil && !policy.Active() {
		handlers = append(handlers, coreUtilsExecHandler)
	}
	return handlers
}

// builtinHandler returns middleware that dispatches recognized Ta-Da
// builtins to their in-process Go implementations. All builtins (jq plus the
// config builtins registered by shellconfig) live in the builtins map; config
// builtins are no-ops without a ConfigBuilder on the context.
func builtinHandler() execMiddleware {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return next(ctx, args)
			}
			if h, ok := builtins[args[0]]; ok {
				hc := interp.HandlerCtx(ctx)
				return h(ctx, args, hc.Stdin, hc.Stdout, hc.Stderr)
			}
			return next(ctx, args)
		}
	}
}

// blockHandler returns middleware that rejects commands matched by any of
// the provided [BlockFunc]s before they reach the underlying exec path.
// Every command the argv runs on its behalf (see [CommandVariants]) is
// checked too, so `env curl`, `timeout 5 curl` or `sh -c 'curl ...'` are
// blocked like `curl`. A nil or empty blockFuncs slice is a no-op.
func blockHandler(blockFuncs []BlockFunc) execMiddleware {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) == 0 || len(blockFuncs) == 0 {
				return next(ctx, args)
			}
			for _, variant := range CommandVariants(args) {
				for _, blockFunc := range blockFuncs {
					if blockFunc(variant) {
						return fmt.Errorf("command is not allowed for security reasons: %q", variant[0])
					}
				}
			}
			return next(ctx, args)
		}
	}
}
