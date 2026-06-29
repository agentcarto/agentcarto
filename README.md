# AgentCarto

AgentCarto is a Go TUI that browses, searches, and inspects the local sessions of
multiple AI coding agents in one place. It normalizes conversation turns and
rewind/fork branches into a common model and — where the agent supports it — can
resume, fork, and relocate sessions.

## Supported agents

| Agent | Browse | Active detection | Resume | Fork | Relocate |
|---|---:|---:|---:|---:|---:|
| Claude Code | ✓ | ✓ | ✓ | ✓ | ✓ |
| Codex | ✓ | ✓ | ✓ | ✓ | ✓ |
| Grok | ✓ | ✓ | ✓ | ✓ | ✓ |
| GitHub Copilot Chat (VS Code / JetBrains) | ✓ | — | — | — | — |

Copilot Chat data is read-only.

## Architecture

Each agent is a separate plugin **executable**. AgentCarto launches them as
subprocesses and talks to them over
[hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) (net/rpc + gob).
Plugins are isolated, independently buildable, and can be added without rebuilding
the host. Shared types live in the `core` SDK.

| Repo | Module | Builds | Role |
|---|---|---|---|
| `agentcarto` | `github.com/agentcarto/agentcarto` | `agentcarto` | host: TUI, scan, cache, config, plugin launching |
| `core` | `github.com/agentcarto/core` | _(library)_ | SDK: domain, plugin (RPC bridge), scan, conversation, transaction, common |
| `plugin-claude` | `github.com/agentcarto/plugin-claude` | `agentcarto-plugin-claude` | Claude Code |
| `plugin-codex` | `github.com/agentcarto/plugin-codex` | `agentcarto-plugin-codex` | Codex |
| `plugin-grok` | `github.com/agentcarto/plugin-grok` | `agentcarto-plugin-grok` | Grok |
| `plugin-copilot` | `github.com/agentcarto/plugin-copilot` | `agentcarto-plugin-copilot-vc` / `-jb` | Copilot Chat |

Dependencies flow `plugin-* → core ← agentcarto` (no cycle; the host uses plugins as
subprocesses, not Go imports). The host locates each plugin binary via (1)
`plugins[].command` in config, (2) `agentcarto-plugin-<type>` next to the `agentcarto`
binary, or (3) `PATH`. A missing plugin is skipped with a warning; the rest keep working.

## Install

The installer downloads the prebuilt binaries (host + all plugins) for your
machine from the latest release and installs them into one directory — no Go or
git needed (Linux/macOS):

```sh
curl -fsSL https://raw.githubusercontent.com/agentcarto/agentcarto/main/install.sh | sh
```

By default it installs to `~/.local/bin`; override with `PREFIX=/usr/local/bin`.
Then run `agentcarto`. Windows users can download the `.zip` from the
[releases page](https://github.com/agentcarto/agentcarto/releases).

Release binaries are built by CI ([`.github/workflows/release.yml`](./.github/workflows/release.yml))
on every `v*` tag.

## Build & run

Requires Go 1.24+ and a UTF-8/ANSI terminal (Linux, macOS, Windows; amd64/arm64).
CGO is not required; race tests need a C compiler.

Clone the repos side by side, then build the host and all plugin executables into `bin/`:

```sh
make build      # bin/agentcarto + bin/agentcarto-plugin-*
make run        # build and launch the TUI
make check      # build + test across every repo
```

Without `make`, build each binary into the same directory:

```sh
go build -o bin/agentcarto ./cmd/agentcarto
go build -o bin/agentcarto-plugin-claude     ../plugin-claude/cmd/agentcarto-plugin-claude
go build -o bin/agentcarto-plugin-codex      ../plugin-codex/cmd/agentcarto-plugin-codex
go build -o bin/agentcarto-plugin-grok       ../plugin-grok/cmd/agentcarto-plugin-grok
go build -o bin/agentcarto-plugin-copilot-vc ../plugin-copilot/cmd/agentcarto-plugin-copilot-vc
go build -o bin/agentcarto-plugin-copilot-jb ../plugin-copilot/cmd/agentcarto-plugin-copilot-jb
```

A Go workspace (`go work init ./agentcarto ./core ./plugin-*`) is handy for
cross-module development but optional.

## CLI

```text
agentcarto                  launch the TUI
agentcarto list             list sessions
agentcarto active           list running sessions
agentcarto config validate  validate config and list enabled plugins
agentcarto config print     print the merged config
agentcarto plugins list     list plugins and capabilities
agentcarto doctor           diagnose config, executables, and storage
agentcarto cache stats|clear
```

Global flags go before the subcommand, e.g. `agentcarto --config ./config.yaml list`
or `agentcarto --no-cache list`.

## TUI

- **List** — `j`/`k` move, `g`/`G` top/bottom, `Enter` open, `/` search, `v` switch
  time/project view, `a` toggle active filter, `o` resume, `m` relocate, `q` quit.
- **Detail** — `j`/`k` select turn, `Enter`/`l` expand/collapse, `f` fork from a turn,
  `q`/`h` back.

Status markers: `ASK`, `TOOL`, `THINK`, `RUN`, `READY`, `OTHER`.

## Configuration

Config is merged in this order (later wins): built-in defaults → `config.yaml` next to
the executable → OS user config → `--config` file. Missing sources are skipped.

| Location | Path |
|---|---|
| Next to the executable | `<dir of agentcarto>/config.yaml` |
| User (Linux) | `$XDG_CONFIG_HOME/agentcarto/config.yaml` or `~/.config/agentcarto/config.yaml` |
| User (macOS) | `~/Library/Application Support/agentcarto/config.yaml` |
| User (Windows) | `%AppData%\agentcarto\config.yaml` |

See [`config.example.yaml`](./config.example.yaml) for a ready-to-use file. Validate it
with `agentcarto config validate`. `${VAR}` and a leading `~` are expanded; unknown
fields and undefined variables are errors.

## Safety

Browsing, search, and active detection are read-only. Writes happen only on
user-confirmed fork and relocate:

- Fork creates a new session without modifying the original.
- Relocate validates a plan first and uses atomic temp-file replacement.
- Resuming or relocating a running session is refused.
- Writes outside a plugin's declared storage are refused; Copilot's IDE files are never written.

Cached list metadata, the full-text index, and opened conversations are stored in a
local SQLite database; conversation data is never sent over the network. `--no-cache`
disables the cache for one run.

## Development

```sh
make check                          # build + test every repo
go vet ./...
CGO_ENABLED=1 go test -race ./...
```
