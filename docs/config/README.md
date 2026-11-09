# Config

> [!NOTE]
> This document was designed for both humans and agents.

> [!TIP]
>
> Ta-Da can configure itself via a builtin config skill. That is to say,
> can generally just tell Ta-Da want you want to configure using natural
> language.
>
> If you're migrating from the old JSON format, you can also ask Ta-Da to
> convert the config for you.

Ta-Da is configured with Bash via a set of Ta-Da-specific builtin commands. By
default, global config lives at `~/.config/tada/tadarc` on Unix-like systems
and `%USERPROFILE%\.config\tada\tadarc` on Windows. It works like a `.bashrc`:
it runs when Ta-Da starts and configures the agent.

```bash
# Add Ollama.
provider add ollama --type ollama --base-url "http://localhost:11434/v1"

# Register a model on Ollama.
model add ollama/llama3.3 --name "Llama 3.3" --context-window 128000

# Auto-approve some tools.
permissions allow view edit

# Add an MCP server
mcp add github \
  --type http \
  --url "https://api.githubcopilot.com/mcp/" \
  --header Authorization "Bearer $GITHUB_TOKEN"
```

Since it’s Bash, so you can use logic, `source` other files, and so on. It’s
really handy.

```bash
# Change config based on the machine you're on.
if [[ $HOSTNAME == "babysquid" ]]; then
    option skill-path "$HOME/squid-skills"
fi

# Load some extra config
source "$XDG_CONFIG_HOME/squid-config.sh"

# Get API keys from your password manager.
provider add my-secret-provider \
  --type openai-compat \
  --base-url "https://api.example.com/v1" \
  --api-key "$(op read my-secret-key)"
```

## Why Bash?

Two reasons:

1. Ta-Da ships with a first-class Bash interpreter, so we get the logic for
   free.
2. Ultimately, Ta-Da needs to be able to configure itself, and command-based
   config allows both users and the agent to use the same tools.

## What about JSON?

