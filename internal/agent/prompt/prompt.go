package prompt

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/gitutil"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/worktree"
)

// Prompt represents a template-based prompt generator.
type Prompt struct {
	name       string
	template   string
	now        func() time.Time
	platform   string
	workingDir string
}

type PromptDat struct {
	Provider   string
	Model      string
	Config     config.Config
	WorkingDir string
	IsGitRepo  bool
	// Worktree is the branch of the linked git worktree the working
	// directory is in, or "" when it is not in one.
	Worktree           string
	Platform           string
	Date               string
	ContextFiles       []ContextFile
	GlobalContextFiles []ContextFile
	AvailSkillXML      string
}

type ContextFile struct {
	Path    string
	Content string
}

type Option func(*Prompt)

func WithTimeFunc(fn func() time.Time) Option {
	return func(p *Prompt) {
		p.now = fn
	}
}

func WithPlatform(platform string) Option {
	return func(p *Prompt) {
		p.platform = platform
	}
}

func WithWorkingDir(workingDir string) Option {
	return func(p *Prompt) {
		p.workingDir = workingDir
	}
}

// InDir returns a copy of the prompt that describes dir as the working
// folder. A tab moved to another folder with /cd builds its system prompt
// from this copy.
func (p *Prompt) InDir(dir string) *Prompt {
	c := *p
	c.workingDir = dir
	return &c
}

// WorkingDir returns the folder the prompt describes, or "" when it
// uses the folder Ta-Da was launched in.
func (p *Prompt) WorkingDir() string {
	return p.workingDir
}

func NewPrompt(name, promptTemplate string, opts ...Option) (*Prompt, error) {
	p := &Prompt{
		name:     name,
		template: promptTemplate,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *Prompt) Build(ctx context.Context, provider, model string, store *config.ConfigStore) (string, error) {
	t, err := template.New(p.name).Parse(p.template)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var sb strings.Builder
	d, err := p.promptData(ctx, provider, model, store)
	if err != nil {
		return "", err
	}
	if err := t.Execute(&sb, d); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return sb.String(), nil
}

// maxContextFileBytes caps how much of a single context file goes into the
// system prompt, so one huge file cannot crowd out everything else.
const maxContextFileBytes = 64 * 1024

func processFile(filePath string) *ContextFile {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	if len(content) > maxContextFileBytes {
		// Back off to a rune boundary so the cut never splits a character.
		n := maxContextFileBytes
		for n > 0 && !utf8.RuneStart(content[n]) {
			n--
		}
		content = append(content[:n:n], fmt.Sprintf("\n\n[Ta-Da: truncated %s after %d bytes]", filePath, maxContextFileBytes)...)
	}
	return &ContextFile{
		Path:    filePath,
		Content: string(content),
	}
}

func processContextPath(p, workingDir string) []ContextFile {
	var contexts []ContextFile
	fullPath := filepathext.SmartJoin(workingDir, p)
	info, err := os.Stat(fullPath)
	if err != nil {
		return contexts
	}
	if info.IsDir() {
		filepath.WalkDir(fullPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				if result := processFile(path); result != nil {
					contexts = append(contexts, *result)
				}
			}
			return nil
		})
	} else {
		result := processFile(fullPath)
		if result != nil {
			contexts = append(contexts, *result)
		}
	}
	return contexts
}

// expandPath expands ~ and environment variables in file paths
func expandPath(path string, store *config.ConfigStore) string {
	path = home.Long(path)
	// Handle environment variable expansion using the same pattern as config
	if strings.HasPrefix(path, "$") {
		if expanded, err := store.Resolver().ResolveValue(path); err == nil {
			path = expanded
		}
	}

	return path
}

// loadContextFiles loads and deduplicates context files from a list of paths.
// Files come back in the order of paths (the first of any case-insensitive
// duplicates wins), so the system prompt is byte-for-byte stable across
// builds and provider prompt caching keeps working.
func loadContextFiles(paths []string, workingDir string, store *config.ConfigStore) []ContextFile {
	var files []ContextFile
	seen := map[string]bool{}
	for _, pth := range paths {
		expanded := expandPath(pth, store)
		pathKey := strings.ToLower(expanded)
		if seen[pathKey] {
			continue
		}
		seen[pathKey] = true
		files = append(files, processContextPath(expanded, workingDir)...)
	}
	return files
}

// templateUses reports whether the prompt template mentions the named field,
// e.g. ".GitStatus". Fields a template never reads are not worth gathering.
func (p *Prompt) templateUses(field string) bool {
	return strings.Contains(p.template, field)
}

