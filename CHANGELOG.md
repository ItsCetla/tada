# Changelog

All notable changes to this fork are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/). Upstream Crush changes are not
listed; see the [upstream releases](https://github.com/charmbracelet/crush/releases).

## [Unreleased]

### Added

- **Sandbox for shell commands on macOS.** `option sandbox
  workspace-write` (or `read-only`) runs the agent's shell commands under
  `sandbox-exec`, so they can only write to the project and temp folders,
  and can't touch git hooks, git config or config files that run code.
  With `option sandbox-network off` (the default) the network is cut off
  too. It's an extra layer on top of permission prompts, which still
  appear as usual. Off by default.
- **Rewind undoes file changes made by bash commands.** Ta-Da takes a
  hidden git snapshot of the working directory before and after each
  foreground bash command and records the text files it changed, so
  rewinding a turn restores or deletes them like edits made with the file
  tools. Binary files, files over 1 MiB and non-regular files are named in
  the command's result instead. Background jobs, gitignored files and
  nested repositories aren't covered, and nothing is recorded without
  `git`, in your home folder or a folder with more than 50,000 files, or
  while another session runs a command in the same folder; the command's
  result says when a change wasn't recorded. Edits you make yourself while
  an agent's shell command runs may be undone along with it.
- **Lean tool profile.** `option tool-profile lean` trims the built-in
  tools to a small core and hides MCP tools; `option enable-tool <name>`
  adds single tools or whole MCP servers back.
- **Smaller, stable prompt.** Tool descriptions and the coder system prompt
  were condensed, git status moves out of the system prompt into a hidden
  first-turn snapshot so the prompt prefix caches across turns, and OpenAI
  requests carry `prompt_cache_key`.
- **Budgeted compaction.** Summaries are back-dated to the cut point and
  requests that overflow the context are retried after compaction.
- **Per-project skill toggles.** Skills can be enabled or disabled per
  project from the command palette.

### Changed

- **Breaking: artifact tools are opt-in.** `artifact`, `artifact_data` and
  `artifact_comments` are no longer sent by default. Add
  `option enable-tool artifact` (and the other two) to `tadarc` to get them
  back. `machines` stays on whenever machines are configured.
- **Breaking: `~/.claude/skills` is no longer loaded by default.** Add
  `option skill-path ~/.claude/skills` to `tadarc` to keep using those
  skills.

## [0.6.2] - 2026-10-23

### Security

- **Artifact viewer sign-in links work once across all viewers.** A
  ticket redeemed on one viewer could be reused on a second viewer for the
  same store (another port, or a viewer restarted within two minutes).
  Used tickets are now recorded in the store, so every viewer refuses them.

## [0.6.1] - 2026-10-23

### Security

- **Review and commit no longer runs the repository's file system
  monitor.** Reading status in the review screen (`alt+v`) could start a
  program named by `core.fsmonitor` in the repository's git config. It is
  now turned off for those reads, as the swarm already did.
- **The review screen strips control characters.** File names, the branch
  name, file contents, git errors and drafted commit messages could carry
  terminal escape sequences that the screen passed straight through. They
  now show as `?`.

## [0.6.0] - 2026-10-23

### Added

- **Choose how each machine is used.** In `/model`, tab over to
  **Machines** to see this computer and every machine, with how each is
  used and whether it answers. Press enter on one to make it the main AI,
  a helper picked automatically, a helper used only when you ask for it
  by name ("have pc review this"), or off. The same choice is `--use auto`
  or `--use named` on `tada machines add` and `machine add` in `tadarc`.

- **Swarm on your own machines, with a merge board.** A mission's open
  criteria can be split across your ready machines, each working in its
  own worktree and branch with its own model. Start it with `w` on the
  cockpit's Missions page, **Start Swarm** in the command palette or
  `tada swarm start <mission-id>`; Ta-Da shows the plan and starts only
  when you confirm. The **Merge Board** shows each machine's card with its
  status, checks, cost and diff, and merges (`m`) or discards (`d`) each
  branch; merging ticks only the criteria whose approved checks passed.
  `tada swarm status`, `merge` and `discard` do the same from the shell.
  Every swarm tool call goes through your permission prompts, a swarm
  refuses to start in auto mode (and its tool calls are refused if auto
  mode is turned on mid-run), and its tools, glob patterns included, are
  confined to its worktree. Checks on a machine's work ask before they
  run, even in auto mode, skip any check whose command changed mid-run,
  and run without secret-looking environment variables.

- **Live privacy receipt.** Ta-Da now counts every host it talks to over
  HTTP, per session: model providers, MCP servers, web tools, fleet
  machines, sign-ins, model lists and update checks, with requests and
  estimated bytes each way. The status bar shows `↑ N hosts` when anything
  left this machine (or `local only`), the cockpit has a fifth page,
  Network, that lists each host with why it was contacted, and
  `tada status --network` prints the receipt of a running Ta-Da server.
  The receipt is kept in memory only and never written to disk.

- **Self-checking missions.** A mission criterion written as
  `text :: command` (for example `Tests pass :: go test ./...`) ticks only
  when its command exits 0 in the project folder. Checking it runs the
  command, a failure keeps it open and shows the output, and open checks
  rerun in the background after each interactive turn. Press `v` on the
  cockpit's Missions page to run the approved ones; the agent can do the
  same with the mission tool's `verify` and `set_check` actions. Setting a
  check asks permission like a shell command, since it later runs without
  asking. A check is approved only when you grant that prompt, type it
  yourself, or press space on it in the cockpit, which shows its full
  command. Only approved checks run unattended, so a mission file that
  came with a repository can't run its own commands, and checks run under
  the bash tool's block list. A check with hidden characters, such as
  escape codes, line breaks or direction marks, is refused and never runs,
  and the cockpit shows those characters spelled out. Each check has a
  5 minute timeout by default.

- **Turn receipts.** The "Worked for …" line after each turn now reads
  like a receipt: `Worked for 2m 14s · 5 files changed (+120 −34) ·
  3 commands · tests passed · $0.42`. It shows only what the turn did,
  reports tests only when the turn's last command was a test run, and
  drops the least important parts first on narrow windows.

- **Cockpit summary in the status bar.** Next to the cost and network
  pills, the status bar now shows how many permission requests and
  questions wait across all tabs (`2 waiting`) and the open mission's
  progress (`◆ 2/5`). On narrow windows the pills drop in order (cost,
  then network, then mission) so what needs you stays in view.

- **Side-by-side diffs on wide windows.** Edit, write and permission
  diffs switch to side by side once they have 160 columns, in the chat as
  well as the permissions dialog. The `diff_mode` option (`option ui diff`
  in `tadarc`) now takes `auto`, the default, besides `split` and
  `unified`, and the chat follows it too, not just the dialog.
- **Local models on first run.** Ta-Da looks for Ollama (`:11434`),
  LM Studio (`:1234`) and llama.cpp or another OpenAI-compatible server
  (`:8080`) on this computer when the model picker opens, including during
  onboarding. Models it finds are listed at the top under "running on this
  computer", and picking one saves the server as a provider in your global
  config with no API key needed. Each probe gives up after 300ms, and
  servers you've already configured aren't shown again.
  `tada models --discover` prints what it finds and the tadarc line for
  each.
- **`tada doctor`.** Checks your setup and says what to fix: config files
  (syntax errors with file and line, untrusted project configs, the
  deprecated `tada.json`), where each provider's key comes from (never the
  key itself), whether the selected models exist, local Ollama and LM
  Studio servers, language servers, MCP and hook commands, permissions,
  git, the terminal, the data directory and the session database. It
  contacts only localhost unless you pass `--online`, which sends one HEAD
  request per provider and MCP host and looks for an update. `-v` shows
  every finding, `--json` prints JSON, and it exits with status 1 when a
  check fails.

