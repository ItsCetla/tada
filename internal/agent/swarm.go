package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/fleet"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/sandbox"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/worktree"
)

// ErrSwarmAutoMode is returned when a swarm agent would start while auto
// mode approves tool calls without asking.
var ErrSwarmAutoMode = errors.New("auto mode is on; turn it off before starting a swarm, so every swarm tool call goes through your permission prompts")

// SwarmAgentRequest is one machine's share of a swarm.
type SwarmAgentRequest struct {
	// Machine is the machine whose model runs the agent.
	Machine fleet.Machine
	// WorkDir is the assignment's worktree. Every tool the agent has is
	// confined to it.
	WorkDir string
	Prompt  string
	Title   string
	// Session, when set, is called with the agent's session id as soon as
	// the session exists.
	Session func(id string)
}

// SwarmAgentResult is how a swarm agent's run went.
type SwarmAgentResult struct {
	SessionID string
	Cost      float64
	Text      string
}

// swarmToolNames are the tools a swarm agent gets: the ones it needs to
// read and change files and run commands in its worktree. It never gets
// the agent, mission or machines tools, so it can't start more agents or
// swarms, nor tools that reach outside the worktree such as fetch,
// downloads, MCP servers, LSP renames or artifacts.
var swarmToolNames = []string{
	tools.BashToolName,
	tools.EditToolName,
	tools.GlobToolName,
	tools.GrepToolName,
	tools.JobKillToolName,
	tools.JobOutputToolName,
	tools.LSToolName,
	tools.MultiEditToolName,
	tools.TodosToolName,
	tools.ViewToolName,
	tools.WriteToolName,
}

// RunSwarmAgent runs a writing agent on a machine, in a worktree. Its
// tool calls go through the permission service and the PreToolUse hooks
// like the main agent's do, and nothing about the run approves them on
// the user's behalf. It refuses to start while auto mode is on.
func (c *coordinator) RunSwarmAgent(ctx context.Context, req SwarmAgentRequest) (SwarmAgentResult, error) {
	if c.permissions == nil {
		return SwarmAgentResult{}, errors.New("swarm agents need the permission service")
	}
	if c.permissions.SkipRequests() {
		return SwarmAgentResult{}, ErrSwarmAutoMode
	}
	if req.WorkDir == "" || !filepath.IsAbs(req.WorkDir) {
		return SwarmAgentResult{}, fmt.Errorf("swarm worktree %q isn't an absolute path", req.WorkDir)
	}
	cfg := c.cfg.Config()
	coder, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return SwarmAgentResult{}, errors.New("coder agent not configured")
	}

	model, err := c.buildMachineModel(ctx, req.Machine)
	if err != nil {
		return SwarmAgentResult{}, err
	}
	pc, ok := cfg.MachineProvider(req.Machine.Name)
	if !ok {
		return SwarmAgentResult{}, fmt.Errorf("machine %s has no provider", req.Machine.Name)
	}
	p, err := coderPrompt(prompt.WithWorkingDir(req.WorkDir))
	if err != nil {
		return SwarmAgentResult{}, err
	}
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:         model,
		SmallModel:         model,
		SystemPromptPrefix: pc.SystemPromptPrefix,
		IsSubAgent:         true,
		// Nothing above a swarm agent checks the budget for it.
		EnforceBudget:        true,
		DisableAutoSummarize: cfg.Options.DisableAutoSummarize,
		Sessions:             c.sessions,
		Messages:             c.messages,
		Cfg:                  c.cfg,
		Notify:               c.notify,
		RunComplete:          c.runComplete,
		Permissions:          c.permissions,
		Questions:            c.questions,
		WorkDir:              req.WorkDir,
		Control:              c.agentControl(),
	})
	initCtx := context.WithoutCancel(ctx)
	systemPrompt, err := c.buildSystemPrompt(initCtx, p, model, true, false)
	if err != nil {
		return SwarmAgentResult{}, err
	}
	a.SetSystemPrompt(systemPrompt)
	a.SetTools(c.swarmTools(coder, req.WorkDir, model.ModelCfg.Model))

	title := req.Title
	if title == "" {
		title = "Swarm on " + req.Machine.Name
	}
	sess, err := c.sessions.Create(ctx, title)
	if err != nil {
		return SwarmAgentResult{}, fmt.Errorf("create session: %w", err)
	}
	// The session's folder is what the agent's tools resolve paths and
	// permission checks against.
	if _, err := c.sessions.SetWorkingDir(ctx, sess.ID, req.WorkDir); err != nil {
		return SwarmAgentResult{SessionID: sess.ID}, fmt.Errorf("pin session to worktree: %w", err)
	}
	if req.Session != nil {
		req.Session(sess.ID)
	}

	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}
	result, runErr := a.Run(ctx, SessionAgentCall{
		SessionID:        sess.ID,
		Prompt:           req.Prompt,
		MaxOutputTokens:  maxTokens,
		ProviderOptions:  getProviderOptions(model, pc),
		Temperature:      model.ModelCfg.Temperature,
		TopP:             model.ModelCfg.TopP,
		TopK:             callTopK(pc, model.ModelCfg.TopK),
		FrequencyPenalty: model.ModelCfg.FrequencyPenalty,
		PresencePenalty:  model.ModelCfg.PresencePenalty,
		NonInteractive:   true,
		OnAuthRefresh:    c.makeAuthRefreshCallback(pc),
	})
	// Jobs the agent left running would go on changing the worktree after
	// the swarm commits it.
	killWorktreeJobs(ctx, req.WorkDir)
	out := SwarmAgentResult{SessionID: sess.ID, Text: subAgentOutput(result)}
	// Read the cost even when the run failed: what it spent is spent.
	if s, err := c.sessions.Get(context.WithoutCancel(ctx), sess.ID); err == nil {
		out.Cost = s.Cost
	}
	return out, runErr
}

