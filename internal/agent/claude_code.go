package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/claudecode"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/control"
	"github.com/charmbracelet/crush/internal/fleet"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/netlog"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/workdir"
)

// The Claude Code engine runs the user's own `claude` CLI for a turn
// instead of Ta-Da's model loop. Claude Code owns the loop, the tools and
// context compaction; Ta-Da sends the prompt, answers permission prompts
// and renders the CLI's live output with its own chat, tool cards and
// diffs.

// ccKnownSessions remembers the Claude Code sessions this process has
// started, so the next turn resumes even before the CLI's transcript file
// is visible on disk.
var ccKnownSessions sync.Map

// ccTranscriptLimit caps the earlier-conversation text sent to a fresh
// Claude Code session, keeping the most recent part.
const ccTranscriptLimit = 20000

// ccHiddenTool reports Claude Code tools that Ta-Da handles itself
// instead of showing a tool card.
func ccHiddenTool(name string) bool {
	return name == "ExitPlanMode"
}

type ccBlock struct {
	typ   string
	id    string
	name  string
	input strings.Builder
}

type ccToolUse struct {
	name  string
	input json.RawMessage
}

// ccRun is the state of one engine turn. Everything except denied is
// touched only from the CLI's event goroutine.
type ccRun struct {
	a              *sessionAgent
	ctx            context.Context // parent ctx: survives cancel for tool updates
	genCtx         context.Context
	call           SessionAgentCall
	model          Model
	sessionLock    *sync.Mutex
	currentSession *session.Session
	assistant      **message.Message
	completed      map[string]struct{}

	streamed   map[string]bool
	fallbackID string
	blocks     map[int]*ccBlock
	tools      map[string]ccToolUse
	usage      claudecode.Usage
	turnUsage  fantasy.Usage
	stopReason string
	finished   bool

	// steered holds prompts written into the running turn that the CLI
	// has not taken in yet, by message UUID. The session's Run goroutine
	// adds to it while the event goroutine takes from it.
	steerMu sync.Mutex
	steered map[string]SessionAgentCall
	order   []string

	denied atomic.Bool
	// questionReplies holds what each AskUserQuestion call's card shows,
	// by tool use ID. canUseTool fills it on its own goroutine.
	questionReplies sync.Map
}

