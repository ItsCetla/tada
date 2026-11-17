package agent

import (
	"context"
	"encoding/json"
	"log/slog"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
)

// shouldAutoLean reports whether a run should use the lean tool profile
// on its own: the user set no tool_profile, and the full prompt (system
// prompt and tool definitions) would take more than half of a known
// context window, leaving too little room for the conversation.
func shouldAutoLean(profile string, contextWindow, fullPromptTokens int64) bool {
	return profile == "" && contextWindow > 0 && fullPromptTokens > contextWindow/2
}

// estimatePromptTokens estimates the fixed part of every request: the
// system prompt and the tool definitions.
func estimatePromptTokens(systemPrompt string, agentTools []fantasy.AgentTool) int64 {
	tokens := approxTokenCount(systemPrompt)
	for _, t := range agentTools {
		info := t.Info()
		params, _ := json.Marshal(info.Parameters)
		tokens += approxTokenCount(info.Name) + approxTokenCount(info.Description) + approxTokenCount(string(params))
	}
	return tokens
}

// autoLean reports whether this run should trim its tools to the lean
// profile. The decision is made on a session's first run here and then
// kept, so tools never disappear mid-session (say, once MCP servers
// connect) after the history has called them. A session that already
// has replies stays full; the overflow handling covers it.
func (a *sessionAgent) autoLean(ctx context.Context, sessionID string, model Model, systemPrompt string, agentTools []fantasy.AgentTool) bool {
	if a.cfg == nil || a.cfg.Config() == nil || a.cfg.Config().Options == nil {
		return false
	}
	profile := a.cfg.Config().Options.ToolProfile
	if profile != "" {
		return false
	}
	return a.stickyLean(sessionID, func() bool {
		cw := a.contextWindow(model)
		if cw <= 0 {
			return false
		}
		// Size the tools the steps will actually send.
		full := estimatePromptTokens(systemPrompt, a.filterDisabledMCPTools(ctx, agentTools))
		if !shouldAutoLean(profile, cw, full) || a.sessionHasReplies(ctx, sessionID) {
			return false
		}
		if _, logged := a.autoLeanLogged.LoadOrStore(contextWindowKey(model), true); !logged {
			slog.Info("Using the lean tool profile, the full prompt would fill over half the context window",
				"provider", model.ModelCfg.Provider, "model", model.ModelCfg.Model,
				"context_window", cw, "prompt_tokens", full)
		}
		return true
	})
}

// stickyLean returns the lean decision stored for sessionID, or runs
// decide and stores its answer on the session's first call.
func (a *sessionAgent) stickyLean(sessionID string, decide func() bool) bool {
	if v, ok := a.leanSessions.Load(sessionID); ok {
		lean, _ := v.(bool)
		return lean
	}
	lean := decide()
	a.leanSessions.Store(sessionID, lean)
	return lean
}

// sessionHasReplies reports whether the session already has an assistant
// message, that is, whether it has run before with the full tools.
func (a *sessionAgent) sessionHasReplies(ctx context.Context, sessionID string) bool {
	msgs, err := a.messages.List(ctx, sessionID)
	if err != nil {
		return true
	}
	for _, msg := range msgs {
		if msg.Role == message.Assistant {
			return true
		}
	}
	return false
}

// leanToolSet keeps the tools the lean tool profile keeps.
func leanToolSet(cfg *config.Config, agentTools []fantasy.AgentTool) []fantasy.AgentTool {
	kept := make([]fantasy.AgentTool, 0, len(agentTools))
	for _, t := range agentTools {
		inner := t
		if h, ok := t.(*hookedTool); ok {
			inner = h.inner
		}
		var server string
		if m, ok := inner.(interface{ MCP() string }); ok {
			server = m.MCP()
		}
		if cfg.LeanToolKept(t.Info().Name, server) {
			kept = append(kept, t)
		}
	}
	return kept
}
