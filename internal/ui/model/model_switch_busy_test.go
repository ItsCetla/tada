package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/ui/util"
)

func (w *testWorkspace) UpdatePreferredModel(_ config.Scope, typ config.SelectedModelType, m config.SelectedModel) error {
	if w.cfg.Models == nil {
		w.cfg.Models = map[config.SelectedModelType]config.SelectedModel{}
	}
	w.cfg.Models[typ] = m
	return nil
}

// newModelSwitchUI returns a chat UI on session sessionID with one
// configured provider and both models selected.
func newModelSwitchUI(t *testing.T, sessionID string) (*UI, *testWorkspace) {
	t.Helper()
	u, ws := newPlanUI(t, sessionID)
	u.state = uiChat
	u.userThemeSelected = true
	ws.cfg.Providers.Set("acme", config.ProviderConfig{ID: "acme"})
	ws.cfg.Models = map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeLarge: {Provider: "acme", Model: "old"},
		config.SelectedModelTypeSmall: {Provider: "acme", Model: "small"},
	}
	ws.cfg.Agents = map[string]config.Agent{
		config.AgentCoder: {ID: config.AgentCoder, Model: config.SelectedModelTypeLarge},
	}
	return u, ws
}

func selectLarge(u *UI, model string) tea.Cmd {
	return u.handleSelectModel(dialog.ActionSelectModel{
		Model:     config.SelectedModel{Provider: "acme", Model: model},
		ModelType: config.SelectedModelTypeLarge,
	})
}

// isBusyWarning runs cmd and reports whether it is the "agent is busy"
// warning the model switch guards return.
func isBusyWarning(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	info, ok := cmd().(util.InfoMsg)
	return ok && info.Type == util.InfoTypeWarn && info.Msg == "Agent is busy, please wait..."
}

// Another session's run makes the agent busy, but the session in view is
// idle (probed): switching its model must go through.
func TestSelectModel_AllowedWhileAnotherSessionRuns(t *testing.T) {
	t.Parallel()
	u, ws := newModelSwitchUI(t, "idle")
	u.agentBusyCache.set(true)
	u.probedSession, u.probedSessionBusy = "idle", false

	require.False(t, isBusyWarning(selectLarge(u, "new")))
	require.Equal(t, "new", ws.cfg.Models[config.SelectedModelTypeLarge].Model)
}

// A brand-new chat has no session yet; another session's run must not
// block picking a model for it.
func TestSelectModel_AllowedInNewChatWhileAnotherSessionRuns(t *testing.T) {
	t.Parallel()
	u, ws := newModelSwitchUI(t, "")
	u.agentBusyCache.set(true)

	require.False(t, isBusyWarning(selectLarge(u, "new")))
	require.Equal(t, "new", ws.cfg.Models[config.SelectedModelTypeLarge].Model)
}

// The session in view is itself running: the switch stays blocked.
func TestSelectModel_BlockedWhileOwnSessionRuns(t *testing.T) {
	t.Parallel()
	u, ws := newModelSwitchUI(t, "busy")
	u.agentBusyCache.set(true)
	u.probedSession, u.probedSessionBusy = "busy", true

	require.True(t, isBusyWarning(selectLarge(u, "new")))
	require.Equal(t, "old", ws.cfg.Models[config.SelectedModelTypeLarge].Model)
}

func TestSelectReasoningEffort_FollowsSessionBusy(t *testing.T) {
	t.Parallel()

	u, ws := newModelSwitchUI(t, "idle")
	u.agentBusyCache.set(true)
	u.probedSession, u.probedSessionBusy = "idle", false
	require.False(t, isBusyWarning(u.selectReasoningEffort("high")))
	require.Equal(t, "high", ws.cfg.Models[config.SelectedModelTypeLarge].ReasoningEffort)

	u.probedSessionBusy = true
	require.True(t, isBusyWarning(u.selectReasoningEffort("low")))
	require.Equal(t, "high", ws.cfg.Models[config.SelectedModelTypeLarge].ReasoningEffort)
}
