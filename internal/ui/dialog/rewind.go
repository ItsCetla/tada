package dialog

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/control"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/rewind"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// RewindID is the identifier for the rewind timeline dialog.
const RewindID = "rewind"

// ActionOpenRewind opens the rewind timeline. MessageID, when set,
// preselects the turn that message starts.
type ActionOpenRewind struct {
	MessageID string
}

// ActionRewindPlan asks the UI for a dry run of Request and to hand the
// plan back through SetPlan.
type ActionRewindPlan struct {
	SessionID string
	Request   rewind.Request
}

// ActionRewind asks the UI to rewind the session.
type ActionRewind struct {
	SessionID string
	Request   rewind.Request
}

// ActionForkSession asks the UI to copy the session into a new one and
// open it. AtMessageID is where the copy ends, as for rewind.Service.Fork;
// empty copies everything.
type ActionForkSession struct {
	SessionID   string
	AtMessageID string
}

// RewindQuery reports whether value is the rewind command typed into the
// editor ("/rewind", "/undo" or "/checkpoint").
func RewindQuery(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "/rewind", "/undo", "/checkpoint", "/checkpoints":
		return true
	}
	return false
}

var rewindModes = []rewind.Mode{rewind.ModeBoth, rewind.ModeChat, rewind.ModeFiles}

func rewindModeLabel(m rewind.Mode) string {
	switch m {
	case rewind.ModeChat:
		return "Chat only"
	case rewind.ModeFiles:
		return "Files only"
	default:
		return "Files and chat"
	}
}

// Rewind lists a session's turns and turns the session back to just
// before the selected one.
type Rewind struct {
	com       *common.Common
	sessionID string
	workDir   string
	width     int
	height    int

	// want is the message to select once the timeline loads.
	want    string
	loaded  bool
	turns   []rewind.Turn
	cursor  int
	mode    rewind.Mode
	scope   rewind.Scope
	force   bool
	armed   bool
	plan    *rewind.Result
	running bool
	forking bool
	err     string

	help   help.Model
	keyMap struct {
		Up        key.Binding
		Down      key.Binding
		Mode      key.Binding
		Scope     key.Binding
		Force     key.Binding
		Confirm   key.Binding
		Fork      key.Binding
		ForkAfter key.Binding
		Close     key.Binding
	}
}

var _ Dialog = (*Rewind)(nil)

// NewRewind creates the rewind dialog for a session. workDir is used to
// show paths relative to the project; messageID preselects a turn.
func NewRewind(com *common.Common, sessionID, workDir, messageID string) *Rewind {
	d := &Rewind{
		com:       com,
		sessionID: sessionID,
		workDir:   workDir,
		want:      messageID,
		mode:      rewind.ModeBoth,
		scope:     rewind.ScopeAll,
	}
	d.help = help.New()
	d.help.Styles = com.Styles.DialogHelpStyles()
	d.keyMap.Up = key.NewBinding(key.WithKeys("up", "k", "ctrl+p"), key.WithHelp("↑↓", "turn"))
	d.keyMap.Down = key.NewBinding(key.WithKeys("down", "j", "ctrl+n"))
	d.keyMap.Mode = key.NewBinding(key.WithKeys("m", "tab"), key.WithHelp("m", "mode"))
	d.keyMap.Scope = key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "scope"))
	d.keyMap.Force = key.NewBinding(key.WithKeys("!"), key.WithHelp("!", "force"))
	d.keyMap.Confirm = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "rewind"))
	d.keyMap.Fork = key.NewBinding(key.WithKeys("f"), key.WithHelp("f/F", "fork"))
	d.keyMap.ForkAfter = key.NewBinding(key.WithKeys("F"))
	d.keyMap.Close = CloseKey
	return d
}

// ID implements Dialog.
func (d *Rewind) ID() string { return RewindID }

// SessionID is the session the dialog rewinds.
func (d *Rewind) SessionID() string { return d.sessionID }

// SetTimeline shows the session's turns, or why they couldn't load.
func (d *Rewind) SetTimeline(turns []rewind.Turn, err error) {
	d.loaded = true
	if err != nil {
		d.err = err.Error()
		return
	}
	d.err = ""
	d.turns = turns
	d.cursor = max(0, len(turns)-1)
	for i, t := range turns {
		if t.MessageID == d.want {
			d.cursor = i
			break
		}
	}
	d.disarm()
}

// SetPlan shows the dry run for req. A plan for anything other than the
// current selection is dropped.
func (d *Rewind) SetPlan(req rewind.Request, res rewind.Result, err error) {
	req.DryRun = false
	if !d.armed || req != d.request() {
		return
	}
	if err != nil {
		d.armed = false
		d.err = err.Error()
		return
	}
	d.plan = &res
}

