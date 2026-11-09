package dialog

import (
	"os"
	"slices"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/commands"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/magiccontext"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
)

// CommandsID is the identifier for the commands dialog.
const CommandsID = "commands"

// CommandType represents the type of commands being displayed.
type CommandType uint

// String returns the string representation of the CommandType.
func (c CommandType) String() string { return []string{"System", "User", "MCP"}[c] }

const (
	sidebarCompactModeBreakpoint = 120
)

const (
	SystemCommands CommandType = iota
	UserCommands
	MCPPrompts
)

// Commands represents a dialog that shows available commands.
type dockerMCPAvailabilityCheckedMsg struct {
	available bool
}

type Commands struct {
	com    *common.Common
	keyMap struct {
		Select,
		UpDown,
		Next,
		Previous,
		Tab,
		ShiftTab,
		Close key.Binding
	}

	sessionID  string
	hasSession bool
	hasTodos   bool
	hasQueue   bool
	selected   CommandType

	spinner spinner.Model
	loading bool

	help  help.Model
	input textinput.Model
	list  *list.FilterableList

	windowWidth int

	customCommands []commands.CustomCommand
	mcpPrompts     []commands.MCPPrompt

	dockerMCPAvailable     *bool
	dockerMCPCheckInFlight bool

	// recent lists recently used system command IDs, most recent first.
	recent []string
	// flatItems is the ungrouped command order used while filtering, so
	// ranking is the same as without sections; groupedItems has section
	// headers and the Recent section, shown when the filter is empty.
	flatItems    []list.FilterableItem
	groupedItems []list.FilterableItem
	// chosen is the ID of the system command picked last, for the caller
	// to record as recently used (see TakeChosen).
	chosen string
}

var _ Dialog = (*Commands)(nil)

// NewCommands creates a new commands dialog.
func NewCommands(com *common.Common, sessionID string, hasSession, hasTodos, hasQueue bool, customCommands []commands.CustomCommand, mcpPrompts []commands.MCPPrompt) (*Commands, error) {
	c := &Commands{
		com:            com,
		selected:       SystemCommands,
		sessionID:      sessionID,
		hasSession:     hasSession,
		hasTodos:       hasTodos,
		hasQueue:       hasQueue,
		customCommands: customCommands,
		mcpPrompts:     mcpPrompts,
	}

	help := help.New()
	help.Styles = com.Styles.DialogHelpStyles()

	c.help = help

	c.list = list.NewFilterableList()
	c.list.Focus()
	c.list.SetSelected(0)

	c.input = textinput.New()
	c.input.SetVirtualCursor(false)
	c.input.Placeholder = "Type to filter"
	c.input.SetStyles(com.Styles.TextInput)
	c.input.Focus()

	c.keyMap.Select = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "confirm"),
	)
	c.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑/↓", "choose"),
	)
	c.keyMap.Next = key.NewBinding(
		key.WithKeys("down"),
		key.WithHelp("↓", "next item"),
	)
	c.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	c.keyMap.Tab = key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "switch selection"),
	)
	c.keyMap.ShiftTab = key.NewBinding(
		key.WithKeys("shift+tab"),
		key.WithHelp("shift+tab", "switch selection prev"),
	)
	closeKey := CloseKey
	closeKey.SetHelp("esc", "cancel")
	c.keyMap.Close = closeKey

	if available, known := config.DockerMCPAvailabilityCached(); known {
		c.dockerMCPAvailable = &available
	}

	// Set initial commands
	c.setCommandItems(c.selected)

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = com.Styles.Dialog.Spinner
	c.spinner = s

	return c, nil
}

// ID implements Dialog.
func (c *Commands) ID() string {
	return CommandsID
}