func (a *sessionAgent) runClaudeCode(
	genCtx, ctx context.Context,
	call SessionAgentCall,
	model Model,
	history []message.Message,
	sessionLock *sync.Mutex,
	currentSession *session.Session,
	currentAssistant **message.Message,
	completed map[string]struct{},
) (*fantasy.AgentResult, error) {
	r := &ccRun{
		a:              a,
		ctx:            ctx,
		genCtx:         genCtx,
		call:           call,
		model:          model,
		sessionLock:    sessionLock,
		currentSession: currentSession,
		assistant:      currentAssistant,
		completed:      completed,
		streamed:       map[string]bool{},
		blocks:         map[int]*ccBlock{},
		tools:          map[string]ccToolUse{},
	}

	prompt := message.PromptWithTextAttachments(call.Prompt, call.Attachments)
	var images []claudecode.Image
	for _, att := range call.Attachments {
		if att.IsImage() && len(att.Content) > 0 {
			images = append(images, claudecode.Image{MediaType: att.MimeType, Data: att.Content})
		}
	}

	if err := a.checkBudget(ctx, call.SessionID); err != nil {
		if ensureErr := r.ensureAssistant(); ensureErr != nil {
			return nil, ensureErr
		}
		return nil, budgetError(err)
	}

	// The CLI keeps its sessions per folder, so a tab moved with /cd
	// starts a new CLI session there and gets the history replayed.
	dir := workdir.From(ctx, a.workDir)
	ccSession := claudecode.SessionUUID(call.SessionID)
	if !sameFolder(dir, a.workDir) {
		ccSession = claudecode.SessionUUID(call.SessionID + "\x00" + filepath.Clean(dir))
	}
	_, known := ccKnownSessions.Load(ccSession)
	resume := known || claudecode.SessionExists(ccSession)

	// Outside plan mode the CLI keeps the user's own permission mode;
	// anything it would ask about comes to Ta-Da's permission dialog.
	mode := ""
	if a.planMode {
		mode = "plan"
	}
	opts := claudecode.TurnOptions{
		Binary:         model.ClaudeBinary,
		WorkDir:        dir,
		Model:          model.ModelCfg.Model,
		PermissionMode: mode,
		Effort:         config.ClaudeCodeEffort(model.CatwalkCfg, model.ModelCfg),
		SessionID:      ccSession,
		Images:         images,
		Env:            claudecode.ChildEnv(os.Environ(), model.ClaudeKeepAPIKey),
		ExtraArgs:      a.claudeCodeArgs(call.SessionID),
	}
	handler := claudecode.Handler{
		OnEvent:    r.onEvent,
		CanUseTool: r.canUseTool,
		OnStart:    r.onStart,
		OnSteer:    r.onSteer,
		OnModels:   a.onClaudeModels,
	}
	defer r.endSteering()

	run := func(resume bool) (*claudecode.TurnResult, error) {
		opts.Resume = resume
		// A live process already holds the conversation up to its last
		// turn, so it only needs the turns other models handled since,
		// the same as a resumed session.
		live := claudecode.DefaultPool.Live(opts)
		opts.Prompt = ccTranscript(history, model.ModelCfg.Provider, resume || live) + prompt
		return claudecode.DefaultPool.RunTurn(genCtx, opts, handler)
	}
	// The CLI does its own networking, out of Ta-Da's sight; note the turn
	// so the privacy receipt still says a model host was reached.
	netlog.Default().Note(call.SessionID, netlog.HostClaudeCLI, netlog.PurposeProvider)
	res, err := run(resume)
	switch {
	case resume && errors.Is(err, claudecode.ErrSessionNotFound):
		res, err = run(false)
	case !resume && errors.Is(err, claudecode.ErrSessionInUse):
		res, err = run(true)
	}
	if err == nil || res != nil && res.Result != nil {
		ccKnownSessions.Store(ccSession, true)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || genCtx.Err() != nil {
			return nil, err
		}
		if ensureErr := r.ensureAssistant(); ensureErr != nil {
			return nil, ensureErr
		}
		msg := err.Error()
		if res != nil && strings.TrimSpace(res.Stderr) != "" {
			msg += "\n" + strings.TrimSpace(res.Stderr)
		}
		return nil, &fantasy.Error{Title: "Claude Code", Message: msg, Cause: err}
	}

	if r.cur() != nil && !r.finished {
		if err := r.finishMessage(); err != nil {
			return nil, err
		}
	}

	var final string
	if ev := res.Result; ev != nil {
		final = ev.Result
		if model.ClaudeKeepAPIKey && ev.TotalCostUSD > 0 {
			r.addCost(ev.TotalCostUSD)
		}
		r.recordTurnUsage(ev.TotalCostUSD)
		if ev.IsError && !r.denied.Load() {
			if err := r.ensureAssistant(); err != nil {
				return nil, err
			}
			msg := strings.Join(ev.Errors, "\n")
			if msg == "" {
				msg = cmpOr(ev.Result, ev.Subtype, "Claude Code ended the turn with an error.")
			}
			return nil, &fantasy.Error{Title: "Claude Code", Message: msg}
		}
	}
	if err := r.ensureAssistant(); err != nil {
		return nil, err
	}
	// A denied permission interrupts the turn; end it cleanly so the UI
	// shows the footer instead of waiting for more tool calls.
	if cur := r.cur(); cur != nil && (r.denied.Load() || cur.FinishReason() == message.FinishReasonToolUse) {
		cur.AddFinish(message.FinishReasonEndTurn, "", "")
		if err := a.messages.Update(ctx, *cur); err != nil {
			return nil, err
		}
	}
	if final == "" {
		if cur := r.cur(); cur != nil {
			final = cur.Content().String()
		}
	}
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: final}}},
	}, nil
}

// onStart lets prompts sent while the turn runs go to the CLI.
func (r *ccRun) onStart(steer claudecode.SteerFunc) {
	r.a.ccSteer.Set(r.call.SessionID, func(call SessionAgentCall) bool {
		var images []claudecode.Image
		for _, att := range call.Attachments {
			if att.IsImage() && len(att.Content) > 0 {
				images = append(images, claudecode.Image{MediaType: att.MimeType, Data: att.Content})
			}
		}
		prompt := message.PromptWithTextAttachments(call.Prompt, call.Attachments)
		r.steerMu.Lock()
		defer r.steerMu.Unlock()
		id, ok := steer(claudecode.UserContent(prompt, images))
		if !ok {
			return false
		}
		if r.steered == nil {
			r.steered = map[string]SessionAgentCall{}
		}
		r.steered[id] = call
		r.order = append(r.order, id)
		return true
	})
	r.a.ccPending.Set(r.call.SessionID, r.pendingPrompts)
}