- **Export a session.** `tada session export [id]` writes a session as
  Markdown, a self-contained HTML page or JSON (`--format md|html|json`,
  `-o file`), with a header of models, tokens, cost and files changed,
  then the conversation with folded reasoning, tool calls and edits as
  diffs. The HTML page works offline, in light and dark, and on a phone.
  API keys, tokens and secret-looking variables are replaced with
  `[REDACTED]` and the home folder is shown as `~` unless you pass
  `--no-redact`. **Export Session** in the command palette writes the
  current session as HTML, including the hosts it talked to.
- **Rewind.** Press **r** on one of your messages, type `/rewind` (also
  `/undo`, `/checkpoint`) or pick "Rewind…" in the palette to see the
  session's turns with their prompt, time, files touched, graded actions
  and shell use. Rewind restores files, chat or both to before the chosen
  turn, optionally only for files outside the project. Enter first lists
  the exact files to restore or delete and asks again. Files changed by
  hand since Ta-Da wrote them are skipped unless you press `!`. Rewinding
  the chat puts the prompt back in the editor. Changes made by shell
  commands are not rewound. Also available to clients at
  `GET /v1/workspaces/{id}/sessions/{sid}/timeline` and
  `POST …/rewind`.
- **Fork.** Copy a session into a new tab to try another direction
  without losing the original. In the rewind timeline `f` forks just
  before the selected turn and `F` just after it; in the chat **F** on a
  prompt forks before it and on a reply after its turn; "Fork Session" in
  the palette copies everything. The fork carries the chat, the summary,
  the file history and read records up to that point, so it can be
  rewound like the original; files on disk, cost, tokens, todos and
  sub-agent sessions are not copied. Also `tada session fork <id> [--at
  <message-id>] [--json]` and `POST /v1/workspaces/{id}/sessions/{sid}/fork`.
- **Review and commit.** Alt+V, `/review`, `/commit` or **Review
  changes…** in the palette opens a screen listing the tab folder's
  staged, unstaged and untracked files with `+/-` counts and a `•` on the
  files the AI edited this session, next to a scrollable diff (split when
  wide, unified when narrow). Pick files with space (`a` for all), press
  `c` to have the small model draft a semantic commit message from the
  diff and recent history, edit it, and commit with Ctrl+S. Errors stay
  on screen; nothing is ever pushed.
- **A worktree per tab.** `/worktree <branch> [base]` (also `/wt`) moves
  the tab in view into a worktree of its own for that branch, creating
  the branch from `base` (or the current branch) when needed, so several
  tabs can work on one repository without stepping on each other. New
  worktrees go in `../.tada-worktrees/<repo>/` next to the repository, or
  wherever `option worktree-dir` says. The tab bar marks such tabs with
  `⎇ branch` and a `*` while they have uncommitted changes, and the system
  prompt names the branch. `/worktree` alone (or **Switch Worktree…** in
  the command palette) lists the worktrees to switch to, open as a new
  tab with `n`, or remove with `d`; **New Worktree Tab…** opens a new tab
  in one. `/worktree rm <branch>` removes one, asking first when it has
  uncommitted changes and refusing while a tab works in it; the branch
  is always kept. Closing a tab leaves its worktree on disk and says so.
