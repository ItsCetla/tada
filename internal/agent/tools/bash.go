package tools

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/sandbox"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/snapshot"
	"github.com/charmbracelet/crush/internal/workdir"
)

type BashParams struct {
	Description         string `json:"description" description:"What the command does, in a few words"`
	Command             string `json:"command" description:"Command to run"`
	WorkingDir          string `json:"working_dir,omitempty" description:"Working directory (default: current)"`
	RunInBackground     bool   `json:"run_in_background,omitempty" description:"Run as a background job"`
	AutoBackgroundAfter int    `json:"auto_background_after,omitempty" description:"Seconds before moving to the background (default 60)"`
}

type BashPermissionsParams struct {
	Description         string `json:"description"`
	Command             string `json:"command"`
	WorkingDir          string `json:"working_dir"`
	RunInBackground     bool   `json:"run_in_background"`
	AutoBackgroundAfter int    `json:"auto_background_after"`
}

// TargetCommand returns the command so the permission service can grade
// it.
func (p BashPermissionsParams) TargetCommand() string { return p.Command }

// TargetWorkdir returns the directory the command runs in.
func (p BashPermissionsParams) TargetWorkdir() string { return p.WorkingDir }

type BashResponseMetadata struct {
	StartTime        int64  `json:"start_time"`
	EndTime          int64  `json:"end_time"`
	Output           string `json:"output"`
	Description      string `json:"description"`
	WorkingDirectory string `json:"working_directory"`
	Background       bool   `json:"background,omitempty"`
	ShellID          string `json:"shell_id,omitempty"`
	// ExitCode is the command's exit status. It's nil while the command
	// still runs in the background and when it was interrupted.
	ExitCode *int `json:"exit_code,omitempty"`
}

const (
	BashToolName = "bash"

	DefaultAutoBackgroundAfter = 60 // Commands taking longer automatically become background jobs
	MaxOutputLength            = 30000
	BashNoOutput               = "no output"
)

//go:embed bash.md.tpl
var bashDescriptionTmpl []byte

var bashDescriptionTpl = template.Must(
	template.New("bashDescription").
		Parse(string(bashDescriptionTmpl)),
)

type bashDescriptionData struct {
	BannedCommands             string
	MaxOutputLength            int
	DefaultAutoBackgroundAfter int
	Attribution                config.Attribution
	ModelID                    string
	RgAvailable                bool
	GhAvailable                bool
}

var bannedCommands = []string{
	// Network/Download tools
	"alias",
	"aria2c",
	"axel",
	"chrome",
	"curl",
	"curlie",
	"firefox",
	"http-prompt",
	"httpie",
	"links",
	"lynx",
	"nc",
	"safari",
	"scp",
	"ssh",
	"telnet",
	"w3m",
	"wget",
	"xh",

	// System administration
	"doas",
	"su",
	"sudo",

	// Package managers
	"apk",
	"apt",
	"apt-cache",
	"apt-get",
	"dnf",
	"dpkg",
	"emerge",
	"home-manager",
	"makepkg",
	"opkg",
	"pacman",
	"paru",
	"pkg",
	"pkg_add",
	"pkg_delete",
	"portage",
	"rpm",
	"yay",
	"yum",
	"zypper",

	// System modification
	"at",
	"batch",
	"chkconfig",
	"crontab",
	"fdisk",
	"mkfs",
	"mount",
	"parted",
	"service",
	"systemctl",
	"umount",

	// Network configuration
	"firewall-cmd",
	"ifconfig",
	"ip",
	"iptables",
	"netstat",
	"pfctl",
	"route",
	"ufw",
}

func bashDescription(attribution *config.Attribution, modelID string) string {
	bannedCommandsStr := strings.Join(bannedCommands, ", ")
	var out bytes.Buffer
	if err := bashDescriptionTpl.Execute(&out, bashDescriptionData{
		BannedCommands:             bannedCommandsStr,
		MaxOutputLength:            MaxOutputLength,
		DefaultAutoBackgroundAfter: DefaultAutoBackgroundAfter,
		Attribution:                *attribution,
		ModelID:                    modelID,
		RgAvailable:                getRg() != "",
		GhAvailable:                ghAvailable,
	}); err != nil {
		// this should never happen.
		panic("failed to execute bash description template: " + err.Error())
	}
	return out.String()
}