// pendingPrompts lists the steered prompts the CLI hasn't taken in yet,
// oldest first.
func (r *ccRun) pendingPrompts() []string {
	r.steerMu.Lock()
	defer r.steerMu.Unlock()
	var prompts []string
	for _, id := range r.order {
		if call, ok := r.steered[id]; ok {
			prompts = append(prompts, call.Prompt)
		}
	}
	return prompts
}

// onSteer shows a steered prompt in the chat at the point the CLI took
// it in.
func (r *ccRun) onSteer(id string) error {
	r.steerMu.Lock()
	call, ok := r.steered[id]
	delete(r.steered, id)
	r.steerMu.Unlock()
	if !ok {
		return nil
	}
	if r.cur() != nil && !r.finished {
		if err := r.finishMessage(); err != nil {
			return err
		}
	}
	_, err := r.a.createUserMessage(r.ctx, call)
	return err
}

// endSteering stops taking steered prompts. Any the CLI never took in
// run as their own turns next, unless the turn was canceled.
// onClaudeModels keeps the model list the CLI reported: on disk for the
// next launch, and in the live config so the model picker shows it now.
func (a *sessionAgent) onClaudeModels(models []claudecode.ModelInfo) {
	if len(models) == 0 {
		return
	}
	path := config.ClaudeCodeModelsPath()
	if !reflect.DeepEqual(claudecode.LoadModels(path), models) {
		if err := claudecode.SaveModels(path, models); err != nil {
			slog.Warn("Failed to save the Claude Code model list", "error", err)
		}
	}
	if a.cfg != nil {
		a.cfg.Config().SetClaudeCodeModels(models)
	}
}

func (r *ccRun) endSteering() {
	r.a.ccSteer.Del(r.call.SessionID)
	r.a.ccPending.Del(r.call.SessionID)
	r.steerMu.Lock()
	var left []SessionAgentCall
	for _, id := range r.order {
		if call, ok := r.steered[id]; ok {
			call.Steer = false
			left = append(left, call)
		}
	}
	clear(r.steered)
	r.order = nil
	r.steerMu.Unlock()
	if len(left) == 0 || r.genCtx.Err() != nil {
		return
	}
	// Run takes the dispatch lock before a steer, so take it only after
	// letting go of steerMu.
	mu := r.a.sessionMu(r.call.SessionID)
	mu.Lock()
	defer mu.Unlock()
	queued, _ := r.a.messageQueue.Get(r.call.SessionID)
	r.a.messageQueue.Set(r.call.SessionID, append(left, queued...))
}

func cmpOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (r *ccRun) cur() *message.Message { return *r.assistant }

// ensureAssistant makes sure the turn has an assistant message, so an
// error has somewhere to show up.
func (r *ccRun) ensureAssistant() error {
	if r.cur() != nil {
		return nil
	}
	return r.startMessage(nil)
}

func (r *ccRun) startMessage(usage *claudecode.Usage) error {
	if r.cur() != nil && !r.finished {
		if err := r.finishMessage(); err != nil {
			return err
		}
	}
	msg, err := r.a.messages.Create(r.ctx, r.call.SessionID, message.CreateMessageParams{
		Role:     message.Assistant,
		Parts:    []message.ContentPart{},
		Model:    r.model.ModelCfg.Model,
		Provider: r.model.ModelCfg.Provider,
	})
	if err != nil {
		return err
	}
	*r.assistant = &msg
	r.finished = false
	r.stopReason = ""
	r.usage = claudecode.Usage{}
	if usage != nil {
		r.usage = *usage
	}
	clear(r.blocks)
	return nil
}

func (r *ccRun) update() error { return r.a.messages.Update(r.genCtx, *r.cur()) }

// updateDetached saves through the parent context so tool-call state
// lands even when the turn is being canceled.
func (r *ccRun) updateDetached() error { return r.a.messages.Update(r.ctx, *r.cur()) }