// HandleMsg implements [Dialog].
func (c *Commands) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case dockerMCPAvailabilityCheckedMsg:
		c.dockerMCPAvailable = &msg.available
		c.dockerMCPCheckInFlight = false
		if c.selected == SystemCommands {
			// Preserve the current selection across the rebuild to avoid reset
			var prevID string
			if item, ok := c.list.SelectedItem().(*CommandItem); ok && item != nil {
				prevID = item.id
			}
			c.setCommandItems(c.selected)
			if prevID != "" {
				for i, it := range c.list.FilteredItems() {
					if ci, ok := it.(*CommandItem); ok && ci != nil && ci.id == prevID {
						c.list.SetSelected(i)
						c.list.ScrollToSelected()
						break
					}
				}
			}
		}
		return nil
	case spinner.TickMsg:
		if c.loading {
			var cmd tea.Cmd
			c.spinner, cmd = c.spinner.Update(msg)
			return ActionCmd{Cmd: cmd}
		}
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, c.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, c.keyMap.Previous):
			c.list.Focus()
			c.moveSelection(-1)
		case key.Matches(msg, c.keyMap.Next):
			c.list.Focus()
			c.moveSelection(1)
		case key.Matches(msg, c.keyMap.Select):
			// The palette opens on "/", so "/resume auth bug" arrives here
			// as "resume auth bug".
			typed := "/" + strings.TrimPrefix(strings.TrimSpace(c.input.Value()), "/")
			if query, ok := ResumeQuery(typed); ok && query != "" {
				return ActionOpenResume{Query: query}
			}
			if arg, ok := CDQuery(typed); ok && arg != "" {
				return ActionChangeDir{Arg: arg}
			}
			if wt, ok := WorktreeQuery(typed); ok && (wt.Name != "" || wt.Remove || wt.Err != "") {
				return wt.Action()
			}
			if selectedItem := c.list.SelectedItem(); selectedItem != nil {
				if item, ok := selectedItem.(*CommandItem); ok && item != nil {
					return c.choose(item)
				}
			}
		case key.Matches(msg, c.keyMap.Tab):
			if len(c.customCommands) > 0 || len(c.mcpPrompts) > 0 {
				c.selected = c.nextCommandType()
				c.setCommandItems(c.selected)
			}
		case key.Matches(msg, c.keyMap.ShiftTab):
			if len(c.customCommands) > 0 || len(c.mcpPrompts) > 0 {
				c.selected = c.previousCommandType()
				c.setCommandItems(c.selected)
			}
		default:
			var cmd tea.Cmd
			for _, item := range c.list.FilteredItems() {
				if item, ok := item.(*CommandItem); ok && item != nil {
					if msg.String() == item.Shortcut() {
						return c.choose(item)
					}
				}
			}
			prevValue := c.input.Value()
			c.input, cmd = c.input.Update(msg)
			value := c.input.Value()
			if value != prevValue {
				c.applyFilter(value)
			}
			return ActionCmd{cmd}
		}
	}
	return nil
}

func checkDockerMCPAvailabilityCmd() tea.Cmd {
	return func() tea.Msg {
		return dockerMCPAvailabilityCheckedMsg{available: config.RefreshDockerMCPAvailability()}
	}
}

func (c *Commands) InitialCmd() tea.Cmd {
	if c.dockerMCPAvailable != nil || c.dockerMCPCheckInFlight {
		return nil
	}
	c.dockerMCPCheckInFlight = true
	return checkDockerMCPAvailabilityCmd()
}

// Cursor returns the cursor position relative to the dialog.
func (c *Commands) Cursor() *tea.Cursor {
	return InputCursor(c.com.Styles, c.input.Cursor())
}

// commandsRadioView generates the command type selector radio buttons.
func commandsRadioView(sty *styles.Styles, selected CommandType, hasUserCmds bool, hasMCPPrompts bool) string {
	if !hasUserCmds && !hasMCPPrompts {
		return ""
	}

	selectedFn := func(t CommandType) string {
		if t == selected {
			return sty.Radio.On.Padding(0, 1).Render() + sty.Radio.Label.Render(t.String())
		}
		return sty.Radio.Off.Padding(0, 1).Render() + sty.Radio.Label.Render(t.String())
	}

	parts := []string{
		selectedFn(SystemCommands),
	}

	if hasUserCmds {
		parts = append(parts, selectedFn(UserCommands))
	}
	if hasMCPPrompts {
		parts = append(parts, selectedFn(MCPPrompts))
	}

	return strings.Join(parts, " ")
}