func blockFuncs() []shell.BlockFunc {
	return []shell.BlockFunc{
		shell.CommandsBlocker(bannedCommands),

		// System package managers
		shell.ArgumentsBlocker("apk", []string{"add"}, nil),
		shell.ArgumentsBlocker("apt", []string{"install"}, nil),
		shell.ArgumentsBlocker("apt-get", []string{"install"}, nil),
		shell.ArgumentsBlocker("dnf", []string{"install"}, nil),
		shell.ArgumentsBlocker("pacman", nil, []string{"-S"}),
		shell.ArgumentsBlocker("pkg", []string{"install"}, nil),
		shell.ArgumentsBlocker("yum", []string{"install"}, nil),
		shell.ArgumentsBlocker("zypper", []string{"install"}, nil),

		// Language-specific package managers
		shell.ArgumentsBlocker("brew", []string{"install"}, nil),
		shell.ArgumentsBlocker("cargo", []string{"install"}, nil),
		shell.ArgumentsBlocker("gem", []string{"install"}, nil),
		shell.ArgumentsBlocker("go", []string{"install"}, nil),
		shell.ArgumentsBlocker("npm", []string{"install"}, []string{"--global"}),
		shell.ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}),
		shell.ArgumentsBlocker("pip", []string{"install"}, []string{"--user"}),
		shell.ArgumentsBlocker("pip3", []string{"install"}, []string{"--user"}),
		shell.ArgumentsBlocker("pnpm", []string{"add"}, []string{"--global"}),
		shell.ArgumentsBlocker("pnpm", []string{"add"}, []string{"-g"}),
		shell.ArgumentsBlocker("yarn", []string{"global", "add"}, nil),

		// `go test -exec` can run arbitrary commands
		shell.ArgumentsBlocker("go", []string{"test"}, []string{"-exec"}),
	}
}

