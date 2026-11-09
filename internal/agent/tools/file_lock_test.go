package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// syncFileTracker is a concurrency-safe file tracker for parallel edit tests.
type syncFileTracker struct {
	mu       sync.Mutex
	reads    []string
	lastRead time.Time
}

func (m *syncFileTracker) RecordRead(ctx context.Context, sessionID, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads = append(m.reads, path)
}

func (m *syncFileTracker) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return m.lastRead
}

func (m *syncFileTracker) ListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.reads...), nil
}

func TestConcurrentEditsToSameFileAllLand(t *testing.T) {
	t.Parallel()

	const n = 16
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	var lines []string
	for i := range n {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: &syncFileTracker{lastRead: time.Now().Add(time.Hour)},
		workingDir:  dir,
	}

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			old := fmt.Sprintf("line %02d", i)
			var (
				resp fantasy.ToolResponse
				err  error
			)
			if i%2 == 0 {
				resp, err = replaceContent(edit, filePath, old, strings.ToUpper(old), false, fantasy.ToolCall{ID: "call"})
			} else {
				resp, err = processMultiEditExistingFile(edit, MultiEditParams{
					FilePath: filePath,
					Edits:    []MultiEditOperation{{OldString: old, NewString: strings.ToUpper(old)}},
				}, fantasy.ToolCall{ID: "call"})
			}
			require.NoError(t, err)
			require.False(t, resp.IsError, resp.Content)
		})
	}
	wg.Wait()

	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	for i := range n {
		require.Contains(t, string(content), fmt.Sprintf("LINE %02d", i))
	}
}

func TestFileLockKeyResolvesSymlinks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))
	link := filepath.Join(dir, "link.txt")
	require.NoError(t, os.Symlink(target, link))

	require.Equal(t, fileLockKey(target), fileLockKey(link))
	missing := filepath.Join(dir, "sub", "..", "missing.txt")
	require.Equal(t, fileLockKey(filepath.Join(dir, "missing.txt")), fileLockKey(missing))

	unlock := lockFile(link)
	unlock()
	unlock() // Releasing twice is a no-op.
}