func (p *Prompt) promptData(ctx context.Context, provider, model string, store *config.ConfigStore) (PromptDat, error) {
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	platform := cmp.Or(p.platform, runtime.GOOS)

	cfg := store.Config()

	// Discover and load skills metadata.
	var availSkillXML string

	// Start with builtin skills.
	allSkills := skills.DiscoverBuiltin()
	builtinNames := make(map[string]bool, len(allSkills))
	for _, s := range allSkills {
		builtinNames[s.Name] = true
	}

	// Discover user skills from configured paths.
	if len(cfg.Options.SkillsPaths) > 0 {
		expandedPaths := make([]string, 0, len(cfg.Options.SkillsPaths))
		for _, pth := range cfg.Options.SkillsPaths {
			expandedPaths = append(expandedPaths, expandPath(pth, store))
		}
		for _, userSkill := range skills.Discover(expandedPaths) {
			if builtinNames[userSkill.Name] {
				slog.Warn("User skill overrides builtin skill", "name", userSkill.Name)
			}
			allSkills = append(allSkills, userSkill)
		}
	}

	// Deduplicate: user skills override builtins with the same name.
	allSkills = skills.Deduplicate(allSkills)

	// Filter out disabled skills.
	allSkills = skills.Filter(allSkills, store.EffectiveDisabledSkills())

	if len(allSkills) > 0 {
		availSkillXML = skills.ToPromptXML(allSkills)
	}

	isGit := isGitRepo(workingDir)
	data := PromptDat{
		Provider:      provider,
		Model:         model,
		Config:        *cfg,
		WorkingDir:    filepath.ToSlash(workingDir),
		IsGitRepo:     isGit,
		Platform:      platform,
		Date:          p.now().Format("1/2/2006"),
		AvailSkillXML: availSkillXML,
	}
	if isGit && p.templateUses(".Worktree") {
		if wt, ok := worktree.Lookup(workingDir); ok {
			data.Worktree = wt.Branch
		}
	}
	if p.templateUses(".ContextFiles") {
		data.ContextFiles = loadContextFiles(cfg.Options.ContextPaths, workingDir, store)
	}
	if p.templateUses(".GlobalContextFiles") {
		data.GlobalContextFiles = loadContextFiles(cfg.Options.GlobalContextPaths, workingDir, store)
	}
	return data, nil
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// GitStatusSnapshot returns the git status of dir wrapped for the first
// user turn of a conversation, or "" when dir is not a git repository or
// the status is empty. It lives outside the system prompt so the prompt
// prefix stays byte-identical across sessions and hits provider caches.
func GitStatusSnapshot(ctx context.Context, dir string) string {
	if dir == "" || !isGitRepo(dir) {
		return ""
	}
	status, err := getGitStatus(ctx, dir)
	status = strings.TrimSpace(status)
	if err != nil || status == "" {
		return ""
	}
	return "<git_status snapshot=\"conversation start\">\n" + status + "\n</git_status>"
}

func getGitStatus(ctx context.Context, dir string) (string, error) {
	sh := shell.NewShell(&shell.Options{
		WorkingDir: dir,
	})
	branch := getGitBranch(dir)
	status, err := getGitStatusSummary(ctx, sh)
	if err != nil {
		return "", err
	}
	commits, err := getGitRecentCommits(ctx, sh)
	if err != nil {
		return "", err
	}
	return branch + status + commits, nil
}

// getGitBranch reads the checked-out branch straight from the repository.
// This runs on every prompt build, so it stays off the shell: spawning git
// each turn is wasted work, and a detail of prompt assembly has no business
// passing through command blocking and permission policy.
func getGitBranch(dir string) string {
	branch := gitutil.CurrentBranch(dir)
	if branch == "" {
		return ""
	}
	return fmt.Sprintf("Current branch: %s\n", branch)
}

func getGitStatusSummary(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git status --short 2>/dev/null | head -20")
	if err != nil {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "Status: clean\n", nil
	}
	return fmt.Sprintf("Status:\n%s\n", out), nil
}

func getGitRecentCommits(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git log --oneline -n 3 2>/dev/null")
	if err != nil || out == "" {
		return "", nil
	}
	out = strings.TrimSpace(out)
	return fmt.Sprintf("Recent commits:\n%s\n", out), nil
}

func (p *Prompt) Name() string {
	return p.name
}
