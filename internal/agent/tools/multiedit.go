package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/diff"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/workdir"
)

type MultiEditOperation struct {
	OldString  string `json:"old_string" description:"Exact text to replace"`
	NewString  string `json:"new_string" description:"Replacement text"`
	ReplaceAll bool   `json:"replace_all,omitempty" description:"Replace every match"`
}

type MultiEditParams struct {
	FilePath string               `json:"file_path" description:"Absolute file path"`
	Edits    []MultiEditOperation `json:"edits"`
}

type MultiEditPermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

// TargetFile returns the file the tool writes, for project-scoped
// permission rules.
func (p MultiEditPermissionsParams) TargetFile() string { return p.FilePath }

type FailedEdit struct {
	Index int                `json:"index"`
	Error string             `json:"error"`
	Edit  MultiEditOperation `json:"edit"`
}

type MultiEditResponseMetadata struct {
	Additions    int          `json:"additions"`
	Removals     int          `json:"removals"`
	OldContent   string       `json:"old_content,omitempty"`
	NewContent   string       `json:"new_content,omitempty"`
	EditsApplied int          `json:"edits_applied"`
	EditsFailed  []FailedEdit `json:"edits_failed,omitempty"`
}

const MultiEditToolName = "multiedit"

//go:embed multiedit.md
var multieditDescription string

func NewMultiEditTool(
	lspManager *lsp.Manager,
	permissions permission.Service,
	files history.Service,
	filetracker filetracker.Service,
	workingDir string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		MultiEditToolName,
		multieditDescription,
		func(ctx context.Context, params MultiEditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			workingDir := workdir.From(ctx, workingDir)
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}

			if len(params.Edits) == 0 {
				return fantasy.NewTextErrorResponse("at least one edit operation is required"), nil
			}

			params.FilePath = filepathext.SmartJoin(workingDir, params.FilePath)

			// Validate all edits before applying any
			if err := validateEdits(params.Edits); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			var response fantasy.ToolResponse
			var err error

			editCtx := editContext{ctx, permissions, files, filetracker, workingDir}
			// Handle file creation case (first edit has empty old_string)
			if len(params.Edits) > 0 && params.Edits[0].OldString == "" {
				response, err = processMultiEditWithCreation(editCtx, params, call)
			} else {
				response, err = processMultiEditExistingFile(editCtx, params, call)
			}

			if err != nil {
				return response, err
			}

			if response.IsError {
				return response, nil
			}

			// Notify LSP clients about the change
			notifyLSPs(ctx, lspManager, params.FilePath)

			// Wait for LSP diagnostics and add them to the response
			text := fmt.Sprintf("<result>\n%s\n</result>\n", response.Content)
			text += getDiagnostics(params.FilePath, lspManager)
			response.Content = text
			return response, nil
		},
	)
}

func validateEdits(edits []MultiEditOperation) error {
	for i, edit := range edits {
		// Only the first edit can have empty old_string (for file creation)
		if i > 0 && edit.OldString == "" {
			return fmt.Errorf("edit %d: only the first edit can have empty old_string (for file creation)", i+1)
		}
	}
	return nil
}

// editSpan is one byte range of the original content that an edit replaces.
type editSpan struct {
	start, end  int
	replacement string
	index       int // 1-based edit index, for error messages.
}

// applyEditsToContent matches every edit against the original content, then
// applies all replacements at once. Edits never see each other's results, so
// their order does not matter. If any edit fails to match, matches more than
// once without replace_all, changes nothing, or overlaps another edit, no
// edit is applied: the original content is returned along with every
// failure. It also reports whether any edit only matched after whitespace
// normalization.
func applyEditsToContent(content string, edits []MultiEditOperation, startIndex int) (string, []FailedEdit, bool) {
	var (
		failedEdits         []FailedEdit
		spans               []editSpan
		whitespaceCorrected bool
	)
	for i, edit := range edits {
		index := startIndex + i + 1
		editSpans, corrected, err := locateEdit(content, edit, index)
		if err != nil {
			failedEdits = append(failedEdits, FailedEdit{Index: index, Error: err.Error(), Edit: edit})
			continue
		}
		whitespaceCorrected = whitespaceCorrected || corrected
		spans = append(spans, editSpans...)
	}

	slices.SortStableFunc(spans, func(a, b editSpan) int { return a.start - b.start })
	overlapping := map[int]bool{}
	for i := 1; i < len(spans); i++ {
		prev, cur := spans[i-1], spans[i]
		// Compare against the furthest-reaching earlier span, so an edit
		// nested inside a non-adjacent span is still caught.
		for j := i - 2; j >= 0 && spans[j].end > prev.end; j-- {
			prev = spans[j]
		}
		if prev.end <= cur.start || overlapping[cur.index] {
			continue
		}
		overlapping[cur.index] = true
		failedEdits = append(failedEdits, FailedEdit{
			Index: cur.index,
			Error: fmt.Sprintf("edit %d overlaps edit %d in the original file. Merge them into one edit or target disjoint regions", cur.index, prev.index),
			Edit:  edits[cur.index-startIndex-1],
		})
	}

	if len(failedEdits) > 0 {
		slices.SortStableFunc(failedEdits, func(a, b FailedEdit) int { return a.Index - b.Index })
		return content, failedEdits, false
	}

	// Replace from the end of the file backward so earlier offsets stay
	// valid.
	result := content
	for _, s := range slices.Backward(spans) {
		result = result[:s.start] + s.replacement + result[s.end:]
	}
	return result, nil, whitespaceCorrected
}