// SetResult shows a failed rewind. A successful one closes the dialog.
func (d *Rewind) SetResult(err error) {
	d.running = false
	d.disarm()
	if err != nil {
		d.err = err.Error()
	}
}

// SetForkResult shows a failed fork. A successful one closes the dialog.
func (d *Rewind) SetForkResult(err error) {
	d.forking = false
	if err != nil {
		d.err = err.Error()
	}
}

// fork returns the fork of the selected turn: before it, or with after
// set, through its end.
func (d *Rewind) fork(after bool) Action {
	t := d.selected()
	if t == nil {
		return nil
	}
	at := t.MessageID
	if after {
		// The next turn's prompt is where a copy through this turn ends;
		// the last turn copies everything.
		at = ""
		if d.cursor+1 < len(d.turns) {
			at = d.turns[d.cursor+1].MessageID
		}
	}
	d.err = ""
	d.disarm()
	d.forking = true
	return ActionForkSession{SessionID: d.sessionID, AtMessageID: at}
}

func (d *Rewind) selected() *rewind.Turn {
	if d.cursor < 0 || d.cursor >= len(d.turns) {
		return nil
	}
	return &d.turns[d.cursor]
}

// request is the rewind the current selection and switches describe.
func (d *Rewind) request() rewind.Request {
	t := d.selected()
	if t == nil {
		return rewind.Request{}
	}
	return rewind.Request{
		MessageID: t.MessageID,
		Mode:      d.mode,
		Scope:     d.scope,
		Force:     d.force,
	}
}

func (d *Rewind) disarm() {
	d.armed = false
	d.plan = nil
}

// HandleMsg implements Dialog.
func (d *Rewind) HandleMsg(msg tea.Msg) Action {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	if d.running || d.forking {
		return nil
	}
	switch {
	case key.Matches(k, d.keyMap.Fork):
		return d.fork(false)
	case key.Matches(k, d.keyMap.ForkAfter):
		return d.fork(true)
	case key.Matches(k, d.keyMap.Close):
		if d.armed {
			d.disarm()
			return nil
		}
		return ActionClose{}
	case key.Matches(k, d.keyMap.Up):
		d.err = ""
		d.cursor = max(0, d.cursor-1)
		d.disarm()
	case key.Matches(k, d.keyMap.Down):
		d.err = ""
		d.cursor = max(0, min(d.cursor+1, len(d.turns)-1))
		d.disarm()
	case key.Matches(k, d.keyMap.Mode):
		for i, m := range rewindModes {
			if m == d.mode {
				d.mode = rewindModes[(i+1)%len(rewindModes)]
				break
			}
		}
		d.disarm()
	case key.Matches(k, d.keyMap.Scope):
		if d.scope == rewind.ScopeOutside {
			d.scope = rewind.ScopeAll
		} else {
			d.scope = rewind.ScopeOutside
		}
		d.disarm()
	case key.Matches(k, d.keyMap.Force):
		d.force = !d.force
		d.disarm()
	case key.Matches(k, d.keyMap.Confirm):
		if d.selected() == nil {
			return nil
		}
		d.err = ""
		if !d.armed {
			d.armed = true
			d.plan = nil
			req := d.request()
			req.DryRun = true
			return ActionRewindPlan{SessionID: d.sessionID, Request: req}
		}
		if d.plan == nil {
			// Still planning; the second press needs the plan on screen.
			return nil
		}
		d.running = true
		return ActionRewind{SessionID: d.sessionID, Request: d.request()}
	}
	return nil
}