// Draw implements [Dialog].
func (c *Commands) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := c.com.Styles
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(defaultDialogHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	if area.Dx() != c.windowWidth && c.selected == SystemCommands {
		c.windowWidth = area.Dx()
		// since some items in the list depend on width (e.g. toggle sidebar command),
		// we need to reset the command items when width changes
		c.setCommandItems(c.selected)
	}

	innerWidth := width - c.com.Styles.Dialog.View.GetHorizontalFrameSize()
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.InputPrompt.GetVerticalFrameSize() + inputContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() +
		t.Dialog.View.GetVerticalFrameSize()

	c.input.SetWidth(dialogInputTextWidth(t, c.input, innerWidth))

	c.list.SetSize(innerWidth, max(0, height-heightOffset))

	// Hide the shortcut hints uniformly when the widest would crowd names.
	applyInfoColumnVisibility(c.list.FilteredItems(), innerWidth, commandInfoMaxPercent)

	rc := NewRenderContext(t, width)
	rc.Title = "Commands"
	rc.TitleInfo = commandsRadioView(t, c.selected, len(c.customCommands) > 0, len(c.mcpPrompts) > 0)
	inputView := t.Dialog.InputPrompt.Render(c.input.View())
	rc.AddPart(inputView)
	listView := t.Dialog.List.Height(c.list.Height()).Render(c.list.Render())
	rc.AddPart(listView)
	rc.Help = renderDialogHelp(t, &c.help, c, innerWidth)

	if c.loading {
		rc.Help = t.Dialog.HelpView.Width(innerWidth).Render(c.spinner.View() + " Generating Prompt...")
	}

	view := rc.Render()

	cur := c.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// ShortHelp implements [help.KeyMap].
func (c *Commands) ShortHelp() []key.Binding {
	return []key.Binding{
		c.keyMap.Tab,
		c.keyMap.UpDown,
		c.keyMap.Select,
		c.keyMap.Close,
	}
}

// FullHelp implements [help.KeyMap].
func (c *Commands) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{c.keyMap.Select, c.keyMap.Next, c.keyMap.Previous, c.keyMap.Tab},
		{c.keyMap.Close},
	}
}

// nextCommandType returns the next command type in the cycle.
func (c *Commands) nextCommandType() CommandType {
	switch c.selected {
	case SystemCommands:
		if len(c.customCommands) > 0 {
			return UserCommands
		}
		if len(c.mcpPrompts) > 0 {
			return MCPPrompts
		}
		fallthrough
	case UserCommands:
		if len(c.mcpPrompts) > 0 {
			return MCPPrompts
		}
		fallthrough
	case MCPPrompts:
		return SystemCommands
	default:
		return SystemCommands
	}
}

// previousCommandType returns the previous command type in the cycle.
func (c *Commands) previousCommandType() CommandType {
	switch c.selected {
	case SystemCommands:
		if len(c.mcpPrompts) > 0 {
			return MCPPrompts
		}
		if len(c.customCommands) > 0 {
			return UserCommands
		}
		return SystemCommands
	case UserCommands:
		return SystemCommands
	case MCPPrompts:
		if len(c.customCommands) > 0 {
			return UserCommands
		}
		return SystemCommands
	default:
		return SystemCommands
	}
}

// setCommandItems sets the command items based on the specified command type.
func (c *Commands) setCommandItems(commandType CommandType) {
	c.selected = commandType

	commandItems := []list.FilterableItem{}
	c.groupedItems = nil
	switch c.selected {
	case SystemCommands:
		defaults := c.defaultCommands()
		for _, cmd := range defaults {
			commandItems = append(commandItems, cmd)
		}
		c.groupedItems = groupCommandItems(c.com.Styles, defaults, c.recent)
	case UserCommands:
		for _, cmd := range c.customCommands {
			var action Action
			if cmd.Skill != nil {
				action = ActionAttachSkill{ID: cmd.Skill.SkillFilePath, Name: cmd.Skill.Name}
			} else {
				action = ActionRunCustomCommand{
					Content:   cmd.Content,
					Arguments: cmd.Arguments,
					Skill:     cmd.Skill,
				}
			}
			item := NewCommandItem(c.com.Styles, "custom_"+cmd.ID, cmd.Name, "", action)
			if cmd.Skill != nil {
				item = item.WithDescription(cmd.Skill.Description)
			} else if cmd.Description != "" {
				item = item.WithDescription(cmd.Description)
			}
			commandItems = append(commandItems, item)
		}
	case MCPPrompts:
		for _, cmd := range c.mcpPrompts {
			action := ActionRunMCPPrompt{
				Title:       cmd.Title,
				Description: cmd.Description,
				PromptID:    cmd.PromptID,
				ClientID:    cmd.ClientID,
				Arguments:   cmd.Arguments,
			}
			commandItems = append(commandItems, NewCommandItem(c.com.Styles, "mcp_"+cmd.ID, cmd.PromptID, "", action))
		}
	}

	c.flatItems = commandItems
	c.input.SetValue("")
	c.applyFilter("")
}

