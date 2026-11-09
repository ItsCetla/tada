package message

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestSetCreatedAtPublishesUpdate(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	subCtx, cancelSub := context.WithCancel(t.Context())
	defer cancelSub()
	collector := collect(subCtx, svc.Subscribe(subCtx))

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	collector.dropCreated(t)

	// A pending, not yet flushed delta is published with the new date
	// and its newer content.
	msg.AppendContent("partial")
	require.NoError(t, svc.Update(t.Context(), msg))
	require.NoError(t, svc.SetCreatedAt(t.Context(), msg.ID, 42))
	require.Eventually(t, func() bool {
		return len(collector.snapshot()) >= 1
	}, time.Second, 5*time.Millisecond)
	ev := collector.snapshot()[0]
	require.Equal(t, pubsub.UpdatedEvent, ev.Type)
	require.Equal(t, int64(42), ev.Payload.CreatedAt)
	require.Equal(t, "partial", ev.Payload.Content().Text)

	// With nothing pending the stored row is published.
	require.NoError(t, svc.Flush(t.Context(), msg.ID))
	require.Eventually(t, func() bool {
		return len(collector.snapshot()) >= 2
	}, time.Second, 5*time.Millisecond)
	collector.reset()
	require.NoError(t, svc.SetCreatedAt(t.Context(), msg.ID, 7))
	require.Eventually(t, func() bool {
		return len(collector.snapshot()) >= 1
	}, time.Second, 5*time.Millisecond)
	ev = collector.snapshot()[0]
	require.Equal(t, pubsub.UpdatedEvent, ev.Type)
	require.Equal(t, int64(7), ev.Payload.CreatedAt)
	require.Equal(t, "partial", ev.Payload.Content().Text)

	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, int64(7), got.CreatedAt)
}