// Draw implements Dialog.
func (d *Rewind) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	d.width = max(0, min(88, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	// Title, switches, the two-line footer, help and the frame take about
	// ten rows.
	d.height = max(8, min(24, area.Dy()-10))
	DrawCenter(scr, area, d.content(time.Now()))
	return nil
}

func (d *Rewind) content(now time.Time) string {
	t := d.com.Styles
	inner := max(0, d.width-t.Dialog.View.GetHorizontalFrameSize())
	width := max(0, inner-2)
	rc := NewRenderContext(t, d.width)
	rc.Title = "Rewind"
	if len(d.turns) > 0 {
		rc.TitleInfo = t.Control.Muted.Render(fmt.Sprintf("%d turns", len(d.turns)))
	}
	pad := lipgloss.NewStyle().Padding(0, 1)
	rc.AddPart(pad.Render(ansi.Truncate(d.switches(t), width, "…")) + "\n")

	listH := min(len(d.turns), max(3, d.height/3))
	var body []string
	switch {
	case !d.loaded:
		body = []string{t.Control.Muted.Render("Loading…")}
	case len(d.turns) == 0 && d.err == "":
		body = []string{t.Control.Muted.Render("Nothing to rewind yet: this session has no turns.")}
	default:
		body = append(body, d.rows(t, now, listH, width)...)
		body = append(body, t.Control.Muted.Render(strings.Repeat("─", width)))
		body = append(body, d.detail(t, width)...)
	}
	// A fixed height per timeline keeps the dialog from jumping as the
	// detail changes, without leaving a short timeline mostly empty.
	height := min(d.height, max(8, listH+11))
	if len(body) > height {
		body = body[:height]
	}
	for len(body) < height {
		body = append(body, "")
	}
	for i, l := range body {
		body[i] = pad.Render(ansi.Truncate(l, width, "…"))
	}
	rc.AddPart(strings.Join(body, "\n"))

	var foot string
	switch {
	case d.err != "":
		foot = t.Control.Alert.Render("! " + d.err)
	case d.running:
		foot = t.Control.Muted.Render("Rewinding…")
	case d.forking:
		foot = t.Control.Muted.Render("Forking…")
	case d.armed && d.plan == nil:
		foot = t.Control.Muted.Render("Working out what changes…")
	case d.armed:
		foot = t.Control.Alert.Render("Press enter again to rewind. This can't be undone; esc to back out.")
	default:
		foot = t.Control.Muted.Render("Not undone: background jobs; binary, large, gitignored or out-of-project files.") + "\n" +
			t.Control.Muted.Render("Edits you make while an agent shell command runs may be undone with it.")
	}
	lines := strings.Split(foot, "\n")
	for i, l := range lines {
		lines[i] = pad.Render(ansi.Truncate(l, width, "…"))
	}
	rc.AddPart(strings.Join(lines, "\n"))
	rc.Help = renderDialogHelp(t, &d.help, d, inner)
	return rc.Render()
}

// switches renders the mode, scope and force settings.
func (d *Rewind) switches(t *styles.Styles) string {
	var parts []string
	for _, m := range rewindModes {
		if m == d.mode {
			parts = append(parts, t.Control.TabOn.Render(rewindModeLabel(m)))
		} else {
			parts = append(parts, t.Control.Tab.Render(rewindModeLabel(m)))
		}
	}
	scope := "all files"
	if d.scope == rewind.ScopeOutside {
		scope = "outside project only"
	}
	force := t.Control.Off.Render("off")
	if d.force {
		force = t.Control.Warn.Render("on")
	}
	return strings.Join(parts, " ") + "  " +
		t.Control.Label.Render("scope ") + t.Control.Value.Render(scope) + "  " +
		t.Control.Label.Render("force ") + force
}

// rows renders up to n turns, keeping the cursor in view.
func (d *Rewind) rows(t *styles.Styles, now time.Time, n, width int) []string {
	first := max(0, min(d.cursor-n/2, len(d.turns)-n))
	var out []string
	for i := first; i < len(d.turns) && len(out) < n; i++ {
		row := d.row(t, d.turns[i], now, width)
		if i == d.cursor {
			plain := ansi.Truncate(ansi.Strip(row), width, "…")
			row = t.Dialog.SelectedItem.UnsetPadding().Render(plain + strings.Repeat(" ", max(0, width-lipgloss.Width(plain))))
		}
		out = append(out, row)
	}
	return out
}

func (d *Rewind) row(t *styles.Styles, turn rewind.Turn, now time.Time, width int) string {
	num := t.Control.Label.Render(fmt.Sprintf("#%-3d", turn.Index))
	var meta []string
	meta = append(meta, control.Ago(time.Unix(turn.CreatedAt, 0), now))
	if n := len(turn.Files); n == 1 {
		meta = append(meta, "1 file")
	} else if n > 1 {
		meta = append(meta, fmt.Sprintf("%d files", n))
	}
	if a := actionCounts(turn.Actions, true); a != "" {
		meta = append(meta, a)
	}
	tail := t.Control.Muted.Render(strings.Join(meta, " · "))
	if turn.Bash {
		tail += " " + t.Control.Warn.Render("$ shell")
	}
	room := max(8, width-lipgloss.Width(num)-lipgloss.Width(tail)-3)
	prompt := ansi.Truncate(oneLine(turn.Prompt), room, "…")
	gap := max(1, width-lipgloss.Width(num)-1-lipgloss.Width(prompt)-lipgloss.Width(tail))
	return num + " " + t.Control.Value.Render(prompt) + strings.Repeat(" ", gap) + tail
}

// detail renders the selected turn, or the plan once armed.
func (d *Rewind) detail(t *styles.Styles, width int) []string {
	turn := d.selected()
	if turn == nil {
		return nil
	}
	m := t.Control.Muted
	if d.armed {
		return d.planLines(t, *turn)
	}
	var out []string
	for i, l := range wrapLines(oneLine(turn.Prompt), width) {
		if i == 3 {
			out[len(out)-1] += "…"
			break
		}
		out = append(out, t.Control.Value.Render(l))
	}
	out = append(out, "")
	if len(turn.Files) == 0 {
		out = append(out, m.Render("No tracked file changes."))
	} else {
		out = append(out, t.Control.Label.Render("Changed: ")+m.Render(strings.Join(d.rels(turn.Files), ", ")))
	}
	if a := actionCounts(turn.Actions, false); a != "" {
		out = append(out, t.Control.Label.Render("Actions: ")+m.Render(a))
	}
	if turn.Bash {
		out = append(out, t.Control.Warn.Render("Ran shell commands; some of their changes may not be undone."))
	}
	out = append(out, "",
		m.Render(fmt.Sprintf("enter to rewind to just before turn %d.", turn.Index)),
		m.Render(fmt.Sprintf("f forks into a new tab just before turn %d, F just after it.", turn.Index)))
	return out
}

func (d *Rewind) planLines(t *styles.Styles, turn rewind.Turn) []string {
	m := t.Control.Muted
	out := []string{t.Control.Label.Render(fmt.Sprintf("%s: back to just before turn %d", rewindModeLabel(d.mode), turn.Index))}
	if d.plan == nil {
		return append(out, m.Render("Loading…"))
	}
	p := d.plan
	skipped := false
	for _, f := range p.Files {
		path := d.rel(f.Path)
		switch {
		case f.Skipped:
			skipped = true
			out = append(out, t.Control.Warn.Render("skip     ")+path+m.Render(" — "+f.Reason))
		case f.Op == rewind.OpDelete:
			out = append(out, t.Control.Alert.Render("delete   ")+path)
		default:
			out = append(out, t.Control.On.Render("restore  ")+path)
		}
	}
	if d.mode != rewind.ModeChat && len(p.Files) == 0 {
		out = append(out, m.Render("No files to change."))
	}
	if skipped && !d.force {
		out = append(out, m.Render("Skipped files were edited outside Ta-Da. Press ! to overwrite them too."))
	}
	if d.mode != rewind.ModeFiles {
		out = append(out, m.Render(fmt.Sprintf("Removes %d messages from the chat; the prompt goes back in the editor.", p.MessagesDeleted)))
	}
	if d.shellSince() && d.mode != rewind.ModeChat {
		out = append(out, t.Control.Warn.Render("Shell commands ran in these turns; some of their changes may not be undone."))
	}
	return out
}

// shellSince reports whether a shell command ran in the selected turn or
// any later one, all of which the rewind undoes.
func (d *Rewind) shellSince() bool {
	for i := max(0, d.cursor); i < len(d.turns); i++ {
		if d.turns[i].Bash {
			return true
		}
	}
	return false
}

func (d *Rewind) rels(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = d.rel(p)
	}
	return out
}