// swarmJobKillTimeout bounds how long a finished swarm agent waits for its
// background jobs to stop.
const swarmJobKillTimeout = 5 * time.Second

// killWorktreeJobs stops the background jobs running in dir and returns
// how many it stopped.
func killWorktreeJobs(ctx context.Context, dir string) int {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), swarmJobKillTimeout)
	defer cancel()
	return shell.GetBackgroundShellManager().KillMatching(ctx, func(bs *shell.BackgroundShell) bool {
		return worktree.Contains(dir, bs.WorkingDir)
	})
}

// swarmTools builds a swarm agent's tools, rooted at its worktree. Each
// one refuses paths outside the worktree, and runs the user's PreToolUse
// hooks before that, so a hook that rewrites the input is checked too.
func (c *coordinator) swarmTools(coder config.Agent, dir, modelID string) []fantasy.AgentTool {
	cfg := c.cfg.Config()
	dataDir := cfg.Options.DataDirectory
	shell.SetScrubbedEnvVars(cfg.APIKeyEnvVars())
	all := []fantasy.AgentTool{
		tools.NewBashTool(c.permissions, c.history, dir, dataDir, cfg.Options.Attribution, modelID, sandbox.Settings{Mode: cfg.Options.Sandbox, Network: cfg.Options.SandboxNetwork}),
		tools.NewEditTool(c.lspManager, c.permissions, c.history, c.filetracker, dir),
		tools.NewGlobTool(dir, cfg.Tools.Glob),
		tools.NewGrepTool(dir, cfg.Tools.Grep),
		tools.NewJobKillTool(),
		tools.NewJobOutputTool(dataDir),
		tools.NewLsTool(c.permissions, dir, cfg.Tools.Ls),
		tools.NewMultiEditTool(c.lspManager, c.permissions, c.history, c.filetracker, dir),
		tools.NewTodosTool(c.sessions),
		tools.NewViewTool(c.lspManager, c.permissions, c.filetracker, c.skillTracker, dir),
		tools.NewWriteTool(c.lspManager, c.permissions, c.history, c.filetracker, dir),
	}
	var runner *hooks.Runner
	if pre := cfg.Hooks[hooks.EventPreToolUse]; len(pre) > 0 {
		runner = hooks.NewRunner(pre, dir, c.cfg.WorkingDir())
	}
	return confineTools(all, coder.AllowedTools, dir, runner, c.permissions.SkipRequests)
}

// confineTools keeps the swarm tools the coder agent is allowed, wraps
// each so it refuses paths outside dir and refuses every call while
// autoMode reports true, and wraps that in the hook runner when there is
// one.
func confineTools(all []fantasy.AgentTool, allowed []string, dir string, runner *hooks.Runner, autoMode func() bool) []fantasy.AgentTool {
	var out []fantasy.AgentTool
	for _, t := range all {
		name := t.Info().Name
		if !slices.Contains(swarmToolNames, name) || !slices.Contains(allowed, name) {
			continue
		}
		var wrapped fantasy.AgentTool = &confinedTool{AgentTool: t, root: dir, name: name, autoMode: autoMode}
		if runner != nil {
			wrapped = newHookedTool(wrapped, runner)
		}
		out = append(out, wrapped)
	}
	return out
}

// confinedPathParams are the tool parameters that name a file or folder.
var confinedPathParams = []string{"file_path", "path", "working_dir"}

// confinedShellParam is the job tools' parameter naming a background
// shell, which must have started in the worktree.
const confinedShellParam = "shell_id"

// errInvalidToolInput is returned for tool input that isn't a single JSON
// object.
var errInvalidToolInput = errors.New("the tool input isn't a valid JSON object")

// confinedTool refuses a tool call whose path parameters lead outside
// root or into git's files. It also refuses every call while auto mode is
// on, since auto mode can be turned on after the swarm started.
type confinedTool struct {
	fantasy.AgentTool
	root     string
	name     string
	autoMode func() bool
}

