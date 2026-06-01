# ggcode-acp-go

`ggcode-acp-go` is a standalone Go ACP client/runtime/discovery library extracted from ggcode.

It is meant for hosts that want to talk to ACP-compatible coding agents over stdio without
copying ggcode's full TUI, provider stack, or ACP server implementation.

## What it includes

- ACP JSON-RPC transport over stdio
- client lifecycle management for ACP agents
- agent discovery for the currently supported CLI targets
- streaming prompt events for text, tool calls, and tool results
- a small standalone permission policy model for non-ggcode consumers
- a `ggcode` preset so `ggcode acp` is discoverable like other ACP-capable CLIs

## What it does not include

Version 1 intentionally focuses on the **client/runtime** side.

It does **not** include:

- ggcode's ACP server/handler/auth implementation
- ggcode's TUI/mobile/GUI rendering logic
- ggcode's tool registry, provider stack, or session UI

Those stay in ggcode itself. If you want the server-side implementation, use `ggcode acp`.

## Supported discovered CLIs

The built-in discovery table currently includes:

| Agent name | Binary lookup | ACP command | Mode | Notes |
| --- | --- | --- | --- | --- |
| `copilot` | `copilot` | `copilot agent` | native ACP CLI | GitHub Copilot CLI agent mode |
| `droid` | `droid` | `droid acp` | native ACP CLI | Droid ACP entrypoint |
| `opencode` | `opencode` | `opencode acp` | native ACP CLI | OpenCode ACP entrypoint |
| `ggcode` | `ggcode` | `ggcode acp` | ggcode-hosted ACP server | lets hosts talk to ggcode itself as an ACP agent |

Discovery is PATH-based and returns only binaries that are actually installed and executable.
Workspace-local binaries are rejected on purpose so discovery cannot accidentally execute a project-local shim.

## Supported protocol surface

`ggcode-acp-go` currently targets **ACP over JSON-RPC 2.0 on stdio**.

### Transport

| Layer | Supported |
| --- | --- |
| ACP framing | JSON-RPC 2.0 |
| Process transport | stdio |
| Session model | per-agent ACP session |
| Streaming | `session/update` notifications |

### ACP methods/events used by the runtime

| ACP surface | Role in library |
| --- | --- |
| `initialize` | capability / implementation handshake |
| `session/new` | create a fresh ACP session |
| `session/load` | reuse an existing ACP session when supported |
| `session/prompt` | send prompts to the agent |
| `session/cancel` | cancel an in-flight prompt |
| `session/close` | close a session during shutdown |
| `session/request_permission` | bridge agent-side approval requests to the host |
| `session/update` | stream text/tool activity back to the host |

### Streamed event model exposed to hosts

The public event model is intentionally compact:

| Event | Meaning |
| --- | --- |
| `text` | incremental assistant text |
| `tool_call` | tool started / declared |
| `tool_result` | tool completed or failed |

This matches the delegate-oriented rendering model ggcode uses internally while remaining generic enough for other hosts.

## Package shape

The main public surface is intentionally small:

| API | Purpose |
| --- | --- |
| `Discover()` / `DiscoverWithDefs(...)` | find installed ACP agents |
| `NewClientManager(...)` | create a manager with shared working dir/policy |
| `(*ClientManager).Available()` | list discovered agent names |
| `(*ClientManager).Get(ctx, name)` | start and initialize a client |
| `(*Client).Prompt(...)` | run a prompt and collect the final result |
| `(*Client).PromptStream(...)` | stream text/tool events while collecting the final result |
| `SetLogger(...)` | plug in host-side debug logging |
| `NewConfigPolicyWithMode(...)` | create a small standalone permission policy |

For a deeper breakdown of the layers, see [docs/architecture.md](docs/architecture.md).

## Standalone usage

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	acp "github.com/topcheer/ggcode-acp-go"
)

func main() {
	ctx := context.Background()
	workspace, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	policy := acp.NewConfigPolicyWithMode(nil, []string{workspace}, acp.SupervisedMode)
	manager := acp.NewClientManager(workspace, policy)
	defer manager.CloseAll()

	fmt.Println("available:", manager.Available())

	client, err := manager.Get(ctx, "ggcode")
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	result, err := client.PromptStream(ctx, "Summarize this repository", func(event acp.PromptEvent) {
		switch event.Type {
		case acp.PromptEventText:
			fmt.Print(event.Text)
		case acp.PromptEventToolCall:
			fmt.Printf("\n[tool] %s %s\n", event.ToolName, event.ToolArgs)
		case acp.PromptEventToolResult:
			fmt.Printf("\n[result] %s\n", event.Result)
		}
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("\n\nstop=%s\n", result.StopReason)
}
```

## Integrating into another host

Typical host-side wiring looks like this:

1. construct a `PermissionPolicy`
2. create a `ClientManager`
3. optionally call `SetApprovalHandler(...)`
4. get a client by agent name
5. convert `PromptEvent` / `PromptResult` into your host's UI or tool model

If your host already has its own permission type system, the easiest path is to adapt it to:

```go
type PermissionPolicy interface {
	Check(toolName string, input json.RawMessage) (Decision, error)
	AllowedPathForTool(toolName, path string) bool
}
```

That is how ggcode integrates this library internally.

If your host already has a richer protocol boundary, the recommended layering is:

1. keep `ggcode-acp-go` as the ACP transport/runtime layer
2. add a small local adapter for your own permission/event/result types
3. keep host-specific rendering, persistence, and approval UX outside this library

## ggcode integration

ggcode uses this library through a dedicated adapter package instead of importing it directly from
UI/tooling call sites.

The current integration pattern is:

- `ggcode-acp-go` owns client/runtime/discovery
- `ggcode/internal/acpclient` adapts it to `internal/tool.ACPAgentRegistry`
- `ggcode/internal/acp` still owns the ACP server/handler/auth side

That split keeps the reusable ACP client logic independent without forcing ggcode to rewrite its
existing host-side permission and UI systems.

For the ggcode-side details, see:

- `ggcode/docs/acp-go-integration.md`
- `ggcode/internal/acpclient/manager.go`

## Development

```bash
go test ./...
```

## Design notes

- discovery preserves the currently supported ACP-native CLIs and adds `ggcode`
- the exported permission surface is intentionally smaller than ggcode's full policy API
- the library defaults to no-op logging until `SetLogger(...)` is configured
- the public API is designed so hosts can layer their own adapter without forking transport/client logic
