package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

type mockPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
}

func (m *mockPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}

func (m *mockPermissionService) Grant(req permission.PermissionRequest) bool { return true }

func (m *mockPermissionService) Deny(req permission.PermissionRequest) bool { return true }

func (m *mockPermissionService) GrantPersistent(req permission.PermissionRequest) bool {
	return true
}

func (m *mockPermissionService) AutoApproveSession(sessionID string) {}

func (m *mockPermissionService) SessionAutoApproved(sessionID string) bool { return false }

func (m *mockPermissionService) ActiveRequest() (permission.PermissionRequest, bool) {
	return permission.PermissionRequest{}, false
}

func (m *mockPermissionService) SetSkipRequests(skip bool) {}

func (m *mockPermissionService) SkipRequests() bool {
	return false
}

func (m *mockPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

type mockHistoryService struct {
	*pubsub.Broker[history.File]
}

func (m *mockHistoryService) Create(ctx context.Context, sessionID, path, content string) (history.File, error) {
	return history.File{Path: path, Content: content}, nil
}

func (m *mockHistoryService) CreateNew(ctx context.Context, sessionID, path string) (history.File, error) {
	return history.File{Path: path, Origin: history.OriginAbsent}, nil
}

func (m *mockHistoryService) CreateAbsent(ctx context.Context, sessionID, path string) (history.File, error) {
	return history.File{Path: path, Origin: history.OriginAbsent}, nil
}

func (m *mockHistoryService) CreateVersion(ctx context.Context, sessionID, path, content string) (history.File, error) {
	return history.File{}, nil
}

func (m *mockHistoryService) GetByPathAndSession(ctx context.Context, path, sessionID string) (history.File, error) {
	return history.File{Path: path, Content: ""}, nil
}

func (m *mockHistoryService) Get(ctx context.Context, id string) (history.File, error) {
	return history.File{}, nil
}

func (m *mockHistoryService) ListBySession(ctx context.Context, sessionID string) ([]history.File, error) {
	return nil, nil
}

func (m *mockHistoryService) ListLatestSessionFiles(ctx context.Context, sessionID string) ([]history.File, error) {
	return nil, nil
}

func (m *mockHistoryService) Delete(ctx context.Context, id string) error {
	return nil
}

func (m *mockHistoryService) DeleteSessionFiles(ctx context.Context, sessionID string) error {
	return nil
}

func (m *mockHistoryService) Mark(ctx context.Context) (int64, error) {
	return 0, nil
}

func (m *mockHistoryService) ListPathSince(ctx context.Context, path string, mark int64) ([]history.File, error) {
	return nil, nil
}

// applyEditToContent applies a single multiedit operation, for tests.
func applyEditToContent(content string, edit MultiEditOperation) (string, bool, error) {
	result, failed, corrected := applyEditsToContent(content, []MultiEditOperation{edit}, 0)
	if len(failed) > 0 {
		return "", false, errors.New(failed[0].Error)
	}
	return result, corrected, nil
}

func TestApplyEditToContentPartialSuccess(t *testing.T) {
	t.Parallel()

	content := "line 1\nline 2\nline 3\n"

	// Test successful edit.
	newContent, _, err := applyEditToContent(content, MultiEditOperation{
		OldString: "line 1",
		NewString: "LINE 1",
	})
	require.NoError(t, err)
	require.Contains(t, newContent, "LINE 1")
	require.Contains(t, newContent, "line 2")

	// Test failed edit (string not found).
	_, _, err = applyEditToContent(content, MultiEditOperation{
		OldString: "line 99",
		NewString: "LINE 99",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestApplyEditToContentReplacementModes(t *testing.T) {
	t.Parallel()

	content := "alpha\nbeta\nalpha\n"

	newContent, _, err := applyEditToContent(content, MultiEditOperation{
		OldString:  "alpha",
		NewString:  "ALPHA",
		ReplaceAll: true,
	})
	require.NoError(t, err)
	require.Equal(t, "ALPHA\nbeta\nALPHA\n", newContent)

	_, _, err = applyEditToContent(content, MultiEditOperation{
		OldString: "alpha",
		NewString: "ALPHA",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "multiple times")
	require.Contains(t, err.Error(), "2 matches")

	_, _, err = applyEditToContent(content, MultiEditOperation{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot be empty")

	_, _, err = applyEditToContent(content, MultiEditOperation{OldString: "beta", NewString: "beta"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "identical")
}

func TestMultiEditMatchesAgainstOriginal(t *testing.T) {
	t.Parallel()

	content := "a\nb\nc\n"

	// A later edit cannot target text introduced by an earlier one, and an
	// earlier edit does not hide text from a later one.
	result, failed, _ := applyEditsToContent(content, []MultiEditOperation{
		{OldString: "a", NewString: "b"},
		{OldString: "b", NewString: "B"},
	}, 0)
	require.Empty(t, failed)
	require.Equal(t, "b\nB\nc\n", result)

	// Order does not matter.
	forward, failed, _ := applyEditsToContent(content, []MultiEditOperation{
		{OldString: "a", NewString: "A"},
		{OldString: "c", NewString: "C"},
	}, 0)
	require.Empty(t, failed)
	backward, failed, _ := applyEditsToContent(content, []MultiEditOperation{
		{OldString: "c", NewString: "C"},
		{OldString: "a", NewString: "A"},
	}, 0)
	require.Empty(t, failed)
	require.Equal(t, "A\nb\nC\n", forward)
	require.Equal(t, forward, backward)
}

func TestMultiEditAllOrNothing(t *testing.T) {
	t.Parallel()

	content := "line 1\nline 2\nline 3\nline 4\n"
	result, failed, _ := applyEditsToContent(content, []MultiEditOperation{
		{OldString: "line 1", NewString: "LINE 1"},
		{OldString: "line 99", NewString: "LINE 99"},
		{OldString: "line 3", NewString: "LINE 3"},
		{OldString: "line", NewString: "LINE"},
	}, 0)
	require.Equal(t, content, result, "no edit may apply when any fails")
	require.Len(t, failed, 2)
	require.Equal(t, 2, failed[0].Index)
	require.Contains(t, failed[0].Error, "not found")
	require.Equal(t, 4, failed[1].Index)
	require.Contains(t, failed[1].Error, "multiple times")
}

func TestMultiEditOverlap(t *testing.T) {
	t.Parallel()

	content := "func a() {\n\treturn 1\n}\n"
	result, failed, _ := applyEditsToContent(content, []MultiEditOperation{
		{OldString: "func a() {\n\treturn 1", NewString: "func b() {\n\treturn 1"},
		{OldString: "return 1\n}", NewString: "return 2\n}"},
	}, 0)
	require.Equal(t, content, result)
	require.Len(t, failed, 1)
	require.Equal(t, 2, failed[0].Index)
	require.Contains(t, failed[0].Error, "overlaps edit 1")

	// Adjacent ranges do not overlap.
	result, failed, _ = applyEditsToContent("abcd", []MultiEditOperation{
		{OldString: "ab", NewString: "AB"},
		{OldString: "cd", NewString: "CD"},
	}, 0)
	require.Empty(t, failed)
	require.Equal(t, "ABCD", result)
}

func TestMultiEditReplaceAllAndWhitespace(t *testing.T) {
	t.Parallel()

	content := "func main() {\n\tfoo()\n\tx := 1\n\tfoo()\n}\n"
	result, failed, corrected := applyEditsToContent(content, []MultiEditOperation{
		{OldString: "foo()", NewString: "bar()", ReplaceAll: true},
		{OldString: "    x := 1", NewString: "    x := 2"},
	}, 0)
	require.Empty(t, failed)
	require.True(t, corrected)
	require.Equal(t, "func main() {\n\tbar()\n\tx := 2\n\tbar()\n}\n", result)
}

func TestProcessMultiEditExistingFileFailureWritesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("one\ntwo\nthree\n"), 0o644))

	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: &mockEditFileTracker{lastRead: time.Now().Add(time.Second)},
		workingDir:  dir,
	}
	params := MultiEditParams{
		FilePath: filePath,
		Edits: []MultiEditOperation{
			{OldString: "two", NewString: "TWO"},
			{OldString: "missing", NewString: "MISSING"},
		},
	}

	resp, err := processMultiEditExistingFile(edit, params, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "no changes made: 1 of 2 edit(s) failed")
	require.Contains(t, resp.Content, "edit 2: old_string not found")

	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, "one\ntwo\nthree\n", string(content))

	var meta MultiEditResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 0, meta.EditsApplied)
	require.Len(t, meta.EditsFailed, 1)
	require.Equal(t, 2, meta.EditsFailed[0].Index)
}

func TestProcessMultiEditExistingFileSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("one\ntwo\nthree\n"), 0o644))

	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: &mockEditFileTracker{lastRead: time.Now().Add(time.Second)},
		workingDir:  dir,
	}
	params := MultiEditParams{
		FilePath: filePath,
		Edits: []MultiEditOperation{
			{OldString: "three", NewString: "THREE"},
			{OldString: "one", NewString: "ONE"},
		},
	}

	resp, err := processMultiEditExistingFile(edit, params, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Applied 2 edits")

	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, "ONE\ntwo\nTHREE\n", string(content))

	var meta MultiEditResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 2, meta.EditsApplied)
	require.Empty(t, meta.EditsFailed)
	require.Equal(t, "one\ntwo\nthree\n", meta.OldContent)
	require.Equal(t, "ONE\ntwo\nTHREE\n", meta.NewContent)
}

