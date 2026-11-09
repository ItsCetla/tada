---
name: tada-config
description: Use to configure Ta-Da (tadarc or tada.json) providers, models, LSPs, MCP servers, hooks, skills, permissions.
---

# Ta-Da Configuration

Ta-Da supports two config formats:

- **`tadarc`** — a Bash script that builds config by calling Ta-Da builtins.
  **Preferred.** Because it is real Bash you get includes, secrets,
  conditionals, and variables for free.
- **`tada.json`** — static JSON. Fully supported; see
  [Legacy JSON format](#legacy-json-format).

Both are discovered together and deep-merged. Priority (highest to lowest):

1. `.tadarc` / `tadarc` / `.tada.json` / `tada.json` (project-local,
   closer-to-cwd wins; Windows uses `.\.tadarc` / `.\tadarc`)
2. `$XDG_CONFIG_HOME/tada/tadarc` or `~/.config/tada/tadarc`
   (`%XDG_CONFIG_HOME%\tada\tadarc` or
   `%USERPROFILE%\.config\tada\tadarc` on Windows)

Data directories (`~/.local/share/tada` and `%LOCALAPPDATA%\tada`) contain
machine-owned JSON state only; Ta-Da does not discover or execute a `tadarc`
from those locations.

If a directory has both `tadarc` and `tada.json`, they merge (`tadarc` wins
on conflicts) and Ta-Da logs a warning.

## tadarc at a glance

A `tadarc` is a plain Bash script executed at load time with the same embedded
shell the `bash` tool uses. It builds config by calling builtins (`provider`,
`model`, `mcp`, `lsp`, `hook`, `permissions`, `option`). Statements run top to
bottom; later statements win, and `remove`/`reset` operate on anything defined
earlier or pulled in via `source`.

```bash
#!/usr/bin/env bash
# Includes and secrets are just Bash.
source ~/.config/tada/shared.sh

provider add anthropic --api-key "$ANTHROPIC_API_KEY"

model large anthropic/claude-sonnet-4-20250514 --max-tokens 16384
model small anthropic/claude-haiku-4-20250514

option skill-path ./skills
permissions allow view ls grep edit
```

Values are ordinary Bash — quote and expand normally (`"$VAR"`, `$(cmd)`,
`${VAR:?required}`). A failing `$(command)` aborts the load.

`TADA_VERSION` is exported into the script so you can feature-detect the
running Ta-Da (it is the literal `devel` for local builds):

```bash
[[ "$TADA_VERSION" != devel ]] && lsp add gopls --command gopls
```

## Commands

All entity commands are verb-first. `remove` accepts `rm` as an alias. Booleans
accept `true/false/1/0/yes/no`, case-insensitive.

### providers

```bash
provider add <id> [flags]    # define/update; repeated calls merge
provider remove <id>         # alias: rm — removes the provider and its models
```

Flags: `--name`, `--type` (`openai`, `openai-compat`, `anthropic`, or a local
type like `ollama`, `lmstudio`, `llamacpp`), `--api-key`, `--base-url`,
`--disable BOOL`, `--flat-rate BOOL`, `--discover-models BOOL`,
`--system-prompt-prefix TEXT`, `--extra-header KEY VALUE` (repeatable),
`--extra-body JSON`, `--provider-options JSON`.

```bash
provider add deepseek \
  --type openai-compat \
  --base-url "https://api.deepseek.com/v1" \
  --api-key "${DEEPSEEK_API_KEY:?set DEEPSEEK_API_KEY}"
```

### models

```bash
model add <provider>/<id> [flags]      # register a custom model (provider must exist)
model remove <provider>/<id>           # alias: rm
model large [<provider>/<id>] [flags]  # set the large slot; no arg prints it
model small [<provider>/<id>] [flags]  # set the small slot; no arg prints it
```

- `<provider>/<id>` is the same form `tada models` prints. A missing slash is
  an error. `model add` requires the provider to already exist.
- `model add` flags: `--name`, `--context-window N`, `--default-max-tokens N`,
  `--can-reason BOOL`, `--supports-images BOOL`, `--price-input F`,
  `--price-output F`, `--price-cache-create F`, `--price-cache-hit F`,
  `--reasoning-effort low|medium|high`.
- `model large`/`model small` flags: `--think`, `--reasoning-effort`,
  `--max-tokens N`, `--temperature F`, `--top-p F`, `--top-k N`,
  `--frequency-penalty F`, `--presence-penalty F`, `--provider-options JSON`.
- `model large` with no argument prints the current selection as `provider/id`,
  usable in `$(model large)`.

`large` is the primary coding model; `small` is used for summarization.

### mcp

```bash
mcp add <name> --type stdio|sse|http [flags]   # default type is stdio
mcp remove <name>                              # alias: rm
```

Flags: `--command CMD`, `--args ARG` (repeatable), `--env KEY VALUE`
(repeatable), `--url URL`, `--header KEY VALUE` (repeatable), `--timeout N`,
`--disabled BOOL`, `--disabled-tools TOOL` (repeatable), `--enabled-tools TOOL`
(repeatable), `--oauth BOOL`, `--oauth-client-id ID`, `--oauth-client-secret SECRET`,
`--oauth-callback-port PORT`.

```bash
mcp add github --type http \
  --url "https://api.githubcopilot.com/mcp/" \
  --header Authorization "Bearer $GH_PAT"

mcp add filesystem --command node --args /path/to/mcp-server.js
```

### lsp

```bash
lsp add <name> --command CMD [flags]
lsp remove <name>                     # alias: rm
```

Flags: `--args ARG` (repeatable), `--env KEY VALUE` (repeatable),
`--filetypes TYPE` (repeatable), `--root-markers MARKER` (repeatable),
`--timeout N`, `--disabled BOOL`, `--init-options JSON`, `--options JSON`.

```bash
lsp add go --command gopls --env GOPATH "$HOME/go"
lsp add typescript --command typescript-language-server --args --stdio
```

### hooks

```bash
hook add <event> --command CMD [--name NAME] [--matcher REGEX] [--timeout N]
hook remove <event> [--name NAME]    # alias: rm; without --name clears the event
```

Only named hooks can be removed individually — give a hook `--name` if you
intend to remove it later. See [Hooks runtime](#hooks-runtime) for how hooks
execute (stdin payload, env vars, decisions).

```bash
hook add PreToolUse --matcher "^bash$" --command ".tada/hooks/no-haskell.sh" --name no-haskell
```

### permissions

```bash
permissions allow <tool> [<tool> ...]   # tools that skip permission prompts
permissions allow-in-project <tool> ... # file tools, inside the project only
permissions deny <tool> [<tool> ...]    # hide tools from the agent entirely
```

`deny` is the inverse of `allow`: it writes `options.disabled_tools`. A denied
tool is hidden from the agent, not merely prompted for.

`artifact`, `artifact_data`, `artifact_comments` and `machines` are off by
default; list them in `options.enabled_tools` to turn them on (`machines` is
also on whenever `machines` are configured).

`allow-in-project` (`permissions.project_allowed_tools` in tada.json) is
narrower than `allow`: the listed file tools (`edit`, `multiedit`, `write`, `lsp_replace_symbol`)
skip the prompt only for files inside the project, and still ask for paths
outside it. It takes effect on the next start.

### options

```bash
option <key> [value]
option reset <list-key>    # clear a list option back to empty
```

- **Boolean keys** (value optional, defaults `true`): `debug`, `debug-lsp`,
  `auto-lsp`, `progress`, `web-tools`. `web-tools` turns on the tools that
  reach the internet (`fetch`, `agentic_fetch`, `download`, `sourcegraph`);
  they are hidden until it is set, and still ask before every request.
  `TADA_ENABLE_WEB_TOOLS` overrides it.
- **Boolean keys phrased positively** (stored as the negated field):
  `auto-summarize`, `default-providers`. Example: `option auto-summarize false`
  disables automatic summarization. `provider-auto-update` and `metrics` are
  still accepted but deprecated and have no effect: the provider catalog only
  changes through `tada update-providers`, and this fork has no telemetry.
- **Claude Code engine**: `option claude-code-strict-mcp true` runs the
  `claude` CLI with `--strict-mcp-config`, so it loads only Ta-Da's own MCP
  servers and skips the ones in the user's Claude Code settings (faster
  turns, fewer tool tokens; claude.ai connectors and global MCP servers are
  then unavailable). Default `false`.
- **Tool profile**: `option tool-profile lean` (`options.tool_profile`,
  `full` or `lean`, default `full`) keeps only the core built-in tools
  (`agent`, `bash`, `edit`, `multiedit`, `view`, `write`, `grep`, `glob`,
  `ls`, `question`). Add tools back one per line with `option enable-tool`
  (appends to `options.enabled_tools`), e.g. `option enable-tool lsp_rename`
  and `option enable-tool lsp_replace_symbol`. Lean also hides MCP server
  tools; bring a whole server back with `option enable-tool mcp_<server>` or
  one tool with its full name (`mcp_<server>_<tool>`).
- **Compaction**: `option compaction-reserve-tokens N`
  (`options.compaction_reserve_tokens`, default 16384) is the free context
  kept before the conversation is summarized; `option
  compaction-keep-recent-tokens N` (`options.compaction_keep_recent_tokens`,
  default 20000) is how much of the newest conversation a summary keeps word
  for word. Both are capped by the model's window (half and a quarter).
- **String keys**: `data-directory`, `initialize-as`, `notifications`,
  `worktree-dir`.
- **Attribution keys**: `attribution-trailer-style` (`none`, `co-authored-by`,
  `assisted-by`) and `attribution-generated-with` (boolean).
- **UI settings**: `option ui compact BOOL`, `option ui diff auto|unified|split`,
  `option ui transparent BOOL`, `option ui mouse BOOL` (default `true`;
  disable to let the terminal/tmux handle selection, copy/paste, and
  scrolling), `option ui scrollbar default|always|never`,
  `option ui exit-banner default|compact|none`,
  `option ui completions-max-depth N`, `option ui completions-max-items N`.
- **List keys** (singular, one value per call, repeatable): `context-path`,
  `global-context-path`, `skill-path`, `disable-skill`. Use `option reset <key>`
  to wipe inherited values (e.g. after `source`).

```bash
option progress false
option skill-path ./skills
option disable-skill tada-config
option attribution-trailer-style assisted-by
option attribution-generated-with true
option ui compact true
option ui diff unified
option ui exit-banner compact
```

> [!IMPORTANT] These skill paths are loaded by default and do NOT need
> `skill-path`: `.agents/skills`, `.tada/skills`, `.claude/skills`,
> `.cursor/skills`. Claude Code's user skills (`~/.claude/skills`) are not
> loaded by default; opt in with `option skill-path ~/.claude/skills`.

## Hooks runtime

Hooks are user-defined shell commands that fire on agent events. Currently only
`PreToolUse` is supported, which runs before a tool executes. This behavior is
the same however the hook is defined (`hook add` or JSON).

### How hooks work

1. When a tool is about to be called, all `PreToolUse` hooks with a matching
   `matcher` (or no matcher) run in parallel.
2. Duplicate commands are deduplicated — each unique command runs at most once.
3. The hook receives JSON on **stdin** and hook-specific **environment
   variables**.

Event names are case-insensitive and accept snake_case: `PreToolUse`,
`pretooluse`, `pre_tool_use`, `PRE_TOOL_USE` all work.

### Hook input (stdin)

```json
{
  "event": "PreToolUse",
  "session_id": "abc-123",
  "cwd": "/path/to/project",
  "tool_name": "bash",
  "tool_input": { "command": "ls -la" }
}
```

### Hook environment variables

| Variable                     | Description                                       |
| ---------------------------- | ------------------------------------------------- |
| `TADA_EVENT`                | Event name (e.g. `PreToolUse`)                    |
| `TADA_TOOL_NAME`            | Name of the tool being called                     |
| `TADA_SESSION_ID`           | Current session ID                                |
| `TADA_CWD`                  | Current working directory                         |
| `TADA_PROJECT_DIR`          | Project root directory                            |
| `TADA_TOOL_INPUT_COMMAND`   | Value of `command` from tool input (if present)   |
| `TADA_TOOL_INPUT_FILE_PATH` | Value of `file_path` from tool input (if present) |

### Hook output

**Exit code 0** — hook succeeded. Stdout is parsed as JSON:

```json
{ "decision": "allow", "context": "optional context appended to tool result" }
```

- `decision`: `allow` to explicitly allow, `deny` to block, `none` (or omit).
- `reason`: explanation (used when denying).
- `context`: extra context appended to the tool result.
- `updated_input`: replacement JSON for the tool input; last non-empty wins.

**Exit code 2** — the tool call is blocked; stderr is the deny reason.

**Any other exit code** — non-blocking error; the tool call proceeds.

### Decision aggregation

- **Deny wins over allow** — any deny blocks the call.
- **Allow wins over none** — a lone allow lets it proceed.
- Deny reasons and context strings are concatenated (newline-separated).
- For `updated_input`, the last non-empty value wins.

### Claude Code compatibility

Ta-Da also accepts the Claude Code hook output format, so existing hooks work
unchanged:

```json
{
  "hookSpecificOutput": {
    "permissionDecision": "allow",
    "permissionDecisionReason": "Auto-approved",
    "updatedInput": { "command": "echo rewritten" }
  }
}
```

## User-invocable skills

Skills can be invoked as commands. Add `user-invocable: true` to the skill's
YAML frontmatter:

```yaml
---
name: my-skill
description: A skill that can be invoked as a command.
user-invocable: true
---
```

- Global skills appear as `user:skill-name`; project skills as
  `project:skill-name`.
- Add `disable-model-invocation: true` to keep a skill user-only (hidden from
  the model's available-skills list but still manually invocable).

## Environment variables

- `TADA_VERSION` — exported into `tadarc` at load; the running version (or
  `devel` for local builds).
- `TADA_GLOBAL_CONFIG` — override global config location.
- `TADA_GLOBAL_DATA` — override data directory location.
- `TADA_SKILLS_DIR` — override default skills directory.

## Legacy JSON format

`tada.json` is the original static format. It still works and merges with
`tadarc`. Basic structure:

```json
{
  "$schema": "https://tada.cetla.dev.json",
  "models": {},
  "providers": {},
  "mcp": {},
  "lsp": {},
  "hooks": {},
  "options": {},
  "permissions": {}
}
```

The `$schema` property enables IDE autocomplete but is optional.

### tadarc ↔ tada.json mapping

| tadarc                             | tada.json                                             |
| ------------------------------------ | ------------------------------------------------------ |
| `provider add openai --api-key "$K"` | `providers.openai = {"api_key": "$K"}`                 |
| `model add openai/gpt-x --name X`    | append to `providers.openai.models[]`                  |
| `model large openai/gpt-x`           | `models.large = {"provider":"openai","model":"gpt-x"}` |
| `mcp add gh --type http --url U`     | `mcp.gh = {"type":"http","url":"U"}`                   |
| `lsp add go --command gopls`         | `lsp.go = {"command":"gopls"}`                         |
| `hook add PreToolUse --command C`    | append to `hooks.PreToolUse[]`                         |
| `permissions allow view ls`          | `permissions.allowed_tools = ["view","ls"]`            |
| `permissions deny bash`              | `options.disabled_tools = ["bash"]`                    |
| `option skill-path ./skills`         | `options.skills_paths = ["./skills"]`                  |
| `option auto-summarize false`        | `options.disable_auto_summarize = true`                |
| `option request-timeout 300`          | `options.request_timeout = 300`                        |
| `option attribution-trailer-style none` | `options.attribution.trailer_style = "none"`        |
| `option attribution-generated-with false` | `options.attribution.generated_with = false`       |

### Shell expansion in tada.json

In JSON, only selected string fields are run through the embedded shell at load
time (in `tadarc`, everything is native Bash so this table does not apply):

| Surface                                                         | Expansion                          |
| --------------------------------------------------------------- | ---------------------------------- |
| Provider `api_key`, `base_url`, `api_endpoint`, `extra_headers` | yes                                |
| Provider `extra_body`                                           | **no** (JSON passthrough)          |
| MCP `command`, `args`, `env`, `headers`, `url`                  | yes                                |
| LSP `command`, `args`, `env`                                    | yes                                |
| Hook `command`                                                  | runs via `sh -c`, not the resolver |

Supported constructs: `$VAR`, `${VAR}`, `${VAR:-default}`, `${VAR:+alt}`,
`${VAR:?message}`, `$(command)`. An unset variable expands to empty; a failing
`$(command)` is a hard error. A header that resolves to empty is dropped from
the request.

### Security note

Both formats are trusted code. `tadarc` runs entirely, and any `$(...)` in
`tada.json` runs at load time, with the invoking user's shell privileges,
before the UI appears.

So project-local config (including the workspace config
`.tada/tada.json`) is ignored until the user trusts the project with
`tada trust` (see `tada trust --help`). Ta-Da stores a fingerprint of those
files; any change, new file or removed file makes the project untrusted again
until the user reviews it and runs `tada trust` once more. Directories in
`options.trusted_paths` of the global config, or runs with
`TADA_TRUST_PROJECT=1`, skip the check. If a user's project config seems to
be ignored, suggest reviewing it and running `tada trust`; never run it on
their behalf.

### OpenCode import

Ta-Da also imports from a local OpenCode install
(`~/.config/opencode/opencode.json(c)`, `.opencode/` folders and OpenCode's
cache/database), reading files only; OpenCode plugins are never run:

- MCP servers, and provider settings (base URLs, API keys, custom models,
  `enabled_providers` / `disabled_providers`).
- The model choice: the default agent's model, else `model`, and
  `small_model`, when Ta-Da has no model selected.
- OpenCode's cached models.dev catalog (tool-calling chat models only).
- `instructions` and global `AGENTS.md` as context files, skill folders,
  custom commands (run as `opencode:<name>`), tool toggles and `deny`
  permissions (hide tools), `edit: allow` (as
  `permissions.project_allowed_tools`), and `compaction.auto: false`.

Imported values live in memory only and `tada.json` always wins.
Project-level `opencode.json(c)` and `.opencode/` files are covered by the
same trust check. `tada opencode` shows what was imported and what was
skipped and why. Configure it with `options.opencode`: `mcp`, `providers`
(also the model choice), `models`, `instructions`, `skills`, `commands`,
`tools`, `permissions` and `compaction` default to true; `credentials`
defaults to false and uses `opencode auth login` API keys; `disabled` turns
it all off, as does `TADA_DISABLE_OPENCODE=1`. When a user wants an
OpenCode setting changed only for Ta-Da, set it in tada.json instead of
editing their OpenCode config.