- **Grouped command palette.** `ctrl+p` lists system commands under
  Session, Tabs & Folders, Model, Control, Tools, View and App headers,
  with a Recent section on top holding the last five commands you ran from
  it. Arrow keys skip the headers, and typing still shows one flat list of
  matches. Recent commands are saved to `recent_commands.json` in the
  global data directory.
- **A settings screen.** `/settings` (also `/preferences`, or **Settings…**
  in the command palette) lists the everyday display, behaviour, privacy
  and advanced options with their current values and where each one comes
  from: default, `tadarc`, `tada.json`, a project config or an environment
  variable. Space or enter flips a switch and left and right cycle a
  choice. Changes are saved to `tada.json` in the global data directory
  and most apply at once; the few that need a restart say so. Rows set by
  an environment variable are locked, and a project config still wins over
  a value saved here.

### Changed

- **Chat text fills the window.** Replies and tool output used to stop at
  120 columns, leaving much of a wide terminal empty. They now use the
  whole chat area. To cap the width, pick **Chat width** in `/settings`,
  or set `option ui text-width 120` in `tadarc` or
  `options.tui.text_width` in `tada.json` (0 means full width).

### Fixed

- **Pasting images on Windows.** Windows Terminal keeps `ctrl+v` for its
  own text-only paste, so a screenshot on the clipboard never reached
  Ta-Da. Press `alt+i` to paste an image (it works on every platform),
  and an empty paste now checks the clipboard for an image too. Pasted
  images are also numbered properly instead of always being
  `paste_1.png`.

## [0.5.14] - 2026-10-21

### Added

- **A folder per tab.** `/cd <path>` (also `/directory` and `/dir`) moves
  the tab in view to another project. The AI's tools, permission checks,
  system prompt and the Claude Code engine use that folder from the next
  turn, and file completions, the file picker, the sidebar and the window
  title follow it. `/cd` alone shows the folder and `/cd -` goes back to
  the start folder. The folder is saved with the session. Language
  servers, project config and Magic Context's project memory stay with the
  start folder.
- **Ambient line.** A calm line above the editor breathes and shifts color
  with what the agent is doing, labelled with the current step, then glows
  once and fades when the turn ends. Off with `option ui ambient off`,
  `options.tui.ambient: false` or `TADA_NO_AMBIENT=1`.

### Fixed

- Models behind a LiteLLM proxy now teach Ta-Da their context window from
  the first overflow (`Max Input Tokens=…`), so auto-summarize and Magic
  Context use the real limit instead of guessing 128k.
- A window learned from an overflow is now used by Magic Context, its
  `/ctx` commands and mid-turn summarizing, not only by the overflow retry.
- A request that was summarized and re-run can be summarized again if it
  makes progress and fills the window a second time (up to three times),
  instead of failing with the provider's raw error. When Ta-Da gives up it
  says "Context full" and what to do next.

## [0.5.13] - 2026-10-21

### Added

- **Machines are found and used without asking.** Ta-Da checks your
  machines at startup and before each prompt, and tells the AI how many
  are ready and how many agents they run in total, with a firmer nudge to
  hand side work to them in parallel. llama.cpp servers report their own
  slot count and per-slot context (from `/props`), which now override the
  `--slots` setting; other servers keep using it. The `/model` Machines tab
  shows "· 2 ready, 5 agents", and the machine details show an **Agents**
  row with a hint when the server runs fewer slots than the settings ask
  for.

### Fixed

- A machine that was offline when Ta-Da started, or had no model set, now
  takes work as soon as it comes online instead of after a restart.
- The README privacy table now lists machines, including the new
  `GET /props` check.

## [0.5.12] - 2026-10-21

### Security

- **Built with Go 1.27.3**, which stops trusting compromised intermediate
  certificates in `crypto/x509`
  ([GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809)). 0.5.11 was built
  with Go 1.27.2; update with `tada update`.

## [0.5.11] - 2026-10-21

### Added

- **Machines in `/model`.** Tab past Large and Small to **Machines** to pick
  your main AI and see the machines that help it. Each machine can have a
  role (`--role`, or the Role field in the form), and `s` asks your main AI
  to suggest a role for every machine from its hardware, model, context
  window and slots; `y` uses one suggestion, `Y` uses them all. `m` jumps
  back to the model picker. A machine with a role is the first pick for
  that kind of work.
- **"Allow & Continue in Auto" in the permission prompt** (shortcut `c`).
  It allows the request and switches to Auto mode, so the run carries on
  without more prompts. Hidden when Auto is already on or while planning.
- **Tab status marks.** Each tab shows a spinner while its session works,
  `◎` when it needs you, `✓` when it's done (bold until you look at it) and
  `✗` when it failed, plus how long ago that changed. A finished tab you
  haven't looked at yet shows `✔`, and marks fade into their new color. Ages drop out first
  when the tab bar gets crowded. Sub-agent requests mark their parent tab.
- **"Needs you" count in the tab bar.** Click it, or press **Alt+0**, to jump
  to the next tab waiting on a permission or a question.
- **Rename, group and set aside tabs.** **Alt+R** renames the tab in view
  and **Alt+G** puts it in a named group (empty name ungroups), both in a
  one-line prompt over the tab bar. Groups get a chip you can click to fold
  them; a folded chip shows the member count and the most urgent mark.
  **Alt+A** sets a tab aside: it is dimmed and never notifies. All three are
  also in the command palette.