// NewBashTool returns the bash tool. dataDir is Ta-Da's data directory:
// oversized output spills there, and when files is not nil it holds the
// snapshots that record foreground commands' file changes for rewind.
// sandboxSettings confines commands on macOS; see internal/sandbox.
func NewBashTool(permissions permission.Service, files history.Service, workingDir, dataDir string, attribution *config.Attribution, modelID string, sandboxSettings sandbox.Settings) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		BashToolName,
		string(bashDescription(attribution, modelID)),
		func(ctx context.Context, params BashParams, call fantasy.ToolCall) (resp fantasy.ToolResponse, err error) {
			workingDir := workdir.From(ctx, workingDir)
			if params.Command == "" {
				return fantasy.NewTextErrorResponse("missing command"), nil
			}

			// Determine working directory
			execWorkingDir := cmp.Or(params.WorkingDir, workingDir)

			// Refuse banned commands wherever they appear in the command,
			// including behind wrappers (env curl, sh -c 'curl ...'). The
			// shell's block list re-checks at run time with expanded
			// arguments.
			if banned := bannedCommandIn(params.Command); banned != "" {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("command is not allowed for security reasons: %q", banned)), nil
			}

			// Read-only commands skip the prompt only in the session's
			// working directory: safe-list path checks are relative to it,
			// and git must not be run inside a repository-shaped
			// subdirectory whose config could run programs.
			isSafeReadOnly := sameDir(execWorkingDir, workingDir) && isSafeReadOnlyCommand(params.Command)

			// The sandbox is built per call because the working directory
			// can change between calls.
			projectRoot := config.ProjectRoot(workingDir)
			policy := sandbox.New(sandboxSettings, projectRoot, workingDir, dataDir)

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for executing shell command")
			}
			if !isSafeReadOnly {
				p, err := permissions.Request(
					ctx,
					permission.CreatePermissionRequest{
						SessionID:   sessionID,
						Path:        execWorkingDir,
						ToolCallID:  call.ID,
						ToolName:    BashToolName,
						Action:      "execute",
						Description: fmt.Sprintf("Execute command: %s", params.Command),
						Params:      BashPermissionsParams(params),
					},
				)
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if !p {
					return NewPermissionDeniedResponse(), nil
				}
			}

			// If explicitly requested as background, start immediately with detached context
			if params.RunInBackground {
				startTime := time.Now()
				bgManager := shell.GetBackgroundShellManager()
				bgManager.Cleanup()
				// Use background context so it continues after tool returns
				bgShell, err := bgManager.StartSandboxed(context.Background(), execWorkingDir, blockFuncs(), policy, params.Command, params.Description)
				if err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("error starting background shell: %w", err)
				}

				// Wait a short time to detect fast failures (blocked commands, syntax errors, etc.)
				time.Sleep(1 * time.Second)
				stdout, stderr, done, execErr := bgShell.GetOutput()

				if done {
					// Command failed or completed very quickly
					bgManager.Remove(bgShell.ID)

					interrupted := shell.IsInterrupt(execErr)
					exitCode := shell.ExitCode(execErr)
					if exitCode == 0 && !interrupted && execErr != nil {
						return fantasy.ToolResponse{}, fmt.Errorf("[Job %s] error executing command: %w", bgShell.ID, execErr)
					}

					stdout = formatOutput(stdout, stderr, execErr, dataDir)
					stdout = addSandboxNote(policy, stdout, execErr)

					metadata := BashResponseMetadata{
						StartTime:        startTime.UnixMilli(),
						EndTime:          time.Now().UnixMilli(),
						Output:           stdout,
						Description:      params.Description,
						Background:       params.RunInBackground,
						WorkingDirectory: bgShell.WorkingDir,
					}
					if !interrupted {
						metadata.ExitCode = &exitCode
					}
					if stdout == "" {
						return fantasy.WithResponseMetadata(fantasy.NewTextResponse(BashNoOutput), metadata), nil
					}
					stdout += fmt.Sprintf("\n\n<cwd>%s</cwd>", normalizeWorkingDir(bgShell.WorkingDir))
					return fantasy.WithResponseMetadata(fantasy.NewTextResponse(stdout), metadata), nil
				}

				// Still running after fast-failure check - return as background job
				metadata := BashResponseMetadata{
					StartTime:        startTime.UnixMilli(),
					EndTime:          time.Now().UnixMilli(),
					Description:      params.Description,
					WorkingDirectory: bgShell.WorkingDir,
					Background:       true,
					ShellID:          bgShell.ID,
				}
				response := fmt.Sprintf("Background shell started with ID: %s\n\nUse job_output tool to view output or job_kill to terminate.", bgShell.ID)
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
			}

			// Record the files a foreground command changes so rewind can
			// undo them. The snapshot is fresh for each command, so changes
			// made between commands aren't pinned on this one. Explicit
			// background jobs above aren't covered: they can run for hours
			// alongside the user's own edits. A command moved to the
			// background below is covered up to when the tool returns.
			if !isSafeReadOnly {
				if run := snapshot.Begin(ctx, files, dataDir, workingDir, sessionID); run != nil {
					// Only the project folder is snapshotted, so say when a
					// command runs elsewhere. Read-only ones change nothing.
					outside := !insideDir(execWorkingDir, workingDir) && !isSafeReadOnlyCommand(params.Command)
					defer func() {
						note := run.Finish(ctx)
						if outside {
							note = strings.TrimSpace(outsideNote + "\n" + note)
						}
						if note != "" && err == nil {
							resp.Content += "\n\n" + note
						}
					}()
				}
			}

			// Start synchronous execution with auto-background support
			startTime := time.Now()

			// Start with detached context so it can survive if moved to background
			bgManager := shell.GetBackgroundShellManager()
			bgManager.Cleanup()
			bgShell, err := bgManager.StartSandboxed(context.Background(), execWorkingDir, blockFuncs(), policy, params.Command, params.Description)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error starting shell: %w", err)
			}

			// Wait for either completion, auto-background threshold, or context cancellation
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()

			autoBackgroundAfter := cmp.Or(params.AutoBackgroundAfter, DefaultAutoBackgroundAfter)
			autoBackgroundThreshold := time.Duration(autoBackgroundAfter) * time.Second
			timeout := time.After(autoBackgroundThreshold)

			var stdout, stderr string
			var done bool
			var execErr error

		waitLoop:
			for {
				select {
				case <-ticker.C:
					stdout, stderr, done, execErr = bgShell.GetOutput()
					if done {
						break waitLoop
					}
				case <-timeout:
					stdout, stderr, done, execErr = bgShell.GetOutput()
					break waitLoop
				case <-ctx.Done():
					// Incoming context was cancelled before we moved to background
					// Kill the shell and return error
					bgManager.Kill(bgShell.ID)
					return fantasy.ToolResponse{}, ctx.Err()
				}
			}

			if done {
				// Command completed within threshold - return synchronously
				// Remove from background manager since we're returning directly
				// Don't call Kill() as it cancels the context and corrupts the exit code
				bgManager.Remove(bgShell.ID)

				interrupted := shell.IsInterrupt(execErr)
				exitCode := shell.ExitCode(execErr)
				if exitCode == 0 && !interrupted && execErr != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("[Job %s] error executing command: %w", bgShell.ID, execErr)
				}

				stdout = formatOutput(stdout, stderr, execErr, dataDir)
				stdout = addSandboxNote(policy, stdout, execErr)

				metadata := BashResponseMetadata{
					StartTime:        startTime.UnixMilli(),
					EndTime:          time.Now().UnixMilli(),
					Output:           stdout,
					Description:      params.Description,
					Background:       params.RunInBackground,
					WorkingDirectory: bgShell.WorkingDir,
				}
				if !interrupted {
					metadata.ExitCode = &exitCode
				}
				if stdout == "" {
					return fantasy.WithResponseMetadata(fantasy.NewTextResponse(BashNoOutput), metadata), nil
				}
				stdout += fmt.Sprintf("\n\n<cwd>%s</cwd>", normalizeWorkingDir(bgShell.WorkingDir))
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(stdout), metadata), nil
			}

			// Still running - keep as background job
			metadata := BashResponseMetadata{
				StartTime:        startTime.UnixMilli(),
				EndTime:          time.Now().UnixMilli(),
				Description:      params.Description,
				WorkingDirectory: bgShell.WorkingDir,
				Background:       true,
				ShellID:          bgShell.ID,
			}
			response := fmt.Sprintf("Command is taking longer than expected and has been moved to background.\n\nBackground shell ID: %s\n\nUse job_output tool to view output or job_kill to terminate.", bgShell.ID)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
		},
	)
}

