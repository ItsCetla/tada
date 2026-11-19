package backend

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSetPersistent_RefusesShutdownIfIdle checks that a persistent
// backend (one hosting remote control) refuses a client's idle
// shutdown request, and grants it again once persistence is off.
func TestSetPersistent_RefusesShutdownIfIdle(t *testing.T) {
	t.Parallel()

	b, shutdowns := newTestBackend(t)
	b.SetPersistent(true)
	require.False(t, b.ShutdownIfIdle())
	require.Zero(t, shutdowns.Load())

	b.mu.Lock()
	require.False(t, b.closing, "refusing must not latch closing")
	b.mu.Unlock()

	b.SetPersistent(false)
	require.True(t, b.ShutdownIfIdle())
	require.Equal(t, int32(1), shutdowns.Load())
}

// TestSetPersistent_NoShutdownAfterLastWorkspace checks that the last
// workspace going away does not stop a persistent backend, with or
// without the idle linger.
func TestSetPersistent_NoShutdownAfterLastWorkspace(t *testing.T) {
	t.Parallel()

	for _, linger := range []time.Duration{0, 10 * time.Millisecond} {
		b, shutdowns := newTestBackend(t)
		b.SetIdleShutdownDelay(linger)
		b.SetPersistent(true)

		ws, wsShutdowns := insertTestWorkspace(t, b, t.TempDir())
		cid := newClientID(t)
		require.NoError(t, b.AttachClient(ws.ID, cid))
		b.DetachClient(ws.ID, cid)

		require.Eventually(t, func() bool { return wsShutdowns.Load() == 1 },
			2*time.Second, 5*time.Millisecond, "the workspace itself is torn down")
		require.Zero(t, b.workspaces.Len())

		// Give an armed linger timer time to fire if it was wrongly set.
		time.Sleep(10*linger + 20*time.Millisecond)
		require.Zero(t, shutdowns.Load(), "linger=%s", linger)

		b.mu.Lock()
		require.Nil(t, b.shutdownTimer)
		require.False(t, b.closing)
		b.mu.Unlock()
	}
}

// TestSetPersistent_CancelsArmedTimer checks that turning persistence on
// disarms an idle shutdown that was already pending.
func TestSetPersistent_CancelsArmedTimer(t *testing.T) {
	t.Parallel()

	b, shutdowns := newTestBackend(t)
	b.SetIdleShutdownDelay(50 * time.Millisecond)
	ws, _ := insertTestWorkspace(t, b, t.TempDir())
	cid := newClientID(t)
	require.NoError(t, b.AttachClient(ws.ID, cid))
	b.DetachClient(ws.ID, cid)

	b.mu.Lock()
	armed := b.shutdownTimer != nil
	b.mu.Unlock()
	require.True(t, armed, "the linger timer should be armed")

	b.SetPersistent(true)
	time.Sleep(150 * time.Millisecond)
	require.Zero(t, shutdowns.Load())
}

// TestShutdownWorkspaces_RemovesAll checks that every hosted workspace
// is shut down even while clients still hold it, that the server itself
// is not, and that further creates are refused.
func TestShutdownWorkspaces_RemovesAll(t *testing.T) {
	t.Parallel()

	b, shutdowns := newTestBackend(t)
	b.SetPersistent(true)
	ws1, s1 := insertTestWorkspace(t, b, t.TempDir())
	_, s2 := insertTestWorkspace(t, b, t.TempDir())
	b.registerClient(ws1, newClientID(t))

	b.ShutdownWorkspaces()

	require.Empty(t, b.ListWorkspaces())
	require.Equal(t, int32(1), s1.Load())
	require.Equal(t, int32(1), s2.Load())
	require.Zero(t, shutdowns.Load(), "only the workspaces stop, not the server")
	b.mu.Lock()
	require.Empty(t, b.pathIndex)
	b.mu.Unlock()

	_, _, err := b.CreateWorkspace(protoWS(t.TempDir(), t.TempDir(), newClientID(t)))
	require.ErrorIs(t, err, ErrServerShuttingDown)
}