// rel shows path relative to the project when it's inside it.
func (d *Rewind) rel(path string) string {
	if d.workDir == "" || !filepath.IsAbs(path) {
		return path
	}
	r, err := filepath.Rel(d.workDir, path)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return path
	}
	return r
}

// shortClassLabels are the row labels for action classes.
var shortClassLabels = map[permission.Class]string{
	permission.ClassRead:    "read",
	permission.ClassWrite:   "edit",
	permission.ClassOutside: "outside",
	permission.ClassNetwork: "net",
	permission.ClassDelete:  "del",
	permission.ClassSpend:   "spend",
}

// actionCounts renders a turn's actions by class: "edit 3, network 1", or
// "edit 3 net 1" when short.
func actionCounts(counts map[permission.Class]int, short bool) string {
	var parts []string
	for _, c := range permission.Classes {
		n := counts[c]
		if n == 0 {
			continue
		}
		label := c.Label()
		if short {
			label = shortClassLabels[c]
		}
		parts = append(parts, fmt.Sprintf("%s %d", label, n))
	}
	sep := ", "
	if short {
		sep = " "
	}
	return strings.Join(parts, sep)
}

// wrapLines word-wraps s to width.
func wrapLines(s string, width int) []string {
	if width <= 0 || s == "" {
		return nil
	}
	return strings.Split(ansi.Wordwrap(s, width, ""), "\n")
}

// ShortHelp implements help.KeyMap.
func (d *Rewind) ShortHelp() []key.Binding {
	return []key.Binding{d.keyMap.Up, d.keyMap.Confirm, d.keyMap.Fork, d.keyMap.Mode, d.keyMap.Scope, d.keyMap.Force, d.keyMap.Close}
}

// FullHelp implements help.KeyMap.
func (d *Rewind) FullHelp() [][]key.Binding {
	return [][]key.Binding{d.ShortHelp()}
}
