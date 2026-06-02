# ggcode-acp-go

`ggcode-acp-go` is a **headless ACP runtime in Go**.

Think of it as the missing layer between “I can speak ACP over stdio” and “I can actually run a real multi-session agent product.” It discovers local ACP-capable CLIs, manages their lifecycle, keeps durable session state on disk, streams prompts, queues background work, and ships a standalone `acp-go` CLI on top.

## Why it exists

Plain ACP transport is only the first 20%.

Real hosts also need:

- agent discovery and launch metadata
- session identity that survives process exit
- durable history
- queueing for no-wait prompts
- config defaults
- a CLI/runtime surface that feels product-ready instead of demo-ready

`ggcode-acp-go` packages all of that into one module.

## What you get

| Layer | What it does |
| --- | --- |
| ACP client layer | JSON-RPC transport, process lifecycle, prompt streaming, resume/list/mode/config operations |
| Runtime layer | durable sessions, history, queue ownership, config defaults, exports/imports |
| CLI layer | `prompt`, `exec`, `status`, `cancel`, `sessions`, `config`, `agents`, `flow run` |

Highlights:

- ACP JSON-RPC 2.0 over stdio
- built-in registry with aliases, fallback launch commands, and session hints
- durable file-backed `sessions`, `history`, `queue`, and `config`
- queue owner model for `prompt --wait=false`
- reusable Go library plus standalone `acp-go` binary
- support for non-trivial real-world launch quirks like Droid Factory JSON-RPC and Codex config bridging

## Built-in agent registry

The built-in registry currently knows how to launch:

| Agent | Launch | Notes |
| --- | --- | --- |
| `pi` | `npx pi-acp@^0.0.26` | npm ACP adapter |
| `codex` | `npx -y @agentclientprotocol/codex-acp@^0.0.44` | bridges local Codex config for ACP startup; direct Codex CLI can still use `wire_api="chat"`, but the ACP/app-server bridge path currently cannot |
| `claude` | `npx -y @agentclientprotocol/claude-agent-acp@^0.37.0` | npm ACP adapter |
| `gemini` | `gemini --acp` | native ACP mode |
| `cursor` | `agent acp` | falls back to `cursor-agent acp` |
| `copilot` | `copilot --acp --stdio` | native ACP mode |
| `droid` | `droid exec --input-format stream-jsonrpc --output-format stream-jsonrpc` | Factory protocol bridge; use `GGCODE_ACP_DROID_SETTINGS=/path/to/settings.xxx.json` for explicit custom-model profiles |
| `fast-agent` | `uvx fast-agent-mcp acp` | MCP ACP bridge |
| `kilocode` | `npx -y @kilocode/cli@rc acp` | pinned to `@rc` because stable CLI still lags ACP behavior |
| `kimi` | `kimi acp` | native ACP mode |
| `kiro` | `kiro-cli acp` | falls back to `kiro-cli-chat acp` |
| `opencode` | `npx -y opencode-ai acp` | npm ACP entry |
| `qoder` | `qodercli --acp` | native ACP mode |
| `qwen` | `qwen --acp` | native ACP mode |
| `trae` | `traecli acp serve` | native ACP server mode |
| `ggcode` | `ggcode acp` | self-hosting |

## Quick tour

### One-shot prompt

```bash
go run ./cmd/acp-go copilot prompt --text "Summarize this repository"
```

### Durable named session

```bash
go run ./cmd/acp-go copilot sessions ensure --name repo
go run ./cmd/acp-go copilot prompt --name repo --text "Create a release checklist"
go run ./cmd/acp-go copilot status --name repo
go run ./cmd/acp-go copilot sessions history --name repo
```

### Background queue

```bash
go run ./cmd/acp-go copilot prompt --name repo --wait=false --text "Run the next maintenance task"
go run ./cmd/acp-go copilot status --name repo
go run ./cmd/acp-go copilot cancel --name repo
```

`prompt --wait=false` persists a queue request and starts a background owner process that drains queued work for that session.

### JSON flow runner

```json
{
  "agent": "copilot",
  "name": "release-flow",
  "steps": [
    { "prompt": "Inspect the repository status" },
    { "prompt": "Draft release notes" },
    { "mode": "exec", "prompt": "Summarize open risks in one paragraph" }
  ]
}
```

```bash
go run ./cmd/acp-go flow run --file ./flow.json
```

## State layout

By default state lives in:

- `$GGCODE_ACP_STATE_DIR`, if set
- otherwise `$XDG_STATE_HOME/ggcode-acp-go`
- otherwise `~/.ggcode-acp-go`

The runtime stores:

- `sessions/*.json` for durable session records
- `history/*.ndjson` for append-only turn/event history
- `queue/...` for queued prompt requests and owner lease metadata
- `config.json` for CLI defaults

The files are intentionally plain JSON/NDJSON so they stay easy to inspect, back up, migrate, and debug.

## Library entry points

The library has two main personalities:

1. **ACP plumbing**: discovery, launch, transport, prompt streaming
2. **runtime/product behavior**: durable sessions, history, queueing, config, flows

Useful entry points:

- `NewStaticAgentRegistry(...)`
- `NewRuntimeManager(...)`
- `NewFileSessionStore(...)`
- `NewFileHistoryStore(...)`
- `NewFileQueueStore(...)`

## CLI surface

The standalone binary lives at `cmd/acp-go`.

Current commands:

- `acp-go [agent] prompt`
- `acp-go [agent] exec`
- `acp-go [agent] status`
- `acp-go [agent] cancel`
- `acp-go sessions ensure|list|show|history|export|import|close|prune`
- `acp-go config show|set-default-agent|set-default-session`
- `acp-go agents`
- `acp-go flow run`

## Development

```bash
test -z "$(gofmt -l .)"
go test ./...
go build ./cmd/acp-go
```

For the deeper wiring diagram and design notes, see [docs/architecture.md](docs/architecture.md).