// locateEdit finds the ranges of content that edit replaces, using the same
// exact-then-whitespace-normalized matching as the edit tool. The returned
// boolean reports whether the match relied on whitespace normalization.
func locateEdit(content string, edit MultiEditOperation, index int) ([]editSpan, bool, error) {
	old := edit.OldString
	if old == "" {
		return nil, false, errors.New("old_string cannot be empty for content replacement")
	}
	if old == edit.NewString {
		return nil, false, errors.New("old_string and new_string are identical, so this edit would change nothing")
	}

	var exact []editSpan
	for from := 0; ; {
		i := strings.Index(content[from:], old)
		if i == -1 {
			break
		}
		start := from + i
		exact = append(exact, editSpan{start: start, end: start + len(old), replacement: edit.NewString, index: index})
		from = start + len(old)
	}
	if len(exact) > 0 {
		if len(exact) > 1 && !edit.ReplaceAll {
			return nil, false, ambiguousError(len(exact))
		}
		return exact, false, nil
	}

	matches := findNormalizedMatches(content, old)
	if len(matches) == 0 {
		return nil, false, notFoundError(content, old)
	}
	if len(matches) > 1 && !edit.ReplaceAll {
		return nil, false, ambiguousError(len(matches))
	}

	lines := strings.Split(content, "\n")
	lineStarts := make([]int, len(lines))
	offset := 0
	for i, l := range lines {
		lineStarts[i] = offset
		offset += len(l) + 1
	}
	fileUnit := detectIndentUnit(lines)
	spans := make([]editSpan, 0, len(matches))
	for _, m := range matches {
		start := lineStarts[m.startLine]
		end := lineStarts[m.endLine] + len(lines[m.endLine])
		spans = append(spans, editSpan{
			start:       start,
			end:         end,
			replacement: adaptIndentation(content[start:end], old, edit.NewString, fileUnit),
			index:       index,
		})
	}
	return spans, true, nil
}

func ambiguousError(n int) error {
	return fmt.Errorf("old_string appears multiple times in the file (%d matches). Please provide more context to ensure a unique match, or set replace_all to true", n)
}

// failedEditsMessage describes every failed edit so the caller can fix them
// all in one retry.
func failedEditsMessage(failedEdits []FailedEdit, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "no changes made: %d of %d edit(s) failed. Edits are matched against the original file and applied all-or-nothing, so fix the failed edits and resend the whole batch.", len(failedEdits), total)
	for _, f := range failedEdits {
		fmt.Fprintf(&b, "\n\nedit %d: %s", f.Index, f.Error)
	}
	return b.String()
}

func processMultiEditWithCreation(edit editContext, params MultiEditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	// First edit creates the file
	firstEdit := params.Edits[0]
	if firstEdit.OldString != "" {
		return fantasy.NewTextErrorResponse("first edit must have empty old_string for file creation"), nil
	}

	// Check if file already exists
	if _, err := os.Stat(params.FilePath); err == nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file already exists: %s", params.FilePath)), nil
	} else if !os.IsNotExist(err) {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
	}

	currentContent, failedEdits, whitespaceCorrected := applyEditsToContent(firstEdit.NewString, params.Edits[1:], 1)
	if len(failedEdits) > 0 {
		return failedEditsResponse(failedEdits, len(params.Edits)), nil
	}

	// Create parent directories
	dir := filepath.Dir(params.FilePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to create parent directories: %w", err)
	}

	// Get session and message IDs
	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for creating a new file")
	}

	// Check permissions
	_, additions, removals := diff.GenerateDiff("", currentContent, strings.TrimPrefix(params.FilePath, edit.workingDir))

	editsApplied := len(params.Edits)
	p, err := edit.permissions.Request(edit.ctx, permission.CreatePermissionRequest{
		SessionID:   sessionID,
		Path:        fsext.PathOrPrefix(params.FilePath, edit.workingDir),
		ToolCallID:  call.ID,
		ToolName:    MultiEditToolName,
		Action:      "write",
		Description: fmt.Sprintf("Create file %s with %d edits", params.FilePath, editsApplied),
		Params: MultiEditPermissionsParams{
			FilePath:   params.FilePath,
			OldContent: "",
			NewContent: currentContent,
		},
	})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		resp := NewPermissionDeniedResponse()
		resp = fantasy.WithResponseMetadata(resp, MultiEditResponseMetadata{
			OldContent:   "",
			NewContent:   currentContent,
			Additions:    additions,
			Removals:     removals,
			EditsApplied: editsApplied,
		})
		return resp, nil
	}

	unlock := lockFile(params.FilePath)
	defer unlock()

	// Another call may have created the file while this one waited for
	// permission.
	if _, err := os.Stat(params.FilePath); err == nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file already exists: %s", params.FilePath)), nil
	}

	// Write the file
	err = os.WriteFile(params.FilePath, []byte(currentContent), 0o644)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}

	// Update file history
	_, err = edit.files.CreateNew(edit.ctx, sessionID, params.FilePath)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
	}

	_, err = edit.files.CreateVersion(edit.ctx, sessionID, params.FilePath, currentContent)
	if err != nil {
		slog.Error("Error creating file history version", "error", err)
	}

	edit.filetracker.RecordRead(edit.ctx, sessionID, params.FilePath)

	message := fmt.Sprintf("File created with %d edits: %s", len(params.Edits), params.FilePath)
	message = withWhitespaceNote(message, whitespaceCorrected)

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(message),
		MultiEditResponseMetadata{
			OldContent:   "",
			NewContent:   currentContent,
			Additions:    additions,
			Removals:     removals,
			EditsApplied: editsApplied,
		},
	), nil
}

