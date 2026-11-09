package agent

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/fleet"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

type AgentParams struct {
	Prompt  string `json:"prompt" description:"Task for the agent"`
	Machine string `json:"machine,omitempty" description:"auto (default), main or a machine name"`
}

const (
	AgentToolName = "agent"
)

func (c *coordinator) agentTool(ctx context.Context) (fantasy.AgentTool, error) {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent not configured")
	}
	prompt, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	agent, err := c.buildAgent(ctx, prompt, agentCfg, true)
	if err != nil {
		return nil, err
	}
	machines := newMachineAgents(c, prompt, agentCfg)
	return fantasy.NewParallelAgentTool(
		AgentToolName,
		agentToolDescription+machinesToolHint(c.cfg.Config()),
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}

			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			sub := subAgentParams{
				Agent:          agent,
				SessionID:      sessionID,
				AgentMessageID: agentMessageID,
				ToolCallID:     call.ID,
				Prompt:         params.Prompt,
				SessionTitle:   "New Agent Session",
			}
			run, err := c.pickMachine(ctx, machines, params.Machine)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if run.agent == nil {
				return c.runSubAgent(ctx, sub)
			}
			onMachine := sub
			onMachine.Agent = run.agent
			onMachine.SessionTitle = "Agent on " + run.machine.Name
			resp, err := c.runSubAgent(ctx, onMachine)
			run.lease.Release()
			if err != nil {
				return resp, err
			}
			// A machine that drops mid-run shouldn't sink the task when
			// the caller let Ta-Da pick: redo it here.
			if resp.IsError && ctx.Err() == nil && isAutoMachine(params.Machine) {
				slog.Warn("Machine agent failed, running on main", "machine", run.machine.Name, "error", resp.Content)
				sub.ToolCallID = call.ID + "-main"
				return c.runSubAgent(ctx, sub)
			}
			return machineResult(resp, run.machine), nil
		},
	), nil
}

func isAutoMachine(name string) bool {
	name = strings.TrimSpace(name)
	return name == "" || strings.EqualFold(name, fleet.Auto)
}
