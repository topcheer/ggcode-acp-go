# ggcode-acp-go architecture

`ggcode-acp-go` is no longer just a thin ACP transport/client extraction.

It now has two stacked layers:

1. a reusable ACP library layer
2. a product/runtime layer that provides acpx-style durable sessions, history, queueing, config, and CLI flows

## High-level layout

```text
                    acp-go CLI / host integration
                              |
          +-------------------+-------------------+
          |                                       |
          v                                       v
  RuntimeManager                         Config / Flow runner
  session lifecycle                      command defaults
  history + exports                      JSON flow execution
  queue-aware prompt execution
          |
          v
  +-------------------+      +-------------------+      +-------------------+
  |   SessionStore    |      |   HistoryStore    |      |    QueueStore     |
  | sessions/*.json   |      | history/*.ndjson  |      | queue/**/*.json   |
  +-------------------+      +-------------------+      +-------------------+
          |
          v
     AgentRegistry  ->  Client  ->  Transport(JSON-RPC over stdio)  ->  ACP CLI
```

## Layer breakdown

### 1. Agent registry and discovery

`discovery.go` and `registry.go` define:

- built-in launch definitions
- alias resolution
- PATH-based installed-agent discovery
- runtime override injection

The registry uses richer launch metadata than the original extraction pass:

- `command`
- `args`
- `checkBinaries`
- `aliases`
- optional session support hints

That shape is what lets the runtime model both native ACP CLIs and package-exec style launchers.

### 2. ACP client layer

`client.go`, `transport.go`, and `types.go` provide the ACP wire/runtime implementation:

- process startup and teardown
- `initialize`
- `session/new`
- `session/resume`
- `session/list`
- `session/set_mode`
- `session/set_config`
- `session/prompt`
- `session/cancel`
- `session/close`

The client layer remains reusable on its own for hosts that only want ACP transport + prompt streaming.

### 3. Product runtime layer

`manager.go` is the higher-level facade that sits on top of the client.

Responsibilities:

- ensure or discover durable sessions
- map scoped `(agent, cwd, name)` identities to persistent records
- reconnect/resume sessions on demand
- run prompt turns and persist outcome metadata
- close, export, import, prune, and inspect sessions
- expose a normalized runtime status surface

This is the main bridge from “ACP client library” to “headless product/runtime”.

### 4. Durable stores

The runtime persists state through file-backed stores:

- `store.go` → session metadata
- `history.go` → append-only turn/event history
- `queue.go` → queued prompt requests and owner leases
- `config_store.go` → CLI defaults

These stores are intentionally simple JSON/NDJSON files so they are:

- inspectable by users
- easy to back up or export
- easy to extend without adding an embedded database dependency

### 5. Queue owner model

The current queue model is file-backed and process-owned:

- `prompt --wait=false` writes a queued request
- the CLI spawns an `internal-owner` background worker when needed
- the owner acquires a per-session lease
- queued prompts are drained sequentially
- `cancel` marks queued/running requests and forwards a best-effort session cancel

This keeps no-wait execution durable across short-lived CLI invocations without requiring a long-running daemon install step.

### 6. CLI layer

`cmd/acp-go/main.go` exposes the runtime through commands for:

- prompt execution
- one-shot exec
- status/cancel
- session management
- config defaults
- agent listing
- flow execution

The CLI is intentionally thin: command handlers mostly resolve config/flags and then call the runtime layer.

## Data model

### Session record

Each session record keeps:

- logical session key
- record id
- agent + cwd + name
- remote ACP session id
- mode/config metadata
- last prompt / stop reason / summary / error
- queue owner / active request metadata
- timestamps and close state

### History entry

Each history entry stores:

- timestamp
- kind (`prompt`, `text`, `tool_call`, `tool_result`, `message`, ...)
- role
- text/tool fields

### Queue request

Each queued prompt stores:

- request id
- prompt text
- status (`queued`, `running`, `completed`, `failed`, `cancelled`)
- cancel intent
- timestamps
- final text / stop reason / error

## Current protocol projection

The host-facing streamed runtime event model stays intentionally compact:

- `text_delta`
- `tool_call`
- `tool_result`
- `status`

The runtime keeps ACP-specific wire handling inside the client layer and exposes a stable higher-level event projection to callers.

## Why the package is split this way

The earlier extraction only solved the bottom layer.

That was insufficient for parity with a headless ACP runtime because the product features actually depend on:

- durable session identity
- replayable history
- queueing across invocations
- config defaults
- session-scoped controls
- a standalone command surface

The current architecture keeps the reusable ACP mechanics separate while adding those higher-level features as composable layers on top.
