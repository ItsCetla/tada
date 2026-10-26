package rewind

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
)

// bashToolName matches tools.BashToolName without importing the tools
// package.
const bashToolName = "bash"

// Timeline lists the session's turns, oldest first.
func (s *Service) Timeline(ctx context.Context, sessionID string) ([]Turn, error) {
	if err := s.messages.FlushAll(ctx); err != nil {
		return nil, fmt.Errorf("flushing messages: %w", err)
	}
	msgs, err := s.messages.List(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	files, err := s.history.ListBySession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing file history: %w", err)
	}
	byCall := map[string][]permission.Action{}
	if s.actions != nil {
		for _, a := range s.actions(ctx) {
			if a.ToolCallID != "" {
				byCall[a.ToolCallID] = append(byCall[a.ToolCallID], a)
			}
		}
	}

	starts := turnStarts(msgs)
	at := newPlacer(msgs)
	turns := make([]Turn, 0, len(starts))
	for i, start := range starts {
		end := len(msgs)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		first := msgs[start]
		t := Turn{
			Index:     i + 1,
			MessageID: first.ID,
			Prompt:    promptOf(first),
			CreatedAt: first.CreatedAt,
			Files:     changedBetween(files, at, start, end),
		}
		for _, m := range msgs[start:end] {
			if m.HasShellCommand() {
				t.Bash = true
			}
			for _, tc := range m.ToolCalls() {
				if tc.Name == bashToolName {
					t.Bash = true
				}
				for _, a := range byCall[tc.ID] {
					if a.Class == "" {
						continue
					}
					if t.Actions == nil {
						t.Actions = map[permission.Class]int{}
					}
					t.Actions[a.Class]++
				}
			}
		}
		turns = append(turns, t)
	}
	return turns, nil
}

// turnStarts returns the indexes of the messages that start turns.
func turnStarts(msgs []message.Message) []int {
	var out []int
	for i, m := range msgs {
		if isTurnStart(m) {
			out = append(out, i)
		}
	}
	return out
}

// changedBetween returns the sorted paths with a version recorded from
// msgs[lo] up to, not including, msgs[hi].
func changedBetween(files []history.File, at placer, lo, hi int) []string {
	var out []string
	for _, f := range files {
		if !at.before(f, lo) && at.before(f, hi) {
			out = append(out, f.Path)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// placer puts history versions in the chat.
type placer struct {
	msgs []message.Message
	pos  map[string]int
}

func newPlacer(msgs []message.Message) placer {
	pos := make(map[string]int, len(msgs))
	for i, m := range msgs {
		pos[m.ID] = i
	}
	return placer{msgs: msgs, pos: pos}
}

// before reports whether f was recorded before msgs[i]; every version is
// before len(msgs). A version stamped with the message whose tool call
// wrote it is placed at that message. Older versions, and those whose
// message is gone, fall back to their timestamp, which only has second
// resolution: a tie goes to the later message.
func (p placer) before(f history.File, i int) bool {
	if i >= len(p.msgs) {
		return true
	}
	if j, ok := p.pos[f.MessageID]; ok && f.MessageID != "" {
		return j < i
	}
	return f.CreatedAt < p.msgs[i].CreatedAt
}

// byPath groups a session's versions by path, each group oldest first.
func byPath(files []history.File) map[string][]history.File {
	out := map[string][]history.File{}
	for _, f := range files {
		out[f.Path] = append(out[f.Path], f)
	}
	for _, rs := range out {
		slices.SortStableFunc(rs, func(a, b history.File) int {
			return cmp.Or(
				cmp.Compare(a.CreatedAt, b.CreatedAt),
				cmp.Compare(a.Version, b.Version),
				cmp.Compare(a.ID, b.ID),
			)
		})
	}
	return out
}
