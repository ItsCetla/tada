package dialog

import (
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/rewind"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func newTestRewind(t *testing.T, messageID string) *Rewind {
	t.Helper()
	s := styles.CharmtonePantera()
	d := NewRewind(&common.Common{Styles: &s}, "s1", "/proj", messageID)
	d.width, d.height = 88, 22
	return d
}

func testTurns(now time.Time) []rewind.Turn {
	return []rewind.Turn{
		{Index: 1, MessageID: "u1", Prompt: "add a parser", CreatedAt: now.Add(-time.Hour).Unix(), Files: []string{"/proj/parse.go"}, Actions: map[permission.Class]int{permission.ClassWrite: 2}},
		{Index: 2, MessageID: "u2", Prompt: "fix the\nbuild", CreatedAt: now.Add(-time.Minute).Unix(), Files: []string{"/proj/main.go", "/etc/hosts"}, Bash: true},
		{Index: 3, MessageID: "u3", Prompt: "write docs", CreatedAt: now.Unix()},
	}
}

func TestRewindQuery(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"/rewind", " /UNDO ", "/checkpoint"} {
		require.True(t, RewindQuery(v), v)
	}
	for _, v := range []string{"rewind", "/rewind 3", "/resume"} {
		require.False(t, RewindQuery(v), v)
	}
}

func TestRewind_PreselectsAndRenders(t *testing.T) {
	t.Parallel()
	now := time.Now()
	d := newTestRewind(t, "u2")
	require.Contains(t, ansi.Strip(d.content(now)), "Loading")

	d.SetTimeline(testTurns(now), nil)
	require.Equal(t, 1, d.cursor)
	out := ansi.Strip(d.content(now))
	require.Contains(t, out, "#1")
	require.Contains(t, out, "add a parser")
	require.Contains(t, out, "fix the build")
	require.Contains(t, out, "$ shell")
	require.Contains(t, out, "main.go, /etc/hosts")
	require.Contains(t, out, "some of their changes may not be undone")
	for _, l := range splitLines(out) {
		require.LessOrEqual(t, ansi.StringWidth(l), 88, l)
	}

	// Without a preselection the latest turn is picked.
	d2 := newTestRewind(t, "")
	d2.SetTimeline(testTurns(now), nil)
	require.Equal(t, 2, d2.cursor)
}

func TestRewind_ConfirmNeedsPlanAndSecondEnter(t *testing.T) {
	t.Parallel()
	now := time.Now()
	d := newTestRewind(t, "u2")
	d.SetTimeline(testTurns(now), nil)

	act := d.HandleMsg(enter())
	plan, ok := act.(ActionRewindPlan)
	require.True(t, ok)
	require.True(t, plan.Request.DryRun)
	require.Equal(t, "u2", plan.Request.MessageID)
	require.Equal(t, rewind.ModeBoth, plan.Request.Mode)
	require.True(t, d.armed)

	// Enter before the plan arrives does nothing.
	require.Nil(t, d.HandleMsg(enter()))

	d.SetPlan(plan.Request, rewind.Result{
		Files: []rewind.FileChange{
			{Path: "/proj/main.go", Op: rewind.OpRestore},
			{Path: "/proj/new.go", Op: rewind.OpDelete},
			{Path: "/etc/hosts", Op: rewind.OpRestore, Skipped: true, Reason: "changed since Ta-Da last wrote it"},
		},
		MessagesDeleted: 4,
	}, nil)
	out := ansi.Strip(d.content(now))
	require.Contains(t, out, "restore  main.go")
	require.Contains(t, out, "delete   new.go")
	require.Contains(t, out, "skip     /etc/hosts")
	require.Contains(t, out, "Press ! to overwrite")
	require.Contains(t, out, "Removes 4 messages")
	require.Contains(t, out, "Press enter again")

	act = d.HandleMsg(enter())
	run, ok := act.(ActionRewind)
	require.True(t, ok)
	require.False(t, run.Request.DryRun)
	require.Equal(t, "s1", run.SessionID)
	require.Equal(t, "u2", run.Request.MessageID)

	// Keys are ignored while it runs; a failure shows and disarms.
	require.Nil(t, d.HandleMsg(enter()))
	d.SetResult(errors.New("boom"))
	require.False(t, d.armed)
	require.Contains(t, ansi.Strip(d.content(now)), "! boom")
}

func TestRewind_ChangesDisarmAndDropStalePlans(t *testing.T) {
	t.Parallel()
	now := time.Now()
	d := newTestRewind(t, "u2")
	d.SetTimeline(testTurns(now), nil)

	plan := d.HandleMsg(enter()).(ActionRewindPlan)
	d.HandleMsg(keyMsg('m'))
	require.False(t, d.armed)
	require.Equal(t, rewind.ModeChat, d.mode)

	// The plan for the old mode arrives late and is dropped.
	d.SetPlan(plan.Request, rewind.Result{}, nil)
	require.Nil(t, d.plan)

	d.HandleMsg(keyMsg('s'))
	d.HandleMsg(keyMsg('!'))
	req := d.HandleMsg(enter()).(ActionRewindPlan).Request
	require.Equal(t, rewind.ScopeOutside, req.Scope)
	require.True(t, req.Force)
	require.Equal(t, rewind.ModeChat, req.Mode)

	// Esc backs out of the confirmation before it closes.
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape}))
	require.False(t, d.armed)
	require.Equal(t, ActionClose{}, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape}))
}

func TestRewind_EmptyTimeline(t *testing.T) {
	t.Parallel()
	d := newTestRewind(t, "")
	d.SetTimeline(nil, nil)
	require.Contains(t, ansi.Strip(d.content(time.Now())), "no turns")
	require.Nil(t, d.HandleMsg(enter()))
}

func TestRewind_Fork(t *testing.T) {
	t.Parallel()
	now := time.Now()
	shiftF := tea.KeyPressMsg{Code: 'f', Mod: tea.ModShift, Text: "F"}

	// f copies everything before the selected turn.
	d := newTestRewind(t, "u2")
	d.SetTimeline(testTurns(now), nil)
	require.Equal(t, ActionForkSession{SessionID: "s1", AtMessageID: "u2"}, d.HandleMsg(keyMsg('f')))
	require.Contains(t, ansi.Strip(d.content(now)), "Forking")

	// Keys wait while it runs; a failure shows and frees them.
	require.Nil(t, d.HandleMsg(keyMsg('f')))
	d.SetForkResult(errors.New("boom"))
	require.Contains(t, ansi.Strip(d.content(now)), "! boom")

	// F copies through the selected turn: up to the next prompt.
	require.Equal(t, ActionForkSession{SessionID: "s1", AtMessageID: "u3"}, d.HandleMsg(shiftF))

	// F on the last turn copies the whole session.
	d2 := newTestRewind(t, "")
	d2.SetTimeline(testTurns(now), nil)
	require.Equal(t, ActionForkSession{SessionID: "s1"}, d2.HandleMsg(shiftF))

	// Nothing to fork before the timeline loads.
	d3 := newTestRewind(t, "")
	require.Nil(t, d3.HandleMsg(keyMsg('f')))
}