func (r *ccRun) appendText(text string) error {
	cur := r.cur()
	if len(cur.Parts) == 0 {
		text = strings.TrimPrefix(text, "\n")
	}
	if text == "" {
		return nil
	}
	cur.AppendContent(text)
	return r.update()
}

func (r *ccRun) mergeUsage(u *claudecode.Usage) {
	if u == nil {
		return
	}
	if u.InputTokens > 0 {
		r.usage.InputTokens = u.InputTokens
	}
	if u.OutputTokens > 0 {
		r.usage.OutputTokens = u.OutputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		r.usage.CacheCreationInputTokens = u.CacheCreationInputTokens
	}
	if u.CacheReadInputTokens > 0 {
		r.usage.CacheReadInputTokens = u.CacheReadInputTokens
	}
}

func (r *ccRun) onEvent(ev claudecode.Event) error {
	if ev.IsSubagent() {
		return nil
	}
	switch ev.Type {
	case "stream_event":
		if ev.Event != nil {
			return r.onStream(ev.Event)
		}
	case "assistant":
		return r.onAssistant(ev.Message)
	case "user":
		return r.onUser(ev)
	}
	return nil
}

func (r *ccRun) onStream(se *claudecode.StreamEvent) error {
	switch se.Type {
	case "message_start":
		var usage *claudecode.Usage
		id := ""
		if se.Message != nil {
			usage, id = se.Message.Usage, se.Message.ID
		}
		if id != "" {
			r.streamed[id] = true
		}
		return r.startMessage(usage)
	}
	if r.cur() == nil {
		return nil
	}
	switch se.Type {
	case "content_block_start":
		cb := se.ContentBlock
		if cb == nil {
			return nil
		}
		b := &ccBlock{typ: cb.Type, id: cb.ID, name: cb.Name}
		r.blocks[se.Index] = b
		switch cb.Type {
		case "text":
			if cb.Text != "" {
				return r.appendText(cb.Text)
			}
		case "thinking":
			if cb.Thinking != "" {
				r.cur().AppendReasoningContent(cb.Thinking)
				return r.update()
			}
		case "tool_use":
			if ccHiddenTool(cb.Name) {
				return nil
			}
			r.cur().AddToolCall(message.ToolCall{ID: cb.ID, Name: ccToolName(cb.Name)})
			return r.updateDetached()
		}
	case "content_block_delta":
		d := se.Delta
		if d == nil {
			return nil
		}
		switch d.Type {
		case "text_delta":
			return r.appendText(d.Text)
		case "thinking_delta":
			r.cur().AppendReasoningContent(d.Thinking)
			return r.update()
		case "signature_delta":
			r.cur().AppendReasoningSignature(d.Signature)
			return nil
		case "input_json_delta":
			if b := r.blocks[se.Index]; b != nil {
				b.input.WriteString(d.PartialJSON)
			}
		}
	case "content_block_stop":
		b := r.blocks[se.Index]
		if b == nil {
			return nil
		}
		switch b.typ {
		case "thinking":
			r.cur().FinishThinking()
			return r.update()
		case "tool_use":
			return r.finishTool(b.id, b.name, json.RawMessage(b.input.String()))
		}
	case "message_delta":
		if se.Delta != nil && se.Delta.StopReason != "" {
			r.stopReason = se.Delta.StopReason
		}
		r.mergeUsage(se.Usage)
	case "message_stop":
		return r.finishMessage()
	}
	return nil
}