- **Reorder tabs.** Drag a tab with the mouse, or press
  **Ctrl+Shift+PgUp/PgDn**. A tab steps over a whole group; a grouped tab at
  the group's edge leaves it.
- **Session state line.** Under the sidebar title, and in the compact header,
  the session in view says "Needs you · 2m ago", "Working · 5m",
  "Done · just now", "Failed" or "Inactive · 3h". The compact header also
  counts other tabs that need you.
- **Tabs come back on restart.** Open tabs, their names, groups and folds are
  saved per project and reopened on the next start. Turn off with
  `options.tui.restore_tabs: false`.
- **Vertical tabs (opt-in).** `options.tui.vertical_tabs` lists the sessions,
  grouped, in the sidebar instead of the bar above the chat.
- **Terminal bell (opt-in).** `options.tui.notify_bell` rings the bell when a
  session you aren't looking at starts waiting, finishes or fails.
- **Claude Code sessions in the sidebar (opt-in).**
  `options.tui.claude_sessions` lists the interactive Claude Code sessions
  running on this machine and notifies when one starts waiting for you. It
  only reads `~/.claude/sessions/*.json`; it never talks to the sessions.
- **`needs` theme color** for the "needs you" mark, settable in custom theme
  palettes.
- **Usage limits in the sidebar and `tada status` (opt-in).** Set
  `options.tui.limit_rings` to a local JSON file with each plan's 5 hour and
  weekly usage. Ta-Da only reads the file; some other tool writes it. See
  [Usage limits](README.md#usage-limits).

### Changed

- **Notifications fire once per change.** A burst of permission requests
  sends one "waiting" notification, not one per request.
- **A session stays "working" while queued prompts run.** The "Ta-Da is
  waiting" notification now waits until the queue is empty and the turn
  ended without an error. Canceling a run sends none.

### Fixed

- **A conversation that outgrows the context window recovers on its own.**
  When the provider says the request no longer fits, the session is
  summarized and the request is sent again, once. If the history is too
  big even to summarize, the oldest part is left out of the summary and
  long tool output is shortened. Turned off with auto-summarize.
- **Models without a known context window still auto-summarize.** The
  window size reported in an overflow error is remembered for that model.
- **Auto-summarize leaves room for the reply.** The threshold and Magic
  Context now reserve the model's output tokens, so a full context no
  longer cuts the answer short.
- **`/compact` and `/summarize` find the Summarize Session command.**

### Security

- **Tab, group and Claude Code session names can't send terminal escape
  codes.** Names you type, names restored from the saved tabs file and
  session names read from `~/.claude/sessions` are cleaned of escape
  sequences and control characters and capped at 80 characters.

## [0.5.10] - 2026-10-20

### Security

- **Scripts run by `sh -c`, `eval` and `trap` are graded like the outer
  command.**
  A protected path, glob or run-time path inside `sh -c '...'` (also behind
  `sudo`, `env`, `xargs` or `find -exec`) now asks, as does a script whose
  text is only known at run time.
- **Git config set through the environment is protected.** `GIT_CONFIG_*`,
  `GIT_DIR`, `GIT_EXEC_PATH` and `GIT_TEMPLATE_DIR`, set before a command,
  with `env` or with `export`, are graded like `git -c`. So is a globbed
  git subcommand such as `git con?ig`.
- **Paths that `find` and `xargs` pick at run time are protected.**
  `find -execdir`, `find -exec ... {}` and `xargs` running a command that
  can write files now ask. Read-only tools (`cat`, `grep`, `wc`, `ls`, and
  similar) still run without asking, but only by bare name: `./cat`, or any
  command after the script changes `PATH` (also by name, as in `read PATH`),
  defines an alias or function, uses `hash`, `enable` or `source`, or sets
  `BASH_ENV`, an exported bash function or a loader variable such as
  `LD_PRELOAD` or `DYLD_INSERT_LIBRARIES`, is graded like any other
  program. This includes changes made inside `eval` or `sh -c`.
- **More git environment is protected.** `GIT_WORK_TREE`, `GIT_INDEX_FILE`,
  `GIT_COMMON_DIR` and the object directory variables now ask. So do
  programs git runs (`GIT_PAGER`, `GIT_EDITOR`, `GIT_SSH_COMMAND`,
  `GIT_ASKPASS` and similar), unless set to exactly `cat`, `true`, `false`
  or `:`. Setting a variable whose name is only known at run time
  (`printf -v "$name"`, `declare -n`) asks too.
- **Variables that run or load code ask.** Setting `PS0`, `PS1`, `PS2`,
  `PS4`, `PROMPT_COMMAND`, `SHELLOPTS`, `BASHOPTS`, `BASH_ENV`, `ENV`, an
  exported bash function, `LD_PRELOAD`, `LD_AUDIT`, `DYLD_INSERT_LIBRARIES`
  or `GCONV_PATH` is graded like a protected write. That includes setting
  one through `read`, `printf -v`, `mapfile` or `getopts` (options are
  parsed the way bash does, so `read -raPS4` counts), an array element, or a
  name reference (`declare -n r=PS4`, quoted or not, or `declare -n r` with
  no target yet), and a `mapfile -C` callback. Library search paths such as
  `LD_LIBRARY_PATH` don't ask on their own.

## [0.5.9] - 2026-10-20

### Added

- **`/context` screen.** Turn Magic Context on or off from inside Ta-Da
  (also **Context Settings** in the command palette). It's saved to your
  global config and applies from the next message, with no restart.
- **Magic Context in `tadarc`.** `option magic-context on` turns it on and
  `option magic-context-threshold N` (20 to 90) sets when it compresses
  older turns. The threshold applies on the next start.

### Security

- **Brace expansions and globbed command names are graded.** The grader
  expands `{a,b}` the way the shell does, so `{rm,-rf,x}` counts as a
  delete and `.{g,x}it/hooks/x` as a protected path. A command name with a
  glob (`/bin/r? -rf x`) counts as a delete.
- **Paths and git options known only at run time are protected.** A
  command that writes files and gets an argument from a variable or
  substitution (`cp h $D/hooks/x`) is graded as touching a protected path,
  as is `git $SUB`, `git -c` and `git --config-env`, which can set hooks
  or commands git runs.

## [0.5.8] - 2026-10-20

### Added

- **`/machines` screen.** See every machine you've added, whether it's online
  and how many slots are free. Add one by typing its IP (Ollama and LM Studio
  ports are filled in for you), edit it, switch it on or off, remove it, or
  recheck them all, without leaving Ta-Da. Also in the command palette.

## [0.5.7] - 2026-10-20

### Security

- **Commands named at run time always ask.** A command whose name comes
  from a variable or substitution (`$X -rf dir`, `"$(printf rm)" x`,
  `env $CMD`) can't be graded, so it now counts as a delete. Code handed to
  awk, Lua, Tcl, R, Julia and similar interpreters is checked for deletes
  and network calls too, including shell commands inside it such as awk's
  `system("rm x")`.
- **Quoting no longer hides protected paths from the grader.** Shell
  commands are checked after quotes are removed (`.gi''t`, `".git"/hooks`),
  with globs (`.g?t`), other spellings (`.GIT.`), `git -c x=y config`, and
  output redirected to a path that's only known at run time.

## [0.5.6] - 2026-10-20

### Added

- **Resume old conversations.** `/resume` (or `/continue`) opens a picker
  of this project's saved conversations, newest first, with a preview of
  each one's first and last prompt, message count, cost and dates. Type to
  search titles and the text of every prompt; title matches come first.
  `/resume auth bug` opens it already searching, and it's also in the
  command palette as "Resume Conversation".
- **`tada --resume` (`-r`).** Starts Ta-Da in the resume picker.
  `tada -r auth bug` starts with a search, and `tada -r <id>` opens that
  session directly (the short ID from the exit banner works).

### Security

- **Deletes can't hide from the permission grader.** Shell commands are now
  parsed as well as pattern-matched, so `/bin/rm`, `\rm`, `"rm"`, wrappers
  (`env`, `nice`, `sh -c`, `find -exec`) and interpreter one-liners such as
  `python -c 'shutil.rmtree(...)'` or `node -e 'fs.rmSync(...)'` are graded
  as deletes and always ask. A command that can't be parsed asks too.
- **Edits mode no longer auto-approves protected project files.** Writes to
  `.git/`, `.github/`, `.tada/`, `.claude/`, `.vscode/`, `.idea/`, `.husky/`,
  `tadarc`, `tada.json`, `.mcp.json` and `.envrc`, and shell commands that
  touch them or run `git config`, are graded "outside or protected" and ask,
  even for tools allowed inside the project.

## [0.5.5] - 2026-10-20

### Added

- **Machines.** Tell Ta-Da about other computers on your network that run
  models and how many agents each can run at once (`machine add` in
  `tadarc`, `tada machines add`, or just tell the AI). The AI hands them
  side work through the `agent` tool's `machine` parameter, or the
  `machine_ask` tool with the Claude Code engine. `tada machines` shows
  which are online and how many slots are free.
- **Graded permissions.** Every permission request is graded as read,
  write, outside, network, delete or spend, and the prompt says what kind
  of action it is and why it's asking. `permissions.always_ask` (default
  `delete`) makes a class ask even in Auto and after "allow for session";
  `permissions.auto_budget` caps how many actions of a class Auto approves
  per session.