JSON is still supported but is deprecated and, while it's supported, it won't
be receiving new features. For more see [Legacy JSON](#legacy-json).

## Config versioning

Not breaking the config API is really important to us! That said, you can
target specific Ta-Da versions with `$TADA_VERSION`:

```bash
if [[ $TADA_VERSION == "0.85.*" ]]; then
    option debug true
fi
```

## Security

Just like `tada.json`, `tadarc` is a trusted file. Guard it carefully and
don't download random configs without reading them first.

## Where config lives

Ta-Da looks for config in the following places, with lower numbers taking
precedence:

| Priority | Unix-like                        | Windows                           |
| -------- | -------------------------------- | --------------------------------- |
| 1        | `./.tadarc`                     | `.\.tadarc`                      |
| 2        | `./tadarc`                      | `.\tadarc`                       |
| 3        | `$XDG_CONFIG_HOME/tada/tadarc` | `%XDG_CONFIG_HOME%\tada\tadarc` |

Legacy JSON uses `.tada.json` / `tada.json` in the same directories as the
above. Everything found is merged, with project settings overriding global ones
and `tadarc` overriding JSON in the same directory. If a folder has both, they
merge and Ta-Da logs a warning.

Data directories (`~/.local/share/tada` on Unix-like systems and
`%LOCALAPPDATA%\tada` on Windows) contain machine-owned JSON state. Ta-Da does
not discover or execute a `tadarc` from those locations.

The `/settings` screen saves its changes to `tada.json` in the global data
directory. A project config still overrides those values, and a setting
forced by an environment variable cannot be changed there.

> [!NOTE]
> Ta-Da also stores state data in `$XDG_DATA_HOME/tada`
> (`%LOCALAPPDATA%\tada` on Windows). This is application state, and should
> not be edited by hand.

## Command Reference

The sections below read like CLI help. Entity commands use `add` to create or
update something and `remove` (or `rm`) to delete it. Booleans accept
`true/false/1/0/yes/no`, in any case.

```text
Available Commands:
  provider      Manage model providers
  model         Manage models and model selection
  mcp           Manage MCP servers
  lsp           Manage language servers
  hook          Manage hooks
  machine       Declare other computers that run models
  permissions   Configure tool permissions
  option        Configure general Ta-Da behavior
```

### provider

Manage model providers.

```text
Usage:
  provider [command]

Available Commands:
  add       Add or update a provider
  remove    Remove a provider and its custom models
  rm        Alias for remove
```

#### `provider add`

Add a provider, or update an existing provider with the same ID.

```text
Usage:
  provider add <id> [flags]

Flags:
      --name string                 display name
      --type string                 provider type (openai, openai-compat, anthropic, ollama, …)
      --api-key string              API key
      --base-url string             API base URL
      --disable bool                disable without removing
      --flat-rate bool              use flat-rate billing
      --discover-models bool        auto-discover and merge provider models
      --system-prompt-prefix string text prepended to the system prompt
      --extra-header key value      add an HTTP header (repeatable)
      --extra-body JSON             merge a JSON object into request bodies
      --provider-options JSON       merge a provider-specific JSON object
```

```bash
provider add deepseek \
  --type openai-compat \
  --base-url "https://api.deepseek.com/v1" \
  --api-key "${DEEPSEEK_API_KEY:?set DEEPSEEK_API_KEY}"
```

Headers whose value resolves to the empty string (an unset `$VAR`, a
`$(...)` that prints nothing, or a literal `""`) are dropped from the
outgoing request. This makes env-gated headers safe:

```bash
provider add openai \
  --extra-header OpenAI-Organization "$OPENAI_ORG_ID"
```

If `OPENAI_ORG_ID` is unset, the header is simply not sent.

#### `provider remove`

Remove a provider and all custom models registered on it.

```text
Usage:
  provider remove <id>
  provider rm <id>
```

### model

Manage custom models and the large/small model slots. Model references use the
same `<provider>/<id>` form printed by `tada models`.

```text
Usage:
  model [command]

Available Commands:
  add       Register a custom model on an existing provider
  remove    Remove a custom model
  rm        Alias for remove
  large     Set or print the large model
  small     Set or print the small model
```

#### `model add`

Register a custom model on an existing provider.

```text
Usage:
  model add <provider>/<id> [flags]

Flags:
      --name string                 display name
      --context-window int          context window in tokens
      --default-max-tokens int      default maximum output tokens
      --can-reason bool             model supports reasoning
      --supports-images bool        model accepts image input
      --price-input float           input price per 1M tokens
      --price-output float          output price per 1M tokens
      --price-cache-create float    cache-creation price per 1M tokens
      --price-cache-hit float       cache-hit price per 1M tokens
      --reasoning-effort string     low, medium, or high
```

#### `model remove`

Remove a custom model from its provider.

```text
Usage:
  model remove <provider>/<id>
  model rm <provider>/<id>
```

#### `model large`, `model small`

Set the large or small model slot. With no model argument, print the current
selection.

```text
Usage:
  model large [<provider>/<id>] [flags]
  model small [<provider>/<id>] [flags]

Flags:
      --think                       enable thinking mode
      --reasoning-effort string     low, medium, or high
      --max-tokens int              maximum output tokens
      --temperature float           sampling temperature
      --top-p float                 top-p sampling (0–1)
      --top-k int                   top-k sampling
      --frequency-penalty float     frequency penalty
      --presence-penalty float      presence penalty
      --provider-options JSON       merge a provider-specific JSON object
```

```bash
model large openai/gpt-4o --think
echo "coding with: $(model large)"   # prints: openai/gpt-4o
```

### mcp

Manage Model Context Protocol servers.

```text
Usage:
  mcp [command]

Available Commands:
  add       Add or update an MCP server
  remove    Remove an MCP server
  rm        Alias for remove
```

#### `mcp add`

Add an MCP server, or update an existing server with the same name.

```text
Usage:
  mcp add <name> [flags]

Flags:
      --type string              stdio, sse, or http (default "stdio")
      --command string           executable for stdio servers
      --args string              command argument (repeatable)
      --env key value            environment variable (repeatable)
      --url string               URL for HTTP/SSE servers
      --header key value         HTTP header (repeatable)
      --timeout int              startup timeout in seconds
      --disabled bool            disable without removing
      --disabled-tools string       deny a server tool (repeatable)
      --enabled-tools string        allow only these server tools (repeatable)
      --oauth bool                  enable OAuth 2.1 flow (HTTP only)
      --oauth-client-id string      pre-registered OAuth client ID
      --oauth-client-secret string  pre-registered OAuth client secret
      --oauth-callback-port int     fixed localhost port for the OAuth callback
```

```bash
mcp add github --type http \
  --url "https://api.githubcopilot.com/mcp/" \
  --header Authorization "Bearer $GH_PAT"
```

As with providers, a header whose value resolves to the empty string is
dropped from the outgoing request.

#### `mcp remove`

Remove an MCP server.

```text
Usage:
  mcp remove <name>
  mcp rm <name>
```

### lsp

Manage language servers.

```text
Usage:
  lsp [command]

Available Commands:
  add       Add or update a language server
  remove    Remove a language server
  rm        Alias for remove
```

#### `lsp add`

Add a language server, or update an existing server with the same name.

```text
Usage:
  lsp add <name> --command <command> [flags]

Flags:
      --args string              command argument (repeatable)
      --env key value            environment variable (repeatable)
      --filetypes string         file type to attach to (repeatable)
      --root-markers string      root marker file (repeatable)
      --timeout int              startup timeout in seconds
      --disabled bool            disable without removing
      --init-options JSON        initialization options
      --options JSON             server settings
```

```bash
lsp add go --command gopls --env GOPATH "$HOME/go"
```

#### `lsp remove`

Remove a language server.

```text
Usage:
  lsp remove <name>
  lsp rm <name>
```

### hook

Manage hooks. See the [hooks docs](../hooks/) for what they can do and how
they run.

```text
Usage:
  hook [command]

Available Commands:
  add       Add a hook to an event
  remove    Remove a named hook, or clear an event
  rm        Alias for remove
```

#### `hook add`

Add a shell command that runs when the given hook event fires.

```text
Usage:
  hook add <event> --command <command> [flags]

Flags:
      --command string           shell command to run (required)
      --name string              name used for later removal
      --matcher string           regex tested against the tool name
      --timeout int              timeout in seconds (default 30)
```

```bash
hook add PreToolUse --matcher "^bash$" \
  --command "./hooks/no-haskell.sh" --name no-haskell
```

#### `hook remove`

Remove hooks from an event. Without `--name`, remove every hook for the event.

```text
Usage:
  hook remove <event> [--name <name>]
  hook rm <event> [--name <name>]

Flags:
      --name string              remove hooks with this name
```

### machine

Declare other computers on your network that serve models, and how many
agents each can run at once. Ta-Da tells the AI about them and hands them
side work. See [Machines](../../README.md#machines) for how they're used.

```text
Usage:
  machine [command]

Available Commands:
  add       Add or update a machine
  remove    Remove a machine
  rm        Alias for remove
```

#### `machine add`

Add a machine. Calling it again with the same name updates that machine.
Leave out `--model` and Ta-Da uses the first model the server lists.

```text
Usage:
  machine add <name> --url <url> [flags]

Flags:
      --url string               OpenAI-compatible base URL (required)
      --type string              openai-compat, ollama, lmstudio, llamacpp,
                                 omlx or litellm (default openai-compat)
      --model string             model the machine's agents run
      --slots int                agents it runs at once (default 1)
      --hardware string          what it is, e.g. "M1 Max, 32 GB"
      --api-key string           API key, if the server needs one
      --context-window int       context window when the server doesn't say
      --tag string               what it's good at (repeatable)
      --notes string             anything else the AI should know
      --disabled bool            keep it listed but don't use it
```

```bash
machine add m1 --url "http://192.168.1.20:11434/v1" --type ollama \
  --model qwen2.5-coder:14b --slots 4 --hardware "M1 Max"
machine add pc --url "http://192.168.1.30:11434/v1" --type ollama \
  --slots 2 --hardware "RTX 3060 Ti" --tag tests --tag docs
```

#### `machine remove`

Remove a machine.

```text
Usage:
  machine remove <name>
  machine rm <name>
```

### permissions

Configure tool permissions. `allow` skips approval prompts; `deny` hides tools
from the agent entirely.

```text
Usage:
  permissions [command]

Available Commands:
  allow             Allow tools without prompting
  allow-in-project  Allow file tools without prompting inside the project
  deny              Hide tools from the agent
```

#### `permissions allow`

Allow one or more tools to run without prompting.

```text
Usage:
  permissions allow <tool> [<tool> ...]
```

#### `permissions allow-in-project`

Let file-writing tools (`edit`, `multiedit`, `write`, `lsp_replace_symbol`)
run without prompting when the file is inside the project. Writes outside the
project still ask. Changes take effect the next time Ta-Da starts.

```text
Usage:
  permissions allow-in-project <tool> [<tool> ...]
```

#### `permissions deny`

Hide one or more tools from the agent so they cannot be called.

```text
Usage:
  permissions deny <tool> [<tool> ...]
```

```bash
permissions allow view ls grep
permissions allow-in-project edit multiedit write
permissions deny bash
```

### option

Configure general Ta-Da behavior, paths, attribution, and the terminal UI.
Boolean values are optional and default to `true`.

```text
Usage:
  option <key> [value]
  option [command]

Available Commands:
  reset     Clear every value from a list option
  ui        Configure terminal UI behavior

Boolean Keys:
  debug                          enable debug logging
  debug-lsp                      enable LSP debug logging
  auto-lsp                       automatically configure language servers
  progress                       show progress indicators
  web-tools                      let the agent use fetch, agentic_fetch,
                                 download and sourcegraph (off by default)
  auto-summarize                 automatically summarize long conversations
  magic-context                  turn on Magic Context (off by default; also
                                 accepts on/off)
  provider-auto-update           deprecated; has no effect
  default-providers              include built-in providers
  attribution-generated-with     add the Generated with Ta-Da line

String Keys:
  data-directory string            directory for project data and state
  initialize-as string             context filename created by tada init
  notifications string             notification style: auto, native, osc, bell,
                                   or disabled
  worktree-dir string              where /worktree puts new worktrees, relative
                                   to the main checkout (default
                                   ../.tada-worktrees/<repo>)
  attribution-trailer-style string attribution trailer: none, co-authored-by,
                                   or assisted-by

Integer Keys:
  request-timeout int              seconds before an LLM request is aborted;
                                   streaming responses are only aborted after
                                   this much inactivity; 0 waits forever
                                   (default 60)
  magic-context-threshold int      context use, in percent from 20 to 90, at
                                   which Magic Context compresses older turns
                                   (default 65)

List Keys:
  context-path string             append a project context path
  global-context-path string      append a global context path
  skill-path string               append a skill directory
  disable-skill string            hide a skill from the agent
```

```bash
option progress false
option skill-path ./skills
option attribution-trailer-style assisted-by
option magic-context on
```

> [!TIP]
> You can also switch Magic Context on or off from the `/context` screen.
> That applies from the next message; a threshold change applies on the
> next start.

#### `option reset`

Clear every value previously added to a list option. Values added after the
reset are kept.

```text
Usage:
  option reset <key>

Available Keys:
  context-path          clear project context paths
  global-context-path   clear global context paths
  skill-path            clear additional skill directories
  disable-skill         clear disabled skill names
```

#### `option ui`

Configure terminal UI presentation and completion-list limits.

```text
Usage:
  option ui <key> <value>

Available Keys:
  compact bool                  use the compact chat layout
  diff auto|unified|split       choose unified or side-by-side diffs; auto
                                (default) goes side by side from 160 columns
  transparent bool              use the terminal background
  mouse bool                    enable terminal mouse capture for clicks,
                                selection, and scrolling in the TUI (default
                                true); disable to let the terminal emulator
                                or tmux handle text selection and copy/paste
  scrollbar string              control chat scrollbar visibility: default,
                                always, or never
  exit-banner default|compact|none
                                control the post-session banner: default shows
                                the Ta-Da logo, compact shows only the resume
                                hint, none hides it entirely
  completions-max-depth int     maximum directory depth shown by completions
  completions-max-items int     maximum items returned to completions
```

```bash
option ui compact true
option ui diff unified
option ui transparent true
option ui mouse false
option ui scrollbar always
option ui exit-banner compact
option ui completions-max-depth 4
option ui completions-max-items 200
```

> [!IMPORTANT]
> These skill paths load by default — you do NOT need `skill-path`
> for them: `.agents/skills`, `.tada/skills`, `.claude/skills`,
> `.cursor/skills`. Claude Code's user skills (`~/.claude/skills`) are not
> loaded by default; opt in with `option skill-path ~/.claude/skills`.

> [!NOTE]
> The command palette's "Disable Background Color" and "Disable Mouse" 
> toggles always write to the global config. If a project config
> also sets `transparent` or `mouse`, project settings win on the next
> launch (see [Where config lives](#where-config-lives)), so the toggle can
> look like it silently reverted.

## Composing configs

Because it's Bash, a shared base config is just a `source`:

```bash
# Unix-like: ~/.config/tada/tadarc
# Windows:   %USERPROFILE%\.config\tada\tadarc
source ~/team/tada-base.sh    # sets up providers, a few skills

# …but on this machine, drop a skill path the base added and add my own.
option reset skill-path
option skill-path ~/my/skills
```

`remove`, `rm`, and `option reset` all act on whatever was set earlier in the
script or pulled in via `source`. Later lines win, just like a shell.

## Legacy JSON

`tada.json` is the original format and is now deprecated. We plan to support
it for the forseeable future, but new configuration options will only be added
to Bash-based config.

```jsonc
{
  "$schema": "https://tada.cetla.dev.json",
  "providers": {
    "anthropic": { "api_key": "$ANTHROPIC_API_KEY" },
  },
  "models": {
    "large": { "provider": "anthropic", "model": "claude-sonnet-4-20250514" },
  },
  "permissions": { "allowed_tools": ["view", "ls", "grep"] },
}
```

For a full reference, See the [JSON schema](../../schema.json).

In JSON, only selected string fields (API keys, URLs, MCP/LSP commands and args,
headers) are shell-expanded at load time. In `tadarc` there's no such list —
it's all just Bash.

Both formats are trusted code: they run with your shell privileges before the UI
appears. Don't launch Ta-Da in a directory whose config you haven't read.

---

## Whatcha think?

We'd love to hear your thoughts on this project. Need help? We gotchu. You can
find us on:

- [Twitter](https://twitter.com/charmcli)
- [Slack](https://charm.land/slack)
- [Discord](https://charm.land/discord)
- [The Fediverse](https://mastodon.social/@charmcli)
- [Bluesky](https://bsky.app/profile/charm.land)

---

Part of [Charm](https://charm.land).

<a href="https://charm.land/"><img alt="The Charm logo" width="400" src="https://stuff.charm.sh/charm-banner-softy.jpg" /></a>

<!--prettier-ignore-->
Charm热爱开源 • Charm loves open source
