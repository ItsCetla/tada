package agent

import (
	"context"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// switchingModel runs onFirstStream before its first Stream call, so a
// test can switch the agent's models while a run is in flight.
type switchingModel struct {
	*overflowModel
	once          sync.Once
	onFirstStream func()
}

func (m *switchingModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.once.Do(func() {
		if m.onFirstStream != nil {
			m.onFirstStream()
		}
	})
	return m.overflowModel.Stream(ctx, call)
}

// A model switch made while a run is in flight (from another session)
// must not change that run: every later step, the auto-summary, and the
// re-run after it stay on the model the run started with. The next run
// picks up the new model.
func TestRun_ModelPinnedForInFlightRun(t *testing.T) {
	t.Parallel()

	small := Model{Model: &finishStreamModel{text: "title"}, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}}
	oldLM := &switchingModel{overflowModel: &overflowModel{overflows: 1}}
	newLM := &overflowModel{}
	oldModel := Model{Model: oldLM, ModelCfg: config.SelectedModel{Provider: "fake", Model: "old"}}
	newModel := Model{Model: newLM, ModelCfg: config.SelectedModel{Provider: "fake", Model: "new"}}

	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel: oldModel,
		SmallModel: small,
		IsYolo:     true,
		Sessions:   env.sessions,
		Messages:   env.messages,
	}).(*sessionAgent)
	oldLM.onFirstStream = func() { sa.SetModels(newModel, small) }

	busy, err := env.sessions.Create(t.Context(), "busy")
	require.NoError(t, err)
	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: busy.ID, Prompt: "fix the bug"})
	require.NoError(t, err)

	require.Len(t, oldLM.seen(), 3, "turn, summary, and retry all stay on the pinned model")
	require.Empty(t, newLM.seen(), "the switch must not reach the in-flight run")

	other, err := env.sessions.Create(t.Context(), "other")
	require.NoError(t, err)
	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: other.ID, Prompt: "hello"})
	require.NoError(t, err)
	require.Equal(t, []string{"hello"}, newLM.seen(), "a run started after the switch uses the new model")
}

// A call pinned to a model (the coordinator pins the model its provider
// options were built for) runs on it even if the agent's model changed
// while the call waited.
func TestRun_UsesCallPinnedModel(t *testing.T) {
	t.Parallel()

	pinnedLM := &overflowModel{}
	liveLM := &overflowModel{}
	pinned := Model{Model: pinnedLM, ModelCfg: config.SelectedModel{Provider: "fake", Model: "pinned"}}
	sa, env := newOverflowAgent(t, liveLM)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "hi", Model: &pinned})
	require.NoError(t, err)
	require.Equal(t, []string{"hi"}, pinnedLM.seen())
	require.Empty(t, liveLM.seen())
}

// After an auth refresh rebuilds the agent's models, a run whose model is
// still the selected one must pick up the rebuilt client; once the
// selection moved to another model, the run keeps its own.
func TestPinnedModelProvider(t *testing.T) {
	t.Parallel()

	pinnedLM := &overflowModel{}
	refreshedLM := &overflowModel{}
	otherLM := &overflowModel{}
	cfg := config.SelectedModel{Provider: "fake", Model: "m"}
	sa, _ := newOverflowAgent(t, pinnedLM)
	pinned := Model{Model: pinnedLM, ModelCfg: cfg}
	provide := sa.pinnedModelProvider(pinned)

	sa.SetModels(Model{Model: refreshedLM, ModelCfg: cfg}, Model{})
	require.Same(t, refreshedLM, provide(), "same model, rebuilt client: use the refreshed one")

	sa.SetModels(Model{Model: otherLM, ModelCfg: config.SelectedModel{Provider: "fake", Model: "other"}}, Model{})
	require.Same(t, pinnedLM, provide(), "different model selected: keep the pinned one")
}