// applyFilter shows the items matching query. An empty query shows the
// sectioned list (when there is one); a non-empty query ranks the flat
// list, so section headers drop out and the order matches an ungrouped
// palette. The first command is selected.
func (c *Commands) applyFilter(query string) {
	items := c.flatItems
	if query == "" && c.groupedItems != nil {
		items = c.groupedItems
	}
	c.list.SetItems(items...)
	c.list.SetFilter(query)
	c.list.ScrollToTop()
	c.list.SetSelected(-1)
	if idx := c.nextCommandIndex(-1, 1); idx >= 0 {
		c.list.SetSelected(idx)
	}
}

// nextCommandIndex returns the index of the first command after from in
// direction step (1 or -1), wrapping around the ends and skipping section
// headers, or -1 when there is none.
func (c *Commands) nextCommandIndex(from, step int) int {
	n := c.list.Len()
	if n == 0 {
		return -1
	}
	idx := from
	if idx < 0 && step < 0 {
		idx = n
	}
	for range n {
		idx = ((idx+step)%n + n) % n
		if _, ok := c.list.ItemAt(idx).(*CommandItem); ok {
			return idx
		}
	}
	return -1
}

// moveSelection moves the selection by step (1 or -1) to the next
// command, wrapping around and skipping section headers. A command right
// under a header scrolls the header into view with it.
func (c *Commands) moveSelection(step int) {
	idx := c.nextCommandIndex(c.list.Selected(), step)
	if idx < 0 {
		return
	}
	c.list.SetSelected(idx)
	c.list.ScrollToSelected()
	if idx > 0 {
		if _, ok := c.list.ItemAt(idx - 1).(*commandSectionHeader); ok {
			if offsetIdx, _ := c.list.ScrollPosition(); offsetIdx >= idx {
				c.list.ScrollToIndex(idx - 1)
			}
		}
	}
}

// choose returns item's action and remembers a system command as the one
// picked, so the caller can record it as recently used.
func (c *Commands) choose(item *CommandItem) Action {
	if c.selected == SystemCommands {
		c.chosen = item.id
	}
	return item.Action()
}

// TakeChosen returns the ID of the system command picked since the last
// call, or "" when none was, and clears it.
func (c *Commands) TakeChosen() string {
	id := c.chosen
	c.chosen = ""
	return id
}

// SetRecent sets the recently used command IDs (most recent first) shown
// in the Recent section, and rebuilds the system command list.
func (c *Commands) SetRecent(ids []string) {
	c.recent = slices.Clone(ids)
	if c.selected == SystemCommands {
		c.setCommandItems(c.selected)
	}
}