// confinedPatternParams are the glob patterns each tool resolves against
// its search folder.
var confinedPatternParams = map[string]string{
	tools.GlobToolName: "pattern",
	tools.GrepToolName: "include",
}

func (t *confinedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if t.autoMode != nil && t.autoMode() {
		return fantasy.NewTextErrorResponse(ErrSwarmAutoMode.Error()), nil
	}
	if err := confineInput(t.root, call.Input); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if param, ok := confinedPatternParams[t.name]; ok {
		if err := confineGlobInput(t.root, call.Input, param); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	return t.AgentTool.Run(ctx, call)
}

// confineInput returns why a tool call's input may not run in the
// worktree root, or nil when it may. Tools decode their input with
// encoding/json, which matches keys without regard to case and lets the
// last of a repeated key win, so this matches keys the same way and
// refuses input that repeats one.
func confineInput(root, input string) error {
	params, err := topLevelParams(input)
	if err != nil {
		return err
	}
	for key, raw := range params {
		isPath := slices.ContainsFunc(confinedPathParams, func(p string) bool {
			return strings.EqualFold(p, key)
		})
		switch {
		case isPath:
			s, err := stringParam(key, raw)
			if err != nil {
				return err
			}
			if err := confinePath(root, s); err != nil {
				return err
			}
		case strings.EqualFold(key, confinedShellParam):
			id, err := stringParam(key, raw)
			if err != nil {
				return err
			}
			bs, ok := shell.GetBackgroundShellManager().Get(id)
			if ok && !worktree.Contains(root, bs.WorkingDir) {
				return fmt.Errorf("background job %s didn't start in this swarm's worktree (%s); only touch the jobs you started", id, root)
			}
		}
	}
	return nil
}

// topLevelParams splits a JSON object into its members. It refuses input
// that isn't a single object, and an object that names a key twice,
// including twice in different case.
func topLevelParams(input string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(strings.NewReader(input))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errInvalidToolInput
	}
	params := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errInvalidToolInput
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errInvalidToolInput
		}
		for k := range params {
			if strings.EqualFold(k, key) {
				return nil, fmt.Errorf("the tool input names %q more than once; name each parameter once", key)
			}
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, errInvalidToolInput
		}
		params[key] = raw
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errInvalidToolInput
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errInvalidToolInput
	}
	return params, nil
}

// stringParam decodes a parameter that must be a string. Null reads as
// empty.
func stringParam(key string, raw json.RawMessage) (string, error) {
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("the tool input's %q must be a string", key)
	}
	if s == nil {
		return "", nil
	}
	return *s, nil
}

// confineGlobInput refuses a glob pattern in the param parameter that
// could reach outside root. The glob tool joins the pattern's fixed prefix
// onto its search folder, so a pattern such as "../../etc/*" would walk
// out of the worktree. Grep's include only filters, but gets the same
// check.
func confineGlobInput(root, input, param string) error {
	params, err := topLevelParams(input)
	if err != nil {
		return err
	}
	for key, raw := range params {
		if !strings.EqualFold(key, param) {
			continue
		}
		pattern, err := stringParam(key, raw)
		if err != nil {
			return err
		}
		if err := confineGlobPattern(root, pattern); err != nil {
			return err
		}
	}
	return nil
}

// confineGlobPattern refuses a pattern that is absolute, starts with ~,
// climbs with "..", or reaches into .git.
func confineGlobPattern(root, pattern string) error {
	outside := fmt.Errorf("the glob pattern %s reaches outside this swarm's worktree (%s); use a pattern relative to it", pattern, root)
	if strings.HasPrefix(pattern, "~") || filepath.IsAbs(pattern) || strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, "\\") {
		return outside
	}
	parts := strings.FieldsFunc(pattern, func(r rune) bool { return r == '/' || r == '\\' })
	for _, part := range parts {
		if part == ".." {
			return outside
		}
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf("the glob pattern %s is in git's files; leave .git alone", pattern)
		}
	}
	return nil
}

// confinePath refuses a path, relative ones taken from root, that leads
// outside root or into git's files in it, after resolving symlinks.
func confinePath(root, s string) error {
	if s == "" {
		return nil
	}
	outside := fmt.Errorf("%s is outside this swarm's worktree (%s); work only on files inside it", s, root)
	// Tools expand a leading ~ to the home folder.
	if strings.HasPrefix(s, "~") {
		return outside
	}
	p := s
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	rel, err := filepath.Rel(worktree.Canonical(root), worktree.Canonical(filepath.Clean(p)))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return outside
	}
	// The folder may be case-insensitive, as it is by default on macOS.
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf("%s is in git's files; the swarm commits your work itself, so leave .git alone", s)
		}
	}
	return nil
}
