# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

AgentCarto is a terminal UI (Bubble Tea) that aggregates the local on-disk sessions of multiple
AI coding agents (Claude Code, Codex, Grok, GitHub Copilot Chat) into one searchable list. It
reads each agent's history from disk, normalizes it into a common turn/branch model, and lets you
browse, search, inspect, resume, fork, and relocate sessions. Browsing is read-only; the only
writes are user-confirmed fork and relocate actions.

## Multi-module layout (important)

The product spans **six sibling directories**, each its own Go module **and its own git repo**:

| Directory | Module | Builds | Role |
|---|---|---|---|
| `agentcarto/` | `github.com/agentcarto/agentcarto` | `agentcarto` | host: TUI, CLI, scan orchestration, cache, config, plugin launching |
| `agentcarto-core/` | `github.com/agentcarto/core` | _(library)_ | shared SDK: `domain`, `plugin` (RPC bridge), `scan`, `conversation`, `transaction`, `common` |
| `plugin-claude/` | `github.com/agentcarto/plugin-claude` | `agentcarto-plugin-claude` | Claude Code agent |
| `plugin-codex/` | `github.com/agentcarto/plugin-codex` | `agentcarto-plugin-codex` | Codex agent |
| `plugin-grok/` | `github.com/agentcarto/plugin-grok` | `agentcarto-plugin-grok` | Grok agent |
| `plugin-copilot/` | `github.com/agentcarto/plugin-copilot` | `agentcarto-plugin-copilot-vc` / `-jb` | Copilot Chat (VS Code / JetBrains) |

A `go.work` in the parent directory unifies all six modules for cross-module development. Dependency
flow is `plugin-* → core ← agentcarto` (no cycles). The host and each plugin are **separate
executables**: the host launches plugins as subprocesses and talks to them over
[hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) (net/rpc + gob). A plugin crash or
missing binary degrades gracefully and never takes down the host.

When you change a `core` type or interface, every plugin and the host can be affected — build/test
all modules (`make check`), not just one.

## Build, run, test

All `make` targets live in `agentcarto/Makefile` and must be run from the `agentcarto/` directory.

```sh
make build        # host binary + all plugin executables + bin/config.yaml into bin/
make run          # build then launch the TUI
make test         # build plugins first (integration tests need them), then go test ./...
make check        # build + test across the host AND all 5 sibling modules
make validate     # build then run `agentcarto config validate`
make doctor       # diagnose config, executables, storage
make clean        # remove built binaries
```

Key gotcha: **tests depend on plugin binaries existing in `bin/`.**
`internal/pluginhost/integration_test.go` launches the real plugin subprocesses and **skips** if a
binary is missing. So `make test` (which runs `make plugins` first) is the correct way to test the
host; a bare `go test ./...` will silently skip the integration test.

Run a single test from a module directory:
```sh
go test ./internal/pluginhost/ -run TestName -v
go test ./internal/search/ -run TestSearch/case_name -v   # subtest
```

No CGO (modernc.org/sqlite is pure Go). Requires Go 1.24+ (workspace declares 1.25.0).

## Architecture

### Host request flow
`cmd/agentcarto/main.go` parses global flags (`--config`, `--no-cache`) and dispatches a subcommand
(`list`, `active`, `config`, `plugins`, `cache`, `doctor`) or, by default, launches the TUI.
Startup: `config.Load` → `pluginhost.Launch` (starts each enabled plugin subprocess, builds a
`plugin.Instance` whose `Impl` is the RPC client) → `app.Build` (wraps instances in a `catalog`).
`internal/app/app.go` is the host-side facade: it routes each operation to the right plugin by
`PluginID` and capability (type-asserting `Impl` to the relevant interface, e.g.
`plugin.ConversationLoader`), returning a read-only `ActionAvailability` reason when unsupported.

### The plugin contract (`core/plugin`)
A plugin implements a set of small optional interfaces — `Scanner`, `ConversationLoader`, `Resumer`,
`Rewinder` (fork), `Relocator`, `ActiveMatcher`, `ExecutableProvider` — and advertises which it
supports via `Descriptor.Capabilities`. Each plugin binary's `main` is one line:
`plugin.Serve(<Agent>Factory{})`. `core/plugin/rpc.go` is the host↔plugin bridge: all `domain` types
are plain value types that transfer over gob, the handshake uses MagicCookie `AGENTCARTO_PLUGIN`,
and there is one plugin type per binary (`PluginSetName = "agent"`).

### Incremental scan (runs plugin-side)
`catalog.Scan` hands each plugin the **entire previous snapshot** (`ScanInput.Warm` + `Dead` negative
cache) by value across the process boundary, rather than doing per-path host callbacks. The plugin
itself decides reuse/skip/re-parse using `core/scan.Cache`, keyed by a fingerprint = hash of
`path:size:mtime` (reproducible in both processes). `ParserVersion` in the `Descriptor` invalidates
old cache when the parser changes — **bump it when you change how a plugin parses sessions.**

### Domain model (`core/domain/model.go`)
`Session` (one conversation, with fork lineage via `ParentSessionID`/`ForkAt`) and a normalized
event stream (`EventKind`: user/assistant/reasoning/tool_call/tool_result/file_change/…). Note the
subtle flags that gate listing/actions: `EmptyFork` (a never-continued full-copy fork, excluded from
the list) and `Unresumable` (e.g. Claude native subagent forks with synthetic IDs — resume is not
offered). Respect these when touching list filtering or the resume/fork paths.

### Mutations are validated, sandboxed, atomic (`core/transaction`)
Fork and relocate produce a `domain.MutationPlan` that `transaction.Validate` checks against
`AllowedRoots` (refuses any path outside a plugin's declared storage) before applying writes/moves
with atomic temp-file replacement. Forks always create a new session and never modify the original.
Resuming/relocating a *running* session is refused.

### Resume/fork handoff replaces the process
Resuming or forking hands off by `syscall.Exec`-replacing the agentcarto process with the agent's CLI
(`internal/platform/handoff*.go`). Because of this, `pluginhost.Hosted.Close` must terminate all
plugin subprocesses **before** the handoff.

### Conversation parsing helpers (`core/conversation`)
Shared, agent-agnostic logic for turn/branch reconstruction. What counts as a genuine prompt or
command is decided **plugin-side at parse time**: each plugin classifies its own wrapper tags
(`<command-name>`, `<system-reminder>`, `<bash-input>`, `<user_query>`, …) and fills the normalized
`Event.Prompt`/`Event.Command` fields, which `branches.go` and `common.Title` consume for turn
boundaries, headlines and titles. If an agent introduces new wrapper tags, extend that plugin's
classifier (`classify.go` / `promptText`) — never core. Changing a classifier changes parse output:
bump the plugin's `ParserVersion`.

### Host-side UI and search (`internal/tui`, `internal/search`)
`internal/tui` is the Bubble Tea program (Elm-style model/update/view) for the list and detail
screens; it calls `internal/app` for every operation and never touches plugins directly. `internal/search`
is the in-process full-text engine over titles, working dirs, agent names, and conversation bodies.
Both are host-only — keep agent-specific logic in plugins, not here.

## Config

Settings merge in order (later wins): built-in defaults → `config.yaml` next to the executable → OS
user-config dir → `--config` file. `config.example.yaml` is the reference. Each plugin's storage
dirs and executable name are configured per-agent. `--no-cache` skips the local SQLite cache
(`internal/cache`). The host resolves each plugin binary via `plugins[].command` in config, then
`agentcarto-plugin-<type>` next to the host binary, then `PATH`.