// rebasingPermissionService runs onRequest while the permission prompt is
// open, to simulate the file changing before the edit is written.
type rebasingPermissionService struct {
	*mockPermissionService
	onRequest func()
}

func (m *rebasingPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	m.onRequest()
	return true, nil
}

func TestProcessMultiEditRebaseReportsWrittenDiff(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("x\n"), 0o644))

	edit := editContext{
		ctx: context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &rebasingPermissionService{
			mockPermissionService: &mockPermissionService{},
			onRequest: func() {
				require.NoError(t, os.WriteFile(filePath, []byte("x\nx\nx\n"), 0o644))
			},
		},
		files:       &mockHistoryService{},
		filetracker: &mockEditFileTracker{lastRead: time.Now().Add(time.Second)},
		workingDir:  dir,
	}
	params := MultiEditParams{
		FilePath: filePath,
		Edits:    []MultiEditOperation{{OldString: "x", NewString: "X", ReplaceAll: true}},
	}

	resp, err := processMultiEditExistingFile(edit, params, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)

	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, "X\nX\nX\n", string(content))

	// The metadata describes what was written, not the approved preview.
	var meta MultiEditResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "x\nx\nx\n", meta.OldContent)
	require.Equal(t, "X\nX\nX\n", meta.NewContent)
	require.Equal(t, 3, meta.Additions)
	require.Equal(t, 3, meta.Removals)
}

func TestProcessMultiEditWithCreationFailureCreatesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "nested", "test.txt")

	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: &mockPermissionService{},
		files:       &mockHistoryService{},
		filetracker: &mockEditFileTracker{},
		workingDir:  dir,
	}
	params := MultiEditParams{
		FilePath: filePath,
		Edits: []MultiEditOperation{
			{OldString: "", NewString: "one\ntwo\nthree\n"},
			{OldString: "two", NewString: "TWO"},
			{OldString: "missing", NewString: "MISSING"},
		},
	}

	resp, err := processMultiEditWithCreation(edit, params, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "1 of 3 edit(s) failed")
	require.Contains(t, resp.Content, "edit 3:")
	require.NoFileExists(t, filePath)

	params.Edits = params.Edits[:2]
	resp, err = processMultiEditWithCreation(edit, params, fantasy.ToolCall{ID: "call"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "File created with 2 edits")
	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Equal(t, "one\nTWO\nthree\n", string(content))
}
