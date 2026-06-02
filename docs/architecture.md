# ggcode-acp-go architecture

`ggcode-acp-go` started as “ACP transport in Go,” but that description is too small now.

The package is really a **two-layer machine**:

1. a reusable ACP library layer for discovery, launch, transport, and prompt streaming
2. a product/runtime layer that turns those raw ACP mechanics into durable sessions, replayable history, background queueing, config defaults, and a practical CLI

The mental model is simple:

- the **client layer** talks to ACP-speaking CLIs
- the **runtime layer** remembers what happened between invocations
- the **CLI layer** makes that state operational

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

That shape is what lets the runtime model both native ACP CLIs and trickier launcher styles such as package-exec wrappers, fallback binaries, and protocol-specific bridges.

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

This layer is still reusable on its own if all you want is “launch an ACP agent, stream a prompt, collect results.”

### 3. Product runtime layer

`manager.go` is the higher-level facade that sits on top of the client.

Responsibilities:

- ensure or discover durable sessions
- map scoped `(agent, cwd, name)` identities to persistent records
- reconnect/resume sessions on demand
- run prompt turns and persist outcome metadata
- close, export, import, prune, and inspect sessions
- expose a normalized runtime status surface

This is the bridge from “protocol client” to “headless product.”

### 4. Durable stores

The runtime persists state through file-backed stores:

- `store.go` → session metadata
- `history.go` → append-only turn/event history
- `queue.go` → queued prompt requests and owner leases
- `config_store.go` → CLI defaults

These stores are intentionally simple JSON/NDJSON files so they are:

- inspectable by users
- easy to back up or export
- easy to extend without immediately reaching for an embedded database

### 5. Queue owner model

The current queue model is file-backed and process-owned:

- `prompt --wait=false` writes a queued request
- the CLI spawns an `internal-owner` background worker when needed
- the owner acquires a per-session lease
- queued prompts are drained sequentially
- `cancel` marks queued/running requests and forwards a best-effort session cancel

This is the key trick that makes no-wait execution feel durable without forcing a daemon install story.

### 6. CLI layer

`cmd/acp-go/main.go` exposes the runtime through commands for:

- prompt execution
- one-shot exec
- status/cancel
- session management
- config defaults
- agent listing
- flow execution

The CLI is intentionally thin: most commands resolve flags/config and hand off to the runtime.

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

That was not enough for parity with a real headless ACP runtime, because the features people actually feel depend on:

- durable session identity
- replayable history
- queueing across invocations
- config defaults
- session-scoped controls
- a standalone command surface

So the current architecture keeps the ACP mechanics reusable, while stacking the product behavior above them instead of mixing everything into one giant client object.