// onAssistant handles complete assistant messages. With partial messages
// on these repeat what was streamed, so they only matter when streaming
// didn't happen for a message.
func (r *ccRun) onAssistant(m *claudecode.Message) error {
	if m == nil || r.streamed[m.ID] {
		return nil
	}
	if r.cur() == nil || m.ID == "" || m.ID != r.fallbackID {
		if err := r.startMessage(m.Usage); err != nil {
			return err
		}
		r.fallbackID = m.ID
	} else {
		r.mergeUsage(m.Usage)
	}
	if m.StopReason != "" {
		r.stopReason = m.StopReason
	}
	for _, b := range m.Blocks() {
		switch b.Type {
		case "text":
			if err := r.appendText(b.Text); err != nil {
				return err
			}
		case "thinking":
			r.cur().AppendReasoningContent(b.Thinking)
			if b.Signature != "" {
				r.cur().AppendReasoningSignature(b.Signature)
			}
			r.cur().FinishThinking()
			if err := r.update(); err != nil {
				return err
			}
		case "tool_use":
			if err := r.finishTool(b.ID, b.Name, b.Input); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *ccRun) finishTool(id, ccName string, input json.RawMessage) error {
	if len(strings.TrimSpace(string(input))) == 0 {
		input = json.RawMessage("{}")
	}
	r.tools[id] = ccToolUse{name: ccName, input: input}
	if ccHiddenTool(ccName) {
		if ccName == "ExitPlanMode" {
			var in struct {
				Plan string `json:"plan"`
			}
			if json.Unmarshal(input, &in) == nil && strings.TrimSpace(in.Plan) != "" {
				sep := ""
				if r.cur().Content().String() != "" {
					sep = "\n\n"
				}
				return r.appendText(sep + strings.TrimSpace(in.Plan))
			}
		}
		return nil
	}
	r.cur().AddToolCall(message.ToolCall{
		ID:       id,
		Name:     ccToolName(ccName),
		Input:    ccToolInput(ccName, input),
		Finished: true,
	})
	return r.updateDetached()
}

func (r *ccRun) finishMessage() error {
	cur := r.cur()
	if cur == nil || r.finished {
		return nil
	}
	cur.FinishThinking()
	reason := message.FinishReasonUnknown
	switch r.stopReason {
	case "end_turn", "stop_sequence", "pause_turn":
		reason = message.FinishReasonEndTurn
	case "tool_use":
		reason = message.FinishReasonToolUse
	case "max_tokens":
		reason = message.FinishReasonMaxTokens
	case "refusal":
		reason = message.FinishReasonContentFilter
	case "":
		reason = message.FinishReasonEndTurn
		if len(cur.ToolCalls()) > 0 {
			reason = message.FinishReasonToolUse
		}
	}
	cur.AddFinish(reason, "", "")
	r.finished = true

	r.sessionLock.Lock()
	s, err := r.a.sessions.Get(r.ctx, r.call.SessionID)
	if err != nil {
		r.sessionLock.Unlock()
		return err
	}
	msgUsage := fantasy.Usage{
		InputTokens:         r.usage.InputTokens,
		OutputTokens:        r.usage.OutputTokens,
		CacheReadTokens:     r.usage.CacheReadInputTokens,
		CacheCreationTokens: r.usage.CacheCreationInputTokens,
	}
	r.turnUsage.InputTokens += msgUsage.InputTokens
	r.turnUsage.OutputTokens += msgUsage.OutputTokens
	r.turnUsage.CacheReadTokens += msgUsage.CacheReadTokens
	r.turnUsage.CacheCreationTokens += msgUsage.CacheCreationTokens
	r.a.updateSessionUsage(r.model, &s, msgUsage, nil, false)
	s, err = r.a.sessions.Save(r.ctx, s)
	if err == nil {
		*r.currentSession = s
	}
	r.sessionLock.Unlock()
	if err != nil {
		return err
	}
	return r.updateDetached()
}

// recordTurnUsage adds the turn to the usage ledger. With an API key the
// CLI's own cost is an estimate of the bill; on a subscription it's
// what the turn would cost at API prices, kept apart as flat.
func (r *ccRun) recordTurnUsage(cliCost float64) {
	a := r.a
	if a.control == nil || usageIsZero(r.turnUsage) && cliCost <= 0 {
		return
	}
	row := usageRow(r.model, r.call.SessionID, "chat", r.turnUsage, nil, false)
	row.Provider = "claude-code"
	if cliCost > 0 {
		row.Cost = cliCost
	}
	row.Pricing = control.PricingFlat
	if r.model.ClaudeKeepAPIKey {
		row.Pricing = control.PricingEstimate
		if cliCost <= 0 {
			row.Pricing = control.PricingUnpriced
			row.Cost = 0
		}
	}
	a.control.RecordUsage(row)
}

func (r *ccRun) addCost(cost float64) {
	r.sessionLock.Lock()
	defer r.sessionLock.Unlock()
	s, err := r.a.sessions.Get(r.ctx, r.call.SessionID)
	if err != nil {
		return
	}
	s.Cost += cost
	if s, err = r.a.sessions.Save(r.ctx, s); err == nil {
		*r.currentSession = s
	}
}

func (r *ccRun) onUser(ev claudecode.Event) error {
	var results []claudecode.ContentBlock
	for _, b := range ev.Message.Blocks() {
		if b.Type == "tool_result" {
			results = append(results, b)
		}
	}
	if len(results) == 0 {
		return nil
	}
	if r.cur() != nil && !r.finished {
		if err := r.finishMessage(); err != nil {
			return err
		}
	}
	for _, b := range results {
		tu, ok := r.tools[b.ToolUseID]
		if !ok || ccHiddenTool(tu.name) {
			continue
		}
		text := b.ResultText()
		if reply, ok := r.questionReplies.Load(b.ToolUseID); ok {
			text = reply.(string)
		}
		data, mime := ccImage(b.Content)
		var meta string
		if !b.IsError {
			// The structured result only describes the call when it is
			// the sole result in the message.
			var structured json.RawMessage
			if len(results) == 1 {
				structured = ev.ToolUseResult
			}
			if tu.name == "TodoWrite" {
				meta = r.saveTodos(tu.input)
			} else {
				meta = ccToolMetadata(tu.name, tu.input, structured, text, workdir.From(r.ctx, r.a.workDir))
			}
		}
		tr := message.ToolResult{
			ToolCallID: b.ToolUseID,
			Name:       ccToolName(tu.name),
			Content:    text,
			Data:       data,
			MIMEType:   mime,
			Metadata:   meta,
			IsError:    b.IsError,
		}
		if !tr.IsError {
			r.completed[tr.Name] = struct{}{}
		}
		if _, err := r.a.messages.Create(r.ctx, r.call.SessionID, message.CreateMessageParams{
			Role:  message.Tool,
			Parts: []message.ContentPart{tr},
		}); err != nil {
			return err
		}
	}
	return nil
}

// saveTodos mirrors a TodoWrite call into the session's todo list, which
// drives Ta-Da's todo pill, and returns the todos card metadata.
func (r *ccRun) saveTodos(input json.RawMessage) string {
	todos := ccTodos(input)
	r.sessionLock.Lock()
	defer r.sessionLock.Unlock()
	s, err := r.a.sessions.Get(r.ctx, r.call.SessionID)
	if err != nil {
		return ""
	}
	meta := ccTodosMetadata(s.Todos, todos)
	s.Todos = todos
	if s, err = r.a.sessions.Save(r.ctx, s); err == nil {
		*r.currentSession = s
	} else {
		slog.Warn("Claude Code: failed to save todos", "error", err)
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return ""
	}
	return string(b)
}

// canUseTool answers the CLI's permission prompts through Ta-Da's own
// permission dialog. It runs on its own goroutine per request.
func (r *ccRun) canUseTool(ctx context.Context, req claudecode.PermissionRequest) claudecode.Decision {
	switch req.ToolName {
	case "ExitPlanMode":
		return claudecode.Decision{Message: "Ta-Da has shown your plan to the user. Stop here and end your turn; the user will switch out of plan mode when they want it carried out."}
	case "AskUserQuestion":
		return r.ccAskUser(ctx, req)
	case "mcp__tada-mission__" + control.MissionToolName:
		// The mission tool only writes the session's mission file, unless
		// it sets a check command, which later runs unattended: that asks
		// like a bash command.
		var p control.MissionParams
		_ = json.Unmarshal(req.Input, &p)
		checks := p.Checks()
		if len(checks) == 0 {
			return claudecode.Decision{Allow: true}
		}
		for _, c := range checks {
			if name := tools.BannedCommandIn(c); name != "" {
				return claudecode.Decision{Message: fmt.Sprintf("check command is not allowed for security reasons: %q", name)}
			}
		}
	case "mcp__" + fleet.MCPServerName + "__" + fleet.AskToolName:
		// Machines are ones the user set up to take this work, like the
		// agent tool's machine runs, which don't ask either.
		return claudecode.Decision{Allow: true}
	case "mcp__" + fleet.MCPServerName + "__" + fleet.ToolName:
		var p fleet.Params
		_ = json.Unmarshal(req.Input, &p)
		if !p.Changes() {
			return claudecode.Decision{Allow: true}
		}
	}
	if req.ToolName == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(req.Input, &in)
		if name := tools.BannedCommandIn(in.Command); name != "" {
			return claudecode.Decision{Message: fmt.Sprintf("command is not allowed for security reasons: %q", name)}
		}
	}
	if r.a.permissions == nil {
		r.denied.Store(true)
		return claudecode.Decision{Message: "Ta-Da has no permission service for this session.", Interrupt: true}
	}
	name, action, description, path, params := ccPermission(req, workdir.From(r.ctx, r.a.workDir))
	asked := time.Now()
	ok, err := r.a.permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID:   r.call.SessionID,
		ToolCallID:  req.ToolUseID,
		ToolName:    name,
		Description: description,
		Action:      action,
		Params:      params,
		Path:        path,
	})
	if err != nil || !ok {
		r.denied.Store(true)
		if permission.TakenBackSince(r.a.permissions, asked) {
			return claudecode.Decision{Message: "The person took control back. Stop and ask them before trying again.", Interrupt: true}
		}
		return claudecode.Decision{Message: "The user denied permission for this tool call.", Interrupt: true}
	}
	if req.ToolName == "mcp__tada-mission__"+control.MissionToolName {
		r.approveMissionChecks(ctx, req.Input)
	}
	return claudecode.Decision{Allow: true}
}

