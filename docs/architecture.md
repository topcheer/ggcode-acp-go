# ggcode-acp-go architecture

`ggcode-acp-go` extracts the **ACP client-side runtime** out of ggcode while leaving ggcode-specific
host/server behavior in the main repository.

## Design goals

1. keep the reusable ACP stdio client logic in a standalone Go module
2. preserve support for the ACP-capable CLIs ggcode already knows how to discover
3. let hosts adapt their own permission/UI abstractions instead of inheriting ggcode internals
4. keep ggcode's ACP server, auth, and session-host behavior local to ggcode

## High-level layout

```text
                    Host application
       (ggcode, another CLI, desktop app, IDE bridge)
                             |
                             | Prompt / PromptStream
                             v
                    +-------------------+
                    |   ClientManager   |
                    |  discovery +      |
                    |  shared settings  |
                    +---------+---------+
                              |
                              v
                    +-------------------+
                    |      Client       |
                    | process lifecycle |
                    | prompt execution  |
                    | event aggregation |
                    +---------+---------+
                              |
                              v
                    +-------------------+
                    |    Transport      |
                    | JSON-RPC over     |
                    | stdio             |
                    +---------+---------+
                              |
                              v
                    ACP-compatible agent CLI
         (copilot / droid / opencode / ggcode acp / ...)
```

## Layers

### 1. Discovery

`discovery.go` contains the known agent table and PATH scanning logic.

Responsibilities:

- define built-in ACP-capable CLI targets
- resolve installed binaries from PATH
- reject unsafe/local workspace-relative binaries
- expose `Discover()` and `DiscoverWithDefs(...)`

This layer is intentionally simple and host-agnostic.

### Supported built-in CLI targets

| Agent name | Binary | Command argv | Kind |
| --- | --- | --- | --- |
| `copilot` | `copilot` | `["agent"]` | native ACP CLI |
| `droid` | `droid` | `["acp"]` | native ACP CLI |
| `opencode` | `opencode` | `["acp"]` | native ACP CLI |
| `ggcode` | `ggcode` | `["acp"]` | ggcode-hosted ACP server |

### 2. Client manager

`client_manager.go` is the shared entry point for most hosts.

Responsibilities:

- hold discovered agents
- carry the shared working directory and permission policy
- create per-agent clients on demand
- apply shared approval handlers and MCP configuration

The manager is stateful enough to carry common settings, but it does not own UI concerns.

### 3. Client runtime

`client.go` is the core runtime.

Responsibilities:

- start the agent process
- initialize ACP
- create/load sessions
- send prompts
- stream prompt updates
- aggregate final prompt results
- surface timeout/process diagnostics
- translate ACP permission requests into host callbacks

This is the main extracted value from ggcode.

### 4. Transport and protocol types

`transport.go` and `types.go` implement the ACP wire-level contract.

Responsibilities:

- JSON-RPC request/response/notification IO
- ACP request/response/update structs
- protocol constants used by both client and host integrations

These are reusable by both the standalone client and any higher-level adapter.

## Protocol boundary

The library currently implements the **client side** of ACP using **JSON-RPC 2.0 over stdio**.

### Protocol surfaces actively used by the runtime

| Surface | Direction | Purpose |
| --- | --- | --- |
| `initialize` | client -> agent | negotiate protocol/capabilities |
| `session/new` | client -> agent | create a new session |
| `session/load` | client -> agent | restore an existing session when available |
| `session/prompt` | client -> agent | execute a prompt |
| `session/cancel` | client -> agent | stop an in-flight prompt |
| `session/close` | client -> agent | close the session during teardown |
| `session/request_permission` | agent -> client | ask the host for approval |
| `session/update` | agent -> client | stream text/tool updates |

### Event projection exposed by the library

Internally ACP may carry richer update payloads, but the exported host-facing projection is:

- `text`
- `tool_call`
- `tool_result`

That is a deliberate design choice: the standalone runtime owns ACP parsing, while the host owns the final UI model.

### 5. Host hooks

The library exposes three small host seams:

- `PermissionPolicy`
- approval callback
- optional logger

This is deliberate. The host should own:

- approval UX
- UI rendering
- tool/event formatting policy beyond the raw streamed event
- how prompt events are persisted or replayed

## Why ggcode still keeps server-side ACP code locally

The ACP server side inside ggcode is not just transport glue. It is tightly coupled to:

- ggcode's tool registry
- ggcode's session model
- ggcode's permission UX
- ggcode's ask_user behavior
- ggcode's session persistence and projection rules

That makes it a poor first extraction target. The client/runtime layer, by contrast, is broadly reusable.

## ggcode integration boundary

Inside ggcode, the dependency split is:

```text
ggcode/internal/acp          -> ACP server / handler / auth / session host
ggcode/internal/acpclient    -> adapter from ggcode-acp-go to ggcode tool interfaces
github.com/topcheer/ggcode-acp-go -> reusable ACP client/runtime/discovery library
```

This keeps the library reusable while avoiding a risky rewrite of ggcode's host-side ACP behavior.

## Event model

The standalone library streams a minimal event model:

- `text`
- `tool_call`
- `tool_result`

That model is intentionally close to ggcode's delegate rendering needs, so hosts can either:

- render those events directly, or
- translate them into a richer local format

## Permission model

The standalone library uses a smaller permission surface than ggcode:

```go
type PermissionPolicy interface {
	Check(toolName string, input json.RawMessage) (Decision, error)
	AllowedPathForTool(toolName, path string) bool
}
```

That keeps the reusable boundary narrow. Richer host policies can be adapted down into this surface.

## Non-goals for v1

Version 1 does not try to:

- extract ggcode's ACP server implementation
- standardize UI rendering across hosts
- reimplement ggcode's tool registry
- own third-party host integration policy beyond basic discovery/runtime hooks

Those can be layered on top once the reusable client/runtime boundary is proven stable.