- **Edits mode.** A fourth permission mode between Plan and Auto that
  approves file edits inside the project and asks for everything else.
  Shift+Tab now cycles Manual, Plan, Edits, Auto.
- **Take back control.** Ctrl+Q turns Auto and Edits off, forgets
  session approvals and denies the waiting request, from any screen.
- **Action log.** Every request and what happened to it (asked, approved
  by you, a rule or a mode, denied) is logged per project.
- **Control cockpit.** Ctrl+\ or Alt+C opens a cockpit with Overview,
  Cost, Actions and Missions pages.
- **Budgets.** `options.budget` sets daily, monthly and per-session spend
  limits in dollars, with warnings at `warn_at` percentages. Reaching one
  asks before the next model call. The status bar shows today's spend.
- **Missions.** Goals with a checklist of done criteria that outlive a
  session. The agent manages them with the new `mission` tool (also
  offered to Claude Code engine sessions), you manage them in the cockpit,
  and the active one shows in the sidebar.
- **`tada status`.** Prints spend, budgets, recent actions and missions
  once and exits. `--plain` is ASCII only, `--json` is for scripts.

## [0.5.1] - 2026-10-20

### Security

- **One-time sign-in links.** Ta-Da used to open the artifacts viewer with
  the viewer key in the link, so the key showed up in the browser's command
  line (visible to other programs through `ps`) and could stay in browser
  history. The link now carries a single-use ticket that expires after two
  minutes; the viewer swaps it for the key and removes it from the address
  bar.
