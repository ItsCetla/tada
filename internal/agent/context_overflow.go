package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
)

// contextOverflowPhrases are lowercase fragments of the errors providers
// send when a prompt doesn't fit the model's context window. fantasy
// already recognises the big providers' wording; these cover the
// OpenAI-compatible servers (LM Studio, llama.cpp, vLLM, LiteLLM, …)
// that phrase it their own way.
var contextOverflowPhrases = []string{
	"maximum context length",
	"context_length_exceeded",
	"contextwindowexceeded",
	"context window exceeded",
	"prompt is too long",
	"exceeds the available context size",
	"exceeds the context window",
	"exceeds the maximum number of tokens",
	"input is too long",
	"context length of only",
	"reduce the length of the messages",
}

// contextWindowPatterns pull the window size out of an overflow error
// when the provider names it.
var contextWindowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)maximum context length is (\d+)`),
	regexp.MustCompile(`(?i)context length of (?:only )?(\d+)`),
	regexp.MustCompile(`(?i)context (?:window|size|length) (?:of|is) (\d+)`),
	regexp.MustCompile(`(?i)n_ctx(?:_slot)?\s*[=:]\s*(\d+)`),
	// LiteLLM: "Max Input Tokens=262144, Got=274491".
	regexp.MustCompile(`(?i)max(?:imum)?[ _]input[ _]tokens\s*[=:]\s*(\d+)`),
}

// contextOverflow reports whether err is the provider refusing a prompt
// because it is larger than the model's context window. The second value
// is the window size the error named, or 0 when it didn't say.
func contextOverflow(err error) (bool, int64) {
	if err == nil || errors.Is(err, context.Canceled) {
		return false, 0
	}
	var texts []string
	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		// A rate limit ("too many tokens per minute") is not an overflow.
		if providerErr.StatusCode == 429 {
			return false, 0
		}
		if providerErr.IsContextTooLarge() {
			if providerErr.ContextMaxTokens > 0 {
				return true, int64(providerErr.ContextMaxTokens)
			}
			return true, contextWindowFromText(providerErr.Message, string(providerErr.ResponseBody))
		}
		texts = append(texts, providerErr.Message, string(providerErr.ResponseBody))
	}
	texts = append(texts, err.Error())
	for _, text := range texts {
		lower := strings.ToLower(text)
		for _, phrase := range contextOverflowPhrases {
			if strings.Contains(lower, phrase) {
				return true, contextWindowFromText(texts...)
			}
		}
	}
	return false, 0
}

func contextWindowFromText(texts ...string) int64 {
	for _, text := range texts {
		for _, re := range contextWindowPatterns {
			if m := re.FindStringSubmatch(text); m != nil {
				if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 0
}

// maxOverflowRetries caps how many times one request is summarized and
// re-run after outgrowing the context window.
const maxOverflowRetries = 3

// canRetryOverflow reports whether a turn that overflowed the context
// window should be summarized and run again. The first overflow always
// retries. A re-run that overflows again retries only if it finished at
// least one step first: then summarizing shrinks it. One that fails on
// its first step is already as small as summarizing makes it.
func canRetryOverflow(retries, stepsDone int) bool {
	if retries == 0 {
		return true
	}
	return retries < maxOverflowRetries && stepsDone > 0
}

// Prefixes of the prompts that continue a request after it was
// summarized.
const (
	interruptedPromptPrefix   = "The previous session was interrupted because it got too long, the initial user request was: "
	overflowRetryPromptPrefix = "The previous request failed because the conversation no longer fit in the context window, so it was summarized. The initial user request was: "
)

// continuationPrompt wraps the original request in prefix. A prompt that
// is already a continuation is returned as is, so a request summarized
// more than once doesn't nest the note.
func continuationPrompt(prefix, prompt string) string {
	if strings.HasPrefix(prompt, interruptedPromptPrefix) || strings.HasPrefix(prompt, overflowRetryPromptPrefix) {
		return prompt
	}
	return prefix + "`" + prompt + "`"
}

// isSilentOverflow reports whether step looks like a provider dropping an
// oversized prompt instead of rejecting it: it stopped at the length limit
// without writing any text, reasoning or tool call. The prompt must also
// be known to be large, either from the usage the provider reported or,
// when it reported none, from estimatedPrompt filling the window up to
// threshold. A model that simply hit a small output limit doesn't count.
func isSilentOverflow(step fantasy.StepResult, estimatedPrompt, contextWindow, threshold int64) bool {
	if step.FinishReason != fantasy.FinishReasonLength || step.Usage.OutputTokens != 0 ||
		step.Content.Text() != "" || len(step.Content.Reasoning()) != 0 ||
		len(step.Content.ToolCalls()) != 0 {
		return false
	}
	if contextTokens(step.Usage) > 0 {
		return true
	}
	return contextWindow > 0 && contextWindow-estimatedPrompt <= threshold
}

// overflowGiveUpMessage explains a context overflow that won't be
// summarized and retried.
func overflowGiveUpMessage(autoSummarizeOff, retried bool) string {
	switch {
	case autoSummarizeOff:
		return "The conversation no longer fits the model's context window, and auto-summarize is off. Summarize the session or start a new one."
	case retried:
		return "The conversation still doesn't fit the model's context window after summarizing. Start a new session, or ask for the work in smaller steps."
	default:
		return "The conversation no longer fits the model's context window. Summarize the session or start a new one."
	}
}

func contextWindowKey(m Model) string {
	return m.ModelCfg.Provider + "\x00" + m.ModelCfg.Model
}

// learnContextWindow remembers a window size a provider reported in an
// overflow error, for models whose catalog entry doesn't list one.
func (a *sessionAgent) learnContextWindow(m Model, window int64) {
	if window <= 0 {
		return
	}
	a.learnedWindows.Store(contextWindowKey(m), window)
}

// contextWindow is the model's context window: the catalog value, or
// the one learned from an earlier overflow error when the catalog has
// none. 0 means unknown.
func (a *sessionAgent) contextWindow(m Model) int64 {
	if cw := int64(m.CatwalkCfg.ContextWindow); cw > 0 {
		return cw
	}
	if v, ok := a.learnedWindows.Load(contextWindowKey(m)); ok {
		if cw, ok := v.(int64); ok {
			return cw
		}
	}
	return 0
}

const (
	// summaryToolResultLimit is how many characters of a tool result
	// survive when a summary request has to be shrunk to fit.
	summaryToolResultLimit = 4000
	// summaryTextLimit is the same cap for plain text parts.
	summaryTextLimit = 20000
	// summaryHistoryShare is the slice of the context window the
	// trimmed history may use, leaving room for the summary prompt and
	// the summary itself.
	summaryHistoryShare = 0.5
)

// trimForSummary shrinks a history that overflowed the context window so
// a summary request can fit in tokenBudget tokens. Long tool results and
// text are cut down to their start and end, attachments are dropped, and
// if that is not enough the oldest turns go. It returns the new history
// and whether any messages were dropped. msgs is not modified.
func trimForSummary(msgs []fantasy.Message, tokenBudget int64) ([]fantasy.Message, bool) {
	out := cloneFantasyMessages(msgs)
	for i := range out {
		parts := out[i].Content[:0]
		for _, part := range out[i].Content {
			if trimmed, keep := trimSummaryPart(part); keep {
				parts = append(parts, trimmed)
			}
		}
		out[i].Content = parts
	}

	// Leading system messages always stay.
	start := 0
	for start < len(out) && out[start].Role == fantasy.MessageRoleSystem {
		start++
	}
	dropped := false
	for tokenBudget > 0 && estimateMessageTokens(out) > tokenBudget {
		// Prefer dropping a whole earlier turn: cut up to the next user
		// message after the first one.
		if j := nextRole(out, start+1, fantasy.MessageRoleUser); j > 0 {
			out = append(out[:start:start], out[j:]...)
			dropped = true
			continue
		}
		// One long turn: drop its oldest assistant step together with
		// the tool results that answer it, keeping the user prompt.
		first := nextRole(out, start+1, fantasy.MessageRoleAssistant)
		if first < 0 {
			break
		}
		next := nextRole(out, first+1, fantasy.MessageRoleAssistant)
		if next < 0 {
			break
		}
		out = append(out[:first:first], out[next:]...)
		dropped = true
	}
	return out, dropped
}

func nextRole(msgs []fantasy.Message, from int, role fantasy.MessageRole) int {
	for i := from; i < len(msgs); i++ {
		if msgs[i].Role == role {
			return i
		}
	}
	return -1
}

func trimSummaryPart(part fantasy.MessagePart) (fantasy.MessagePart, bool) {
	switch p := part.(type) {
	case fantasy.TextPart:
		p.Text = truncateMiddle(p.Text, summaryTextLimit)
		return p, true
	case *fantasy.TextPart:
		c := *p
		c.Text = truncateMiddle(c.Text, summaryTextLimit)
		return &c, true
	case fantasy.FilePart, *fantasy.FilePart:
		return fantasy.TextPart{Text: "[attachment left out]"}, true
	case fantasy.ToolResultPart:
		p.Output = trimToolOutput(p.Output)
		return p, true
	case *fantasy.ToolResultPart:
		c := *p
		c.Output = trimToolOutput(c.Output)
		return &c, true
	default:
		return part, true
	}
}

func trimToolOutput(output fantasy.ToolResultOutputContent) fantasy.ToolResultOutputContent {
	switch o := output.(type) {
	case fantasy.ToolResultOutputContentText:
		o.Text = truncateMiddle(o.Text, summaryToolResultLimit)
		return o
	case *fantasy.ToolResultOutputContentText:
		return fantasy.ToolResultOutputContentText{Text: truncateMiddle(o.Text, summaryToolResultLimit)}
	case fantasy.ToolResultOutputContentMedia, *fantasy.ToolResultOutputContentMedia:
		return fantasy.ToolResultOutputContentText{Text: "[media output left out]"}
	default:
		return output
	}
}

// truncateMiddle keeps the first two thirds and the last third of a
// string longer than limit characters.
func truncateMiddle(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	head := strings.ToValidUTF8(s[:limit*2/3], "")
	tail := strings.ToValidUTF8(s[len(s)-limit/3:], "")
	return head + "\n\n[… " + strconv.Itoa(len(s)-len(head)-len(tail)) + " characters left out …]\n\n" + tail
}

const (
	// defaultCompactionReserveTokens is the free context kept before the
	// conversation is summarized, on top of the reply the next request
	// reserves.
	defaultCompactionReserveTokens = 16384
	// defaultCompactionKeepRecentTokens is how much of the newest
	// conversation a summary keeps verbatim.
	defaultCompactionKeepRecentTokens = 20000
	// summaryTokenEstimate is the size a new summary is assumed to have
	// when the cut is chosen, before the summary exists.
	summaryTokenEstimate = 2048
)

// compactionSettings returns the reserve and keep-recent token counts
// from the options, or their defaults. The default reserve is scaled to
// a fifth of a known contextWindow, so a small window isn't mostly
// reserve; a configured reserve is used as is.
func (a *sessionAgent) compactionSettings(contextWindow int64) (reserve, keepRecent int64) {
	reserve, keepRecent = defaultCompactionReserve(contextWindow), defaultCompactionKeepRecentTokens
	if a.cfg == nil || a.cfg.Config() == nil || a.cfg.Config().Options == nil {
		return reserve, keepRecent
	}
	opts := a.cfg.Config().Options
	if opts.CompactionReserveTokens > 0 {
		reserve = int64(opts.CompactionReserveTokens)
	}
	if opts.CompactionKeepRecentTokens > 0 {
		keepRecent = int64(opts.CompactionKeepRecentTokens)
	}
	return reserve, keepRecent
}

// defaultCompactionReserve returns the default reserve for a context
// window: defaultCompactionReserveTokens, or a fifth of a smaller window.
func defaultCompactionReserve(contextWindow int64) int64 {
	if contextWindow > 0 {
		return min(defaultCompactionReserveTokens, contextWindow/5)
	}
	return defaultCompactionReserveTokens
}

// summarizeThreshold returns how many free tokens must remain in the
// context window before auto-summarize kicks in: the reserve plus the
// reply the next request asks for. Servers such as vLLM reject a request
// when prompt + max_tokens exceeds the window, so the reply has to fit
// too. The result is capped at half the window so a large max_tokens on
// a small model can't trigger a summarize loop.
func summarizeThreshold(contextWindow, maxOutputTokens, reserve int64) int64 {
	return min(max(reserve, 0)+max(maxOutputTokens, 0), contextWindow/2)
}

// compactionKeepRecent caps the verbatim tail at a quarter of the window,
// so the tail and its summary fit well below the summarize threshold.
func compactionKeepRecent(keepRecent, contextWindow int64) int64 {
	if contextWindow > 0 {
		return min(keepRecent, contextWindow/4)
	}
	return keepRecent
}

// compactionKeepBudget returns the most estimated tokens a summary may
// keep verbatim so that the prompt after it (fixedTokens of system prompt
// and tools, plus summaryTokens) still leaves threshold plus a quarter of
// the window free. Without that headroom a small window is summarized
// again a step or two later. It is negative when nothing can be kept, and
// unbounded when the window is unknown.
func compactionKeepBudget(contextWindow, threshold, fixedTokens, summaryTokens int64) int64 {
	if contextWindow <= 0 {
		return math.MaxInt64
	}
	return contextWindow - threshold - contextWindow/4 - fixedTokens - summaryTokens
}

// compactionCutPoint returns the index of the first message a summary
// keeps verbatim; the messages before it are summarized. It walks back
// from the newest message until keepRecent estimated tokens are kept,
// then moves the cut forward to the nearest user message, or failing
// that to any message, that doesn't separate a tool result from its call
// and keeps at most maxKept estimated tokens. When the whole history
// fits in keepRecent, the newer half is kept. It returns len(msgs),
// meaning summarize everything, when no such cut leaves anything to
// summarize. When skipFirst is set msgs[0] is the previous summary,
// which can't be the only thing summarized.
func compactionCutPoint(msgs []message.Message, keepRecent, maxKept int64, skipFirst bool) int {
	n := len(msgs)
	first := 0
	if skipFirst {
		first = 1
	}
	if n-first < 2 || keepRecent <= 0 || maxKept <= 0 {
		return n
	}
	sizes := make([]int64, n)
	var total int64
	for i := range msgs {
		sizes[i] = estimateMessageTokens(msgs[i].ToAIMessage())
		total += sizes[i]
	}
	// suffix[i] is the estimate of msgs[i:].
	suffix := make([]int64, n+1)
	for i := n - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + sizes[i]
	}
	if total <= keepRecent {
		keepRecent = total / 2
	}

	// Walk back to the oldest message the tail needs.
	start := n
	var kept int64
	for start > first && kept < keepRecent {
		start--
		kept += sizes[start]
	}

	// callAt maps each tool call to the message that made it.
	callAt := make(map[string]int)
	for i, m := range msgs {
		for _, tc := range m.ToolCalls() {
			callAt[tc.ID] = i
		}
	}
	// minCall[i] is the earliest message holding a call whose result is
	// at index i or later.
	minCall := make([]int, n+1)
	minCall[n] = n
	for i := n - 1; i >= 0; i-- {
		minCall[i] = minCall[i+1]
		for _, tr := range msgs[i].ToolResults() {
			if c, ok := callAt[tr.ToolCallID]; ok && c < minCall[i] {
				minCall[i] = c
			}
		}
	}
	valid := func(c int) bool {
		return msgs[c].Role != message.Tool &&
			minCall[c] >= c &&
			suffix[c] <= maxKept
	}

	for c := max(start, first+1); c < n; c++ {
		if msgs[c].Role == message.User && valid(c) {
			return c
		}
	}
	for c := max(start, first+1); c < n; c++ {
		if valid(c) {
			return c
		}
	}
	return n
}

// compactionBackdates returns the indexes of the summarized messages that
// must move back in time so the summary, dated keptFirst-1, sorts after
// all of them and before the kept messages (created_at has whole-second
// resolution, so the cut can split a second). Each one is re-dated to
// keptFirst-2.
func compactionBackdates(older []message.Message, keptFirst int64) []int {
	var idx []int
	for i := range older {
		if older[i].CreatedAt >= keptFirst-1 {
			idx = append(idx, i)
		}
	}
	return idx
}

// fileListTools are the tools whose file_path arguments are listed after
// a summary, and whether each one changes the file.
var fileListTools = map[string]bool{
	tools.ViewToolName:      false,
	tools.EditToolName:      true,
	tools.MultiEditToolName: true,
	tools.WriteToolName:     true,
}

const (
	// summaryFileListsMarker starts the block formatSummaryFileLists
	// appends. It is an HTML comment, so the rendered summary hides it.
	summaryFileListsMarker      = "<!-- tada:file-lists -->"
	summaryFilesReadHeading     = "Files read:"
	summaryFilesModifiedHeading = "Files modified:"
)

// summaryFileLists collects the files read and modified by the tool calls
// in msgs, plus the lists an earlier summary carried in prevSummary. A
// file that was modified is only listed as modified. Both lists are
// sorted.
func summaryFileLists(msgs []message.Message, prevSummary string) (read, modified []string) {
	readSet := make(map[string]struct{})
	modSet := make(map[string]struct{})
	prevRead, prevMod := parseSummaryFileLists(prevSummary)
	for _, p := range prevRead {
		readSet[p] = struct{}{}
	}
	for _, p := range prevMod {
		modSet[p] = struct{}{}
	}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls() {
			modifies, ok := fileListTools[tc.Name]
			if !ok {
				continue
			}
			var input struct {
				FilePath string `json:"file_path"`
			}
			if json.Unmarshal([]byte(tc.Input), &input) != nil || input.FilePath == "" {
				continue
			}
			if modifies {
				modSet[input.FilePath] = struct{}{}
			} else {
				readSet[input.FilePath] = struct{}{}
			}
		}
	}
	for p := range modSet {
		modified = append(modified, p)
		delete(readSet, p)
	}
	for p := range readSet {
		read = append(read, p)
	}
	slices.Sort(read)
	slices.Sort(modified)
	return read, modified
}

// formatSummaryFileLists renders the lists appended to a summary, after
// summaryFileListsMarker. It returns "" when both are empty.
func formatSummaryFileLists(read, modified []string) string {
	if len(read) == 0 && len(modified) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n" + summaryFileListsMarker)
	for _, l := range []struct {
		heading string
		paths   []string
	}{{summaryFilesReadHeading, read}, {summaryFilesModifiedHeading, modified}} {
		if len(l.paths) == 0 {
			continue
		}
		sb.WriteString("\n\n" + l.heading)
		for _, p := range l.paths {
			sb.WriteString("\n- " + p)
		}
	}
	return sb.String()
}

// parseSummaryFileLists reads back the lists formatSummaryFileLists
// appended to a summary. Only the block after the last marker counts,
// so lists the model wrote itself are ignored.
func parseSummaryFileLists(summary string) (read, modified []string) {
	i := strings.LastIndex(summary, summaryFileListsMarker)
	if i < 0 {
		return nil, nil
	}
	var cur *[]string
	for line := range strings.SplitSeq(summary[i+len(summaryFileListsMarker):], "\n") {
		switch {
		case line == summaryFilesReadHeading:
			cur = &read
		case line == summaryFilesModifiedHeading:
			cur = &modified
		case cur != nil && strings.HasPrefix(line, "- "):
			*cur = append(*cur, strings.TrimPrefix(line, "- "))
		default:
			cur = nil
		}
	}
	return read, modified
}

// stripSummaryFileLists removes the block formatSummaryFileLists appended
// to a summary, so the summarizer sees only the model's own text; the
// lists are carried forward by summaryFileLists instead.
func stripSummaryFileLists(summary string) string {
	i := strings.LastIndex(summary, summaryFileListsMarker)
	if i < 0 {
		return summary
	}
	return strings.TrimRight(summary[:i], "\n")
}