// formatOutput formats the output of a completed command with error handling
func formatOutput(stdout, stderr string, execErr error, spillDir string) string {
	interrupted := shell.IsInterrupt(execErr)
	exitCode := shell.ExitCode(execErr)

	stdout = truncateOutput(stdout, spillDir)
	stderr = truncateOutput(stderr, spillDir)

	errorMessage := stderr
	if errorMessage == "" && execErr != nil {
		errorMessage = execErr.Error()
	}

	if interrupted {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += "Command was aborted before completion"
	} else if exitCode != 0 {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += fmt.Sprintf("Exit code %d", exitCode)
	}

	hasBothOutputs := stdout != "" && stderr != ""

	if hasBothOutputs {
		stdout += "\n"
	}

	if errorMessage != "" {
		stdout += "\n" + errorMessage
	}

	return stdout
}

// TruncateOutput keeps the head and tail of oversized tool output. The
// dropped middle is written in full to a file under spillDir so the agent
// can read it back rather than losing it for good.
func TruncateOutput(content, spillDir string) string {
	if ansi.StringWidth(content) <= MaxOutputLength {
		return content
	}

	halfLength := MaxOutputLength / 2
	start := ansi.Truncate(content, halfLength, "")
	end := ansi.TruncateLeft(content, ansi.StringWidth(content)-halfLength, "")

	totalLines := strings.Count(content, "\n") + 1
	path, err := shell.SpillOutput(ansi.Strip(content), spillDir)
	if err != nil {
		slog.Debug("Could not spill tool output", "dir", spillDir, "error", err)
		path = ""
	}
	return fmt.Sprintf("%s\n\n%s\n\n%s", start, bashTruncationNotice(halfLength, totalLines, path), end)
}

// bashTruncationNotice states what TruncateOutput kept and how to get the
// rest. An empty path means the full output could not be saved.
func bashTruncationNotice(half, totalLines int, path string) string {
	if path == "" {
		return fmt.Sprintf("[Showing first and last %d chars of %d lines. The middle could not be saved; narrow the command's output with grep or a line range if you need it.]", half, totalLines)
	}
	return fmt.Sprintf("[Showing first and last %d chars of %d lines. Full output: %s. Use the grep or view tool on it; don't rerun the command.]", half, totalLines, path)
}

func truncateOutput(content, spillDir string) string {
	return TruncateOutput(content, spillDir)
}

func normalizeWorkingDir(path string) string {
	if runtime.GOOS == "windows" {
		path = strings.ReplaceAll(path, fsext.WindowsWorkingDirDrive(), "")
	}
	return filepath.ToSlash(path)
}

// outsideNote tells the agent rewind can't undo what a command did outside
// the project folder.
const outsideNote = "Note: this command ran outside the project folder; rewind only records file changes inside it."

// insideDir reports whether dir is root or inside it. A relative dir is
// taken relative to root.
func insideDir(dir, root string) bool {
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	if fsext.HasPrefix(dir, root) {
		return true
	}
	realDir, errDir := filepath.EvalSymlinks(dir)
	realRoot, errRoot := filepath.EvalSymlinks(root)
	return errDir == nil && errRoot == nil && fsext.HasPrefix(realDir, realRoot)
}

// sandboxNote tells the agent why a sandboxed command may have failed.
const sandboxNote = "Note: the sandbox may have blocked this command. Shell commands can only write to the project and temp folders"

// addSandboxNote appends sandboxNote to the output of a failed sandboxed
// command that shows signs of hitting the sandbox.
func addSandboxNote(policy *sandbox.Policy, output string, execErr error) string {
	if !policy.Active() || shell.ExitCode(execErr) == 0 {
		return output
	}
	if !strings.Contains(output, "Operation not permitted") && !strings.Contains(output, "sandbox: writing to") {
		return output
	}
	if policy.Network {
		return output + "\n\n" + sandboxNote + "."
	}
	return output + "\n\n" + sandboxNote + ", and the network is off."
}