- **Clean file paths before reading.** An absolute `file_path` or `files`
  source with `..` after a symlink (`project/link/../x`) was checked as the
  cleaned path but read through the link, so it could read a file outside
  the project without asking. Paths are now cleaned once, and the check and
  the read use the same path.

## [0.5.0] - 2026-10-20

### Added

- **Artifacts.** The agent can publish a page instead of a wall of text: a
  report, chart, dashboard, small tool, Markdown document or slide deck.
  Pages are stored locally and shown in a viewer on 127.0.0.1 with a
  gallery, versions, comments and a small per-page database (`window.tada`).
  New tools: `artifact`, `artifact_data` and `artifact_comments`, with their
  own tool cards in the chat. Open the gallery with "Open Artifacts Gallery"
  in the command palette or `tada artifacts open`; manage artifacts with
  `tada artifacts list/serve/rm`. Configure with `options.artifacts`.
- **Artifacts over MCP.** `tada artifacts mcp` serves the same tools over
  stdio for Claude Desktop and other MCP clients. Claude Code engine
  sessions get them automatically.
- **Offline by default.** Artifact pages run in a sandboxed frame that
  blocks every outside request. `options.artifacts.allow_cdn` (or
  `--allow-cdn`) allows pinned scripts from a few CDNs and Google Fonts.
- **A private viewer.** The viewer on 127.0.0.1 needs a per-machine key
  (`.viewer-key` in the artifacts folder), so other local programs and web
  pages can't read or change your artifacts. Ta-Da signs the browser in
  when it opens the viewer. Pages get their own derived key and can't
  reach the API. Ta-Da checks that a viewer already on the port is its own
  with a signed reply before reusing it, and the `artifact` tool follows
  symlinks before deciding whether a file is outside the project.

### Changed

- **Faster releases.** The six binaries now build in parallel on separate
  runners instead of one after another, and a finished release is published
  automatically once the Windows installers are attached. A release takes
  about 4 minutes instead of about 10. CI also skips Markdown-only commits
  and no longer runs the race detector on macOS (Linux still does).

## [0.4.3] - 2026-10-20

### Fixed

- **Windows: boxes around inline code and leftover text on the right.**
  Inline code was padded with a no-break space plus an invisible variation
  selector. The Windows console draws that selector as its own "□" cell,
  so every line with inline code ran two cells wider per code span than
  Ta-Da expected. That shifted the line right and left stale text at the
  right edge of the chat. Windows now pads with a plain no-break space.
- **Magic Context recomp warning** uses a plain "⚠" instead of the emoji
  form, which some terminals draw two cells wide.

## [0.4.2] - 2026-10-20

### Fixed

- **Windows: freezes on "Waiting for tool response…".** Shell commands,
  MCP servers and the Claude Code engine now get their own hidden console
  instead of sharing Ta-Da's. Before, a child process could read the keys
  meant for Ta-Da, draw over the screen, or reset the console mode, which
  left typing, Esc, paste and quitting dead. Cancelling now ends the whole
  process tree (an `npm` → `node` chain, or the `node` behind a `claude.cmd`
  shim) rather than just the first process, so a stuck tool can always be
  stopped.
- **Ctrl+V pastes text.** Ctrl+V used to paste only images, so in terminals
  that pass Ctrl+V to the app (Windows conhost) pasting text did nothing. It
  now pastes an image if the clipboard holds one and the model takes images,
  and text otherwise.
- **Logo.** Only the T is ever stretched now; a stretched A looked like a
  bar or an "F", especially in the Windows console font.

## [0.4.1] - 2026-10-20

### Added

- **Folded turns.** Once a turn ends, its tool calls and in-between notes
  fold into one line, such as "▸ Worked for 1m 12s · 8 tool calls · 4 view,
  3 bash, 1 edit". Click it, or select it and press space, to see
  everything. The final answer, errors and approval requests always stay in
  view, and a running turn stays open so you can watch it. Turn it off with
  `options.tui.fold_tools: false`.