// checkApprover is the part of the control hub that records check
// commands the user allowed.
type checkApprover interface {
	ApproveChecks(ctx context.Context, commands ...string) error
}

// approveMissionChecks records the check commands of a mission tool call
// the user just allowed. Only this process does it, after Ta-Da's own
// prompt: the mission MCP server never approves, so a tool call the CLI
// let through without asking can't approve its own checks.
func (r *ccRun) approveMissionChecks(ctx context.Context, input json.RawMessage) {
	ca, ok := r.a.control.(checkApprover)
	if !ok {
		return
	}
	var p control.MissionParams
	_ = json.Unmarshal(input, &p)
	if err := ca.ApproveChecks(ctx, p.Checks()...); err != nil {
		slog.Warn("Failed to record approved mission checks", "error", err)
	}
}

// ccTranscript returns the earlier Ta-Da conversation the CLI hasn't
// seen, as a prefix for the prompt. A fresh Claude Code session gets the
// whole session; a resumed one only gets turns other models handled
// since the engine last ran.
func ccTranscript(history []message.Message, provider string, resume bool) string {
	start := 0
	if resume {
		start = len(history)
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Role == message.Assistant && history[i].Provider == provider {
				start = i + 1
				break
			}
		}
	}
	var b strings.Builder
	for _, m := range history[start:] {
		switch m.Role {
		case message.User:
			if t := strings.TrimSpace(m.Content().String()); t != "" {
				fmt.Fprintf(&b, "User: %s\n\n", t)
			}
		case message.Assistant:
			if t := strings.TrimSpace(m.Content().String()); t != "" {
				fmt.Fprintf(&b, "Assistant: %s\n\n", t)
			}
			for _, tc := range m.ToolCalls() {
				fmt.Fprintf(&b, "[tool call: %s %s]\n", tc.Name, ccClip(tc.Input, 300))
			}
		case message.Tool:
			for _, tr := range m.ToolResults() {
				fmt.Fprintf(&b, "[tool result: %s] %s\n\n", tr.Name, ccClip(tr.Content, 600))
			}
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return ""
	}
	if len(text) > ccTranscriptLimit {
		text = text[len(text)-ccTranscriptLimit:]
		for !utf8.ValidString(text) && text != "" {
			text = text[1:]
		}
		text = "[earlier messages cut]\n" + text
	}
	intro := "These messages happened earlier in this Ta-Da session, before you joined. Use them as context."
	if resume {
		intro = "Another model handled these turns in this Ta-Da session since your last reply. Use them as context."
	}
	return "<previous-conversation>\n" + intro + "\n\n" + text + "\n</previous-conversation>\n\n"
}

func ccClip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) && s != "" {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// ErrClaudeCodeSummarize is returned when asked to summarize a session the
// Claude Code engine runs: the CLI compacts its own context.
var ErrClaudeCodeSummarize = errors.New("summarize is not available for Claude Code sessions: Claude Code compacts its own context")