// defaultCommands returns the list of default system commands.
func (c *Commands) defaultCommands() []*CommandItem {
	commands := []*CommandItem{
		NewCommandItem(c.com.Styles, "new_session", "New Session", "ctrl+n", ActionNewSession{}).WithAliases("clear").InGroup(CommandGroupSession),
		NewCommandItem(c.com.Styles, "switch_session", "Sessions", "ctrl+s", ActionOpenDialog{SessionsID}).InGroup(CommandGroupSession),
		NewCommandItem(c.com.Styles, "resume", "Resume Conversation", "", ActionOpenResume{}).WithAliases("resume", "continue", "history").InGroup(CommandGroupSession),
		NewCommandItem(c.com.Styles, "tab_folder", "Tab Folder", "", ActionChangeDir{}).WithAliases("cd", "directory", "dir", "folder").InGroup(CommandGroupTabs),
		NewCommandItem(c.com.Styles, "review_changes", "Review changes…", "alt+v", ActionOpenDialog{ReviewID}).WithAliases("commit", "diff", "review", "git").InGroup(CommandGroupSession),
		NewCommandItem(c.com.Styles, "switch_worktree", "Switch Worktree…", "", ActionOpenWorktrees{}).WithAliases("worktree", "worktrees", "wt").InGroup(CommandGroupTabs),
		NewCommandItem(c.com.Styles, "new_worktree_tab", "New Worktree Tab…", "", ActionNewWorktreeTab{}).WithAliases("branch").InGroup(CommandGroupTabs),
		NewCommandItem(c.com.Styles, "switch_model", "Switch Model", "ctrl+l", ActionOpenDialog{ModelsID}).InGroup(CommandGroupModel),
	}

	// Only show compact command if there's an active session
	if c.hasSession {
		commands = append(commands,
			NewCommandItem(c.com.Styles, "next_tab", "Next Tab", "ctrl+pgdn", ActionNextTab{}).InGroup(CommandGroupTabs),
			NewCommandItem(c.com.Styles, "previous_tab", "Previous Tab", "ctrl+pgup", ActionPrevTab{}).InGroup(CommandGroupTabs),
			NewCommandItem(c.com.Styles, "close_tab", "Close Tab", "alt+w", ActionCloseTab{}).InGroup(CommandGroupTabs),
			NewCommandItem(c.com.Styles, "rename_tab", "Rename Tab", "alt+r", ActionRenameTab{}).WithAliases("rename", "title").InGroup(CommandGroupTabs),
			NewCommandItem(c.com.Styles, "group_tab", "Group Tab", "alt+g", ActionGroupTab{}).WithAliases("group", "ungroup").InGroup(CommandGroupTabs),
			NewCommandItem(c.com.Styles, "aside_tab", "Set Tab Aside", "alt+a", ActionAsideTab{}).WithAliases("aside", "mute").InGroup(CommandGroupTabs),
			NewCommandItem(c.com.Styles, "move_tab_left", "Move Tab Left", "ctrl+shift+pgup", ActionMoveTab{Delta: -1}).InGroup(CommandGroupTabs),
			NewCommandItem(c.com.Styles, "move_tab_right", "Move Tab Right", "ctrl+shift+pgdn", ActionMoveTab{Delta: 1}).InGroup(CommandGroupTabs),
		)
		commands = append(commands, NewCommandItem(c.com.Styles, "summarize", "Summarize Session", "", ActionSummarize{SessionID: c.sessionID}).WithAliases("compact", "summarize").InGroup(CommandGroupSession))
		commands = append(commands, NewCommandItem(c.com.Styles, "export_session", "Export Session", "", ActionExportSession{SessionID: c.sessionID}).WithAliases("export", "share", "html").InGroup(CommandGroupSession))
		if opts := c.com.Config().Options; opts != nil && opts.MagicContextEnabled() {
			for _, mc := range magiccontext.Commands {
				commands = append(commands, NewCommandItem(c.com.Styles, "magic_context_"+mc.Name, "Magic Context: "+mc.Description, "",
					ActionMagicContext{SessionID: c.sessionID, Name: mc.Name}).WithAliases(mc.Name).InGroup(CommandGroupSession))
			}
		}
	}

	// Add reasoning toggle for models that support it
	cfg := c.com.Config()
	if agentCfg, ok := cfg.Agents[config.AgentCoder]; ok {
		providerCfg := cfg.GetProviderForModel(agentCfg.Model)
		model := cfg.GetModelByType(agentCfg.Model)
		if providerCfg != nil && model != nil && model.CanReason {
			selectedModel := cfg.Models[agentCfg.Model]

			// Anthropic models: thinking toggle
			if model.CanReason && len(model.ReasoningLevels) == 0 {
				status := "Enable"
				if selectedModel.Think {
					status = "Disable"
				}
				commands = append(commands, NewCommandItem(c.com.Styles, "toggle_thinking", status+" Thinking Mode", "", ActionToggleThinking{}).InGroup(CommandGroupModel))
			}

			// OpenAI models: reasoning effort dialog
			if len(model.ReasoningLevels) > 0 {
				commands = append(commands, NewCommandItem(c.com.Styles, "select_reasoning_effort", "Select Reasoning Effort", "", ActionOpenDialog{
					DialogID: ReasoningID,
				}).InGroup(CommandGroupModel))
			}
		}
	}
	// Only show toggle compact mode command if window width is larger than compact breakpoint (120)
	if c.windowWidth >= sidebarCompactModeBreakpoint && c.hasSession {
		commands = append(commands, NewCommandItem(c.com.Styles, "toggle_sidebar", "Toggle Sidebar", "ctrl+b", ActionToggleCompactMode{}).InGroup(CommandGroupView))
	}
	if c.hasSession {
		cfgPrime := c.com.Config()
		agentCfg := cfgPrime.Agents[config.AgentCoder]
		model := cfgPrime.GetModelByType(agentCfg.Model)
		if model != nil && model.SupportsImages {
			commands = append(commands, NewCommandItem(c.com.Styles, "file_picker", "Open File Picker", "ctrl+f", ActionOpenDialog{
				DialogID: FilePickerID,
			}).InGroup(CommandGroupTools))
		}
	}

	// Add external editor command if $EDITOR is available.
	//
	// TODO: Use [tea.EnvMsg] to get environment variable instead of os.Getenv;
	// because os.Getenv does IO is breaks the TEA paradigm and is generally an
	// antipattern.
	if os.Getenv("EDITOR") != "" {
		commands = append(commands, NewCommandItem(c.com.Styles, "open_external_editor", "Open External Editor", "ctrl+o", ActionExternalEditor{}).InGroup(CommandGroupTools))
	}

	// Add Docker MCP command if available and not already enabled.
	if !cfg.IsDockerMCPEnabled() && c.dockerMCPAvailable != nil && *c.dockerMCPAvailable {
		commands = append(commands, NewCommandItem(c.com.Styles, "enable_docker_mcp", "Enable Docker MCP Catalog", "", ActionEnableDockerMCP{}).InGroup(CommandGroupTools))
	}

	// Add disable Docker MCP command if it's currently enabled
	if cfg.IsDockerMCPEnabled() {
		commands = append(commands, NewCommandItem(c.com.Styles, "disable_docker_mcp", "Disable Docker MCP Catalog", "", ActionDisableDockerMCP{}).InGroup(CommandGroupTools))
	}

	// Toggle MCP servers for this repository when any are configured.
	if len(cfg.MCP) > 0 {
		commands = append(commands, NewCommandItem(c.com.Styles, "toggle_mcps", "Toggle MCPs", "", ActionOpenDialog{
			DialogID: MCPTogglesID,
		}).InGroup(CommandGroupTools))
	}

	commands = append(commands, NewCommandItem(c.com.Styles, "toggle_skills", "Toggle Skills", "", ActionOpenDialog{
		DialogID: SkillTogglesID,
	}).InGroup(CommandGroupTools))

	if c.hasTodos || c.hasQueue {
		var label string
		switch {
		case c.hasTodos && c.hasQueue:
			label = "Toggle To-Dos/Queue"
		case c.hasQueue:
			label = "Toggle Queue"
		default:
			label = "Toggle To-Dos"
		}
		commands = append(commands, NewCommandItem(c.com.Styles, "toggle_pills", label, "ctrl+t", ActionTogglePills{}).InGroup(CommandGroupView))
	}

	// Add a command for selecting notification style via picker dialog.
	notificationLabel := "Notification Style"
	commands = append(commands, NewCommandItem(c.com.Styles, "select_notifications", notificationLabel, "", ActionOpenDialog{DialogID: NotificationsID}).InGroup(CommandGroupView))

	commands = append(
		commands,
		NewCommandItem(c.com.Styles, "toggle_yolo", "Toggle Auto Mode", "ctrl+y", ActionToggleYoloMode{}).InGroup(CommandGroupControl),
		NewCommandItem(c.com.Styles, "toggle_help", "Toggle Help", "ctrl+g", ActionToggleHelp{}).InGroup(CommandGroupApp),
		NewCommandItem(c.com.Styles, "init", "Initialize Project", "", ActionInitializeProject{}).InGroup(CommandGroupSession),
	)

	// Add transparent background toggle.
	transparentLabel := "Disable Background Color"
	if cfg != nil && cfg.Options != nil && cfg.Options.TUI.IsTransparent() {
		transparentLabel = "Enable Background Color"
	}
	commands = append(commands, NewCommandItem(c.com.Styles, "toggle_transparent", transparentLabel, "", ActionToggleTransparentBackground{}).InGroup(CommandGroupView))

	commands = append(commands, NewCommandItem(c.com.Styles, "switch_theme", "Themes", "", ActionOpenDialog{ThemeID}).InGroup(CommandGroupView))

	commands = append(commands,
		NewCommandItem(c.com.Styles, "open_control", "Open Control Cockpit", "ctrl+\\", ActionOpenControl{}).WithAliases("control", "cockpit", "cost", "missions", "actions").InGroup(CommandGroupControl),
		NewCommandItem(c.com.Styles, "take_back", "Take Back Control", "ctrl+q", ActionTakeBack{}).WithAliases("stop", "manual").InGroup(CommandGroupControl),
		NewCommandItem(c.com.Styles, "open_machines", "Machines", "", ActionOpenMachines{}).WithAliases("machines", "machine", "fleet", "ollama", "local models").InGroup(CommandGroupModel),
		NewCommandItem(c.com.Styles, "start_swarm", "Start Swarm…", "", ActionStartSwarm{}).WithAliases("swarm", "fleet", "split mission").WithDescription("Split the attached mission's open criteria across your machines, each on its own branch.").InGroup(CommandGroupControl),
		NewCommandItem(c.com.Styles, "open_merge_board", "Merge Board", "", ActionOpenMergeBoard{}).WithAliases("swarm", "merge", "branches").WithDescription("Review each swarm machine's branch, then merge or discard it.").InGroup(CommandGroupControl),
		NewCommandItem(c.com.Styles, "open_context", "Context Settings", "", ActionOpenContext{}).WithAliases("context", "magic context", "magic-context", "auto-summarize").InGroup(CommandGroupSession),
	)
	if c.hasSession {
		commands = append(commands,
			NewCommandItem(c.com.Styles, "open_rewind", "Rewind…", "", ActionOpenRewind{}).WithAliases("undo", "checkpoint", "restore", "revert").WithDescription("Turn the session back to any earlier turn, restoring files, chat or both.").InGroup(CommandGroupSession),
			NewCommandItem(c.com.Styles, "fork_session", "Fork Session", "", ActionForkSession{}).WithAliases("branch", "copy", "duplicate").WithDescription("Copy this session's chat and file history into a new tab to try another direction.").InGroup(CommandGroupSession),
		)
	}

	if cfg == nil || cfg.Options.ArtifactsEnabled() {
		commands = append(commands, NewCommandItem(c.com.Styles, "open_artifacts", "Open Artifacts Gallery", "", ActionOpenArtifacts{}).WithAliases("artifacts").InGroup(CommandGroupTools))
	}

	// Add mouse support toggle.
	mouseLabel := "Disable Mouse"
	if cfg != nil && cfg.Options != nil && cfg.Options.TUI.Mouse != nil && !*cfg.Options.TUI.Mouse {
		mouseLabel = "Enable Mouse"
	}
	commands = append(commands, NewCommandItem(c.com.Styles, "toggle_mouse", mouseLabel, "", ActionToggleMouseSupport{}).InGroup(CommandGroupView))
	commands = append(commands, NewCommandItem(c.com.Styles, "open_settings", "Settings…", "", ActionOpenSettings{}).WithAliases("settings", "preferences", "options", "config").InGroup(CommandGroupApp))
	commands = append(commands, updateCommandItem(c.com.Styles).InGroup(CommandGroupApp))

	commands = append(
		commands,
		NewCommandItem(c.com.Styles, "quit", "Quit", "ctrl+c", tea.QuitMsg{}).WithAliases("exit").InGroup(CommandGroupApp),
	)

	return commands
}

// SetCustomCommands sets the custom commands and refreshes the view if user commands are currently displayed.
func (c *Commands) SetCustomCommands(customCommands []commands.CustomCommand) {
	c.customCommands = customCommands
	if c.selected == UserCommands {
		c.setCommandItems(c.selected)
	}
}

// SetMCPPrompts sets the MCP prompts and refreshes the view if MCP prompts are currently displayed.
func (c *Commands) SetMCPPrompts(mcpPrompts []commands.MCPPrompt) {
	c.mcpPrompts = mcpPrompts
	if c.selected == MCPPrompts {
		c.setCommandItems(c.selected)
	}
}

// StartLoading implements [LoadingDialog].
func (c *Commands) StartLoading() tea.Cmd {
	if c.loading {
		return nil
	}
	c.loading = true
	return c.spinner.Tick
}

// StopLoading implements [LoadingDialog].
func (c *Commands) StopLoading() {
	c.loading = false
}