- **Steering.** While the agent is working, Ctrl+Enter (or Alt+Enter where
  the terminal can't tell Ctrl+Enter apart) sends your message into the
  running turn instead of waiting for it to end. The built-in agent picks
  it up at its next step; the Claude Code engine hands it to the CLI right
  away. Until the agent takes it in, it shows in the queue pill.
- **Claude Code models and effort.** After your first Claude Code turn,
  the model picker lists the models your account offers, as the CLI
  reports them, instead of a fixed four. Models that support it get a
  reasoning-effort setting (Default, Low, Medium, High, X-High, Max),
  passed to the CLI as `--effort`. The list is read from the CLI's
  startup reply, so it costs no extra request.

### Changed

- Enter while the agent is busy now queues your message as its own turn.
  It used to be folded into the running turn; use Ctrl+Enter for that.
- Claude Code sessions are named from your first message. Ta-Da no longer
  asks another model for the title, so nothing about the session goes to a
  second provider.
- The Claude Code engine now removes more from the CLI's environment so
  it always uses your saved Claude login: `CLAUDE_CODE_OAUTH_TOKEN` and
  other tokens, custom endpoints and headers (`ANTHROPIC_BASE_URL`),
  Bedrock/Vertex/Foundry switches, profiles and model overrides. Before,
  only `ANTHROPIC_API_KEY` was removed. If you sign in through one of
  those, set `use_api_key: true` on the provider to keep them.
- The README's Privacy section is now a table of every connection Ta-Da
  can make, when it happens and how to turn it off, matching the website.

### Fixed

- `schema.json` now includes the Magic Context options.

## [0.4.0] - 2026-10-19

### Added

- **Claude Code engine.** When the `claude` CLI is installed, a "Claude Code"
  provider shows up in the model picker (default model, Opus, Sonnet or
  Haiku). Ta-Da runs your own `claude` for each turn and shows its output,
  tool cards and diffs in the chat. Its tool requests go through Ta-Da's
  permission prompts, and when Claude wants to ask you something it uses
  Ta-Da's question dialog. Plan mode maps to Claude Code's plan mode. Login
  and billing stay with the CLI; set `use_api_key: true` in the provider's
  `provider_options` to bill `ANTHROPIC_API_KEY` instead, or `binary` to
  point at a different CLI.
- **Magic Context**, a Go port of
  [cortexkit/magic-context](https://github.com/cortexkit/magic-context) (MIT).
  Off by default; turn it on with `options.magic_context.enabled: true`. It
  replaces auto-summarize for the main agent: a background historian
  compresses older turns, the agent can drop old tool output with
  `ctx_reduce`, and project memories, notes and search (`ctx_memory`,
  `ctx_note`, `ctx_search`, `ctx_expand`) carry over between sessions.
  Commands: `ctx-status`, `ctx-flush`, `ctx-recomp`, `ctx-wrapup` and
  `ctx-dream`. Data stays in the local database; embeddings are off unless
  you set an endpoint.

### Fixed

- A new tab can be opened while the current session is still working.
- OpenCode plugin entries written as `[name, options]` pairs are read
  correctly.

### Notes

- Claude Code sessions run Claude Code's own loop, tools and compaction, so
  Magic Context and Summarize don't apply to them, and Ta-Da's system prompt
  isn't sent. Cost is only shown with `use_api_key`.

## [0.3.2] - 2026-10-19

### Added

- Three named permission modes: **Manual** (asks first), **Plan** (read-only
  planner) and **Auto** (skips prompts; system level commands stay blocked).
  Shift+Tab cycles Manual → Plan → Auto, and the status bar always shows the
  current mode. Auto replaces the old YOLO name; `--auto` joins `--yolo`, and
  the plan hand-off's "Code in Auto" button also answers to `a`.

### Changed

- The tab bar now shows as soon as a session is open, so its `+` button is
  there to start a second session.
- Auto turned on with Ctrl+Y no longer gets carried into plan mode; Shift+Tab
  from Auto always goes back to Manual.

## [0.3.1] - 2026-10-19

### Security

- Tables and charts no longer pass terminal control characters through.
  Escape sequences in a chart's title, labels, series names or unit, or in a
  table cell, are stripped before drawing, so a reply can't retitle the
  window or clear the screen.
- Charts with extreme values (beyond ±1e300) are refused with a
  `chart: value … out of range` note, and very large or very tiny values can
  no longer crash the chart renderer.

## [0.3.0] - 2026-10-19

### Added

- **Tables and charts in replies.** Markdown tables in assistant replies are
  drawn as styled tables: amber header, faint row stripes, numbers lined up
  on the decimal point, `:--:` alignment, and bold, italic and code cells.
  Tables that are too wide shrink their text columns with `…` (number
  columns are never cut) and fall back to stacked rows when nothing fits.
  A fenced ```` ```chart ```` block with JSON is drawn as a bar, horizontal
  bar, braille line or sparkline chart, with a legend for several series.
  A broken chart shows its code block and a dim `chart: <error>` line. It is
  all drawn locally, follows the active theme and `NO_COLOR`, and copying a
  message still copies the raw markdown. Turn it off with
  `options.tui.rich_blocks: false` or `TADA_NO_RICH=1`.
- In the TUI, the agent's system prompt gets a short note on tables and
  `chart` blocks (only when `rich_blocks` is on). `tada run` and sub-agents
  don't get it.
- **Session tabs.** Sessions you open stay as tabs above the chat. Click a
  tab or press `alt+1`…`alt+9` to switch, `ctrl+pgdown`/`ctrl+pgup` for the
  next or previous tab, `alt+w` or the tab's `×` (or a middle click) to
  close it, and `+` for a new session. The commands palette has Next Tab,
  Previous Tab and Close Tab. The tab bar only shows with two or more tabs.

### Changed

- Diffs in edit and write tool results wrap long lines instead of cutting
  them off, in both unified and split views.

### Fixed

- The header wordmark in wide (fullscreen) windows: the D is never stretched
  into a box, and the dotted field beside it ends in a straight edge instead
  of a ragged one.

## [0.2.0] - 2026-10-19

### Changed

- **Renamed to Ta-Da.** The binary is now `tada`, environment variables are
  `TADA_*`, config lives in `~/.config/tada/tada.json`, `tadarc`, `.tada/` and
  `TADA.md`. The repository moved to
  [ItsCetla/tada](https://github.com/ItsCetla/tada) and the site to
  [tada.cetla.dev](https://tada.cetla.dev).
- On first run, settings and data from `~/.config/crush` and
  `~/.local/share/crush` are copied into the new Ta-Da folders, and `CRUSH_*`
  environment variables are still honoured. Project `.crush/` session history
  is not moved; `.crush/skills` and `.crushignore` are still read.
- New "cetla" default theme, Ta-Da wordmark and a short startup intro
  (skip with any key; turn off with `TADA_NO_INTRO=1` or
  `options.tui.intro: false`).

### Added

- `tada update` (`--check`, `-y`): downloads the latest release from GitHub,
  checks it against `SHA256SUMS.txt` and replaces the binary.
- Background update check: at most every 6 hours, asks api.github.com for the
  latest release and tells you once per new version. Never downloads on its
  own. Turn it off with `options.update_check: false`,
  `option update-check false` or `TADA_NO_UPDATE_CHECK=1`.
- Windows installer (`tada_<version>_Windows_<arch>_Setup.exe`): per-user
  install without admin rights, adds `tada` to `PATH`, Start menu entry,
  uninstaller in Settings → Apps. Upgrading from the 0.1.0 Crush installer
  removes the old `crush.exe` and shortcuts.

## [0.1.0] - 2026-10-19

First tagged release of the fork, based on upstream Crush as of 2026-10-15.

### Added

- `crush trust`, `crush trust --list` and `crush trust --revoke`: a trust gate
  that keeps a cloned project's `crush.json`, `crushrc`, `.crush/` and
  OpenCode config inert until you say so. `CRUSH_TRUST_PROJECT=1` and
  `options.trusted_paths` are the non-interactive equivalents.
- OpenCode import: MCP servers, providers, model choices, the cached
  models.dev catalog, rules, skills, custom commands (`opencode:<name>`),
  tool toggles, deny permissions, `edit: allow` and `compaction.auto` are
  read from a local OpenCode install. `crush opencode` prints exactly what
  was imported and why anything was skipped. Toggle pieces in
  `options.opencode`; `CRUSH_DISABLE_OPENCODE=1` turns it all off.
- `permissions allow-in-project edit write …` (`permissions.project_allowed_tools`)
  lets file tools skip the prompt only inside the project directory.
- `option web-tools true` / `options.enable_web_tools` / `CRUSH_ENABLE_WEB_TOOLS`
  to opt in to the `fetch`, `agentic_fetch`, `download` and `sourcegraph` tools.
- The import report flags MCP servers that fetch packages on start (`npx`,
  `bunx`, `pnpm dlx`, `yarn dlx`, `@latest`).
- Windows, macOS and Linux release archives with `SHA256SUMS.txt`, built by
  the `release` workflow as a draft GitHub release.

### Changed

- The provider catalog is offline by default; refresh it only with
  `crush update-providers`.
- Bash auto-approval only applies to a single parsed read-only command.
  Wrappers (`env`, `timeout`, …), redirections, pipes and multi-line commands
  always ask. `env` dumps, `ps e` and `ls` outside the working directory ask too.
- Every web tool call and every `agentic_fetch` sub-agent request asks for
  permission first; `sourcegraph` asks before each search.
- `du` and `git blame` can no longer read files outside the project.
- Banned shell commands are also caught when hidden behind wrappers or paths.
- Config and lock directories are created readable only by the user, and the
  server's unix socket is private to its owner.
- Panic logs go to the log directory instead of the working directory.
- `crush --version` prefers the version passed at build time over the Go
  toolchain's VCS stamp.

### Removed

- PostHog telemetry and machine-id tracking.
- The startup update check.
- The Charm Hyper provider and the Catwalk/Hyper catalog fetches at startup.
- Automatic `.env` loading, the forced Termux DNS resolvers and the global
  pprof mux.
- Upstream release, CLA and labeler automation.

### Security

- Provider API keys are scrubbed from the agent's shell environment.
- Credential headers and key-like query parameters are redacted from HTTP
  debug logs; env values that fail to resolve are no longer logged.
- The `download` tool caps writes at 100 MiB and removes oversized files.
- The lint workflow no longer persists checkout credentials.

[Unreleased]: https://github.com/ItsCetla/tada/compare/v0.6.2...HEAD
[0.6.2]: https://github.com/ItsCetla/tada/compare/v0.6.1...v0.6.2
[0.6.1]: https://github.com/ItsCetla/tada/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/ItsCetla/tada/compare/v0.5.14...v0.6.0
[0.5.14]: https://github.com/ItsCetla/tada/compare/v0.5.13...v0.5.14
[0.5.13]: https://github.com/ItsCetla/tada/compare/v0.5.12...v0.5.13
[0.5.12]: https://github.com/ItsCetla/tada/compare/v0.5.11...v0.5.12
[0.5.11]: https://github.com/ItsCetla/tada/compare/v0.5.10...v0.5.11
[0.5.10]: https://github.com/ItsCetla/tada/compare/v0.5.9...v0.5.10
[0.5.9]: https://github.com/ItsCetla/tada/compare/v0.5.8...v0.5.9
[0.5.8]: https://github.com/ItsCetla/tada/compare/v0.5.7...v0.5.8
[0.5.7]: https://github.com/ItsCetla/tada/compare/v0.5.6...v0.5.7
[0.5.6]: https://github.com/ItsCetla/tada/compare/v0.5.5...v0.5.6
[0.5.5]: https://github.com/ItsCetla/tada/compare/v0.5.1...v0.5.5
[0.5.1]: https://github.com/ItsCetla/tada/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/ItsCetla/tada/compare/v0.4.3...v0.5.0
[0.4.3]: https://github.com/ItsCetla/tada/compare/v0.4.2...v0.4.3
[0.4.2]: https://github.com/ItsCetla/tada/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/ItsCetla/tada/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/ItsCetla/tada/compare/v0.3.2...v0.4.0
[0.3.2]: https://github.com/ItsCetla/tada/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/ItsCetla/tada/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/ItsCetla/tada/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ItsCetla/tada/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ItsCetla/tada/releases/tag/v0.1.0