func processMultiEditExistingFile(edit editContext, params MultiEditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID, oldContent, _, resp, err := loadExistingFile(edit, params.FilePath, "session ID is required for editing a file")
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if resp.Content != "" || resp.IsError {
		return resp, nil
	}

	currentContent, failedEdits, whitespaceCorrected := applyEditsToContent(oldContent, params.Edits, 0)
	if len(failedEdits) > 0 {
		return failedEditsResponse(failedEdits, len(params.Edits)), nil
	}
	if oldContent == currentContent {
		return fantasy.NewTextErrorResponse("no changes made - all edits resulted in identical content"), nil
	}

	// Generate diff and check permissions
	_, additions, removals := diff.GenerateDiff(oldContent, currentContent, strings.TrimPrefix(params.FilePath, edit.workingDir))

	editsApplied := len(params.Edits)
	p, err := edit.permissions.Request(edit.ctx, permission.CreatePermissionRequest{
		SessionID:   sessionID,
		Path:        fsext.PathOrPrefix(params.FilePath, edit.workingDir),
		ToolCallID:  call.ID,
		ToolName:    MultiEditToolName,
		Action:      "write",
		Description: fmt.Sprintf("Apply %d edits to file %s", editsApplied, params.FilePath),
		Params: MultiEditPermissionsParams{
			FilePath:   params.FilePath,
			OldContent: oldContent,
			NewContent: currentContent,
		},
	})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		resp := NewPermissionDeniedResponse()
		resp = fantasy.WithResponseMetadata(resp, MultiEditResponseMetadata{
			OldContent:   oldContent,
			NewContent:   currentContent,
			Additions:    additions,
			Removals:     removals,
			EditsApplied: editsApplied,
		})
		return resp, nil
	}

	approvedOld := oldContent
	oldContent, _, resp, err = commitLocked(edit, params.FilePath, approvedOld, currentContent, func(current string) (string, error) {
		result, failed, _ := applyEditsToContent(current, params.Edits, 0)
		if len(failed) > 0 {
			return "", errors.New(failedEditsMessage(failed, len(params.Edits)))
		}
		return result, nil
	})
	if err != nil || resp.IsError {
		return resp, err
	}
	if oldContent != approvedOld {
		// The file changed after approval and the edits were re-applied, so
		// the diff stats and whitespace note must describe what was written.
		currentContent, _, whitespaceCorrected = applyEditsToContent(oldContent, params.Edits, 0)
		_, additions, removals = diff.GenerateDiff(oldContent, currentContent, strings.TrimPrefix(params.FilePath, edit.workingDir))
	}

	message := fmt.Sprintf("Applied %d edits to file: %s", len(params.Edits), params.FilePath)
	message = withWhitespaceNote(message, whitespaceCorrected)

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(message),
		MultiEditResponseMetadata{
			OldContent:   oldContent,
			NewContent:   currentContent,
			Additions:    additions,
			Removals:     removals,
			EditsApplied: editsApplied,
		},
	), nil
}

// failedEditsResponse is the error returned when any edit fails; nothing is
// written.
func failedEditsResponse(failedEdits []FailedEdit, total int) fantasy.ToolResponse {
	return fantasy.WithResponseMetadata(
		fantasy.NewTextErrorResponse(failedEditsMessage(failedEdits, total)),
		MultiEditResponseMetadata{EditsFailed: failedEdits},
	)
}
