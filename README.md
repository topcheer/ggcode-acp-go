# ggcode-acp-go

`ggcode-acp-go` is a Go implementation of an acpx-style ACP runtime.

It now provides both:

- a reusable Go library for ACP discovery, client lifecycle, runtime/session management, durable history, and queue storage
- a standalone `acp-go` CLI for prompt execution, session inspection, config defaults, flow execution, and no-wait background queue processing

## What it includes

- ACP JSON-RPC 2.0 transport over stdio
- agent discovery/registry with aliases and launch metadata
- ACP client lifecycle, prompt streaming, resume/list/mode/config operations
- durable file-backed session records
- durable per-session history
- a background queue owner for `prompt --wait=false`
- CLI config persistence
- a standalone `acp-go` command
- a simple JSON-based flow runner

## Built-in agent registry

The built-in registry currently knows how to launch:

| Agent | Launch |
| --- | --- |
| `pi` | `npx pi-acp@^0.0.26` |
| `codex` | `npx -y @agentclientprotocol/codex-acp@^0.0.44` |
| `claude` | `npx -y @agentclientprotocol/claude-agent-acp@^0.37.0` |
| `gemini` | `gemini --acp` |
| `cursor` | `agent acp` (fallback: `cursor-agent acp`) |
| `copilot` | `copilot --acp --stdio` |
| `droid` | `droid exec --output-format acp` |
| `fast-agent` | `uvx fast-agent-mcp acp` |
| `kilocode` | `npx -y @kilocode/cli acp` |
| `kimi` | `kimi acp` |
| `kiro` | `kiro-cli acp` (fallback: `kiro-cli-chat acp`) |
| `opencode` | `npx -y opencode-ai acp` |
| `qoder` | `qodercli --acp` |
| `qwen` | `qwen --acp` |
| `trae` | `traecli acp serve` |
| `ggcode` | `ggcode acp` |

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

`prompt --wait=false` persists a queue request and starts a background owner process that drains queued prompts for the session.

## State layout

By default state lives in:

- `$GGCODE_ACP_STATE_DIR`, if set
- otherwise `$XDG_STATE_HOME/ggcode-acp-go`
- otherwise `~/.ggcode-acp-go`

The runtime stores:

- `sessions/*.json` for durable session records
- `history/*.ndjson` for durable turn/event history
- `queue/...` for queued prompt requests and owner lease metadata
- `config.json` for CLI defaults

## Library surface

The library now has two main layers:

1. low-level ACP client/discovery APIs such as `Discover`, `ClientManager`, `Client`, and prompt streaming
2. product-level runtime APIs such as `RuntimeManager`, `SessionStore`, `HistoryStore`, and `QueueStore`

Useful entry points:

- `NewStaticAgentRegistry(...)`
- `NewRuntimeManager(...)`
- `NewFileSessionStore(...)`
- `NewFileHistoryStore(...)`
- `NewFileQueueStore(...)`

## Example: direct prompt

```bash
go run ./cmd/acp-go copilot prompt --text "Summarize this repository"
```

## Example: persistent session

```bash
go run ./cmd/acp-go copilot sessions ensure --name repo
go run ./cmd/acp-go copilot prompt --name repo --text "Create a release checklist"
go run ./cmd/acp-go copilot status --name repo
go run ./cmd/acp-go copilot sessions history --name repo
```

## Example: queued background prompt

```bash
go run ./cmd/acp-go copilot prompt --name repo --wait=false --text "Run the next maintenance task"
go run ./cmd/acp-go copilot status --name repo
go run ./cmd/acp-go copilot cancel --name repo
```

## Example: flow

Flow files are JSON:

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

Run with:

```bash
go run ./cmd/acp-go flow run --file ./flow.json
```

## Development

```bash
test -z "$(gofmt -l .)"
go test ./...
go build ./cmd/acp-go
```

For architecture details, see [docs/architecture.md](docs/architecture.md).
