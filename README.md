# llm-bridge-codex

Harness bridge for [Codex](https://github.com/openai/codex), translating between the llm-bridge subprocess protocol (NDJSON JSON-RPC on stdin/stdout) and Codex's `app-server` WebSocket JSON-RPC API.

## Architecture

Codex ships an in-process JSON-RPC server (`codex app-server --listen ws://…`). This bridge spawns it (or attaches to an existing one), connects via WebSocket, performs the standard `initialize` / `initialized` handshake, then translates between Codex's notification / request stream and the canonical `msg.Event` shape.

```
llm-bridge (stdin JSON-RPC)
    ↓
llm-bridge-codex
    ├── exec codex app-server --listen ws://127.0.0.1:$CODEX_WS_PORT
    └── WebSocket JSON-RPC (gorilla/websocket)
          • thread/start, thread/resume, thread/fork, thread/compact/start
          • turn/start, turn/interrupt
          • account/login/start, account/read
          • subscribe to item/* notifications, auto-approve all approval requests
    ↓
stdout NDJSON (canonical msg.Event)
```

Multi-turn continuity is delegated to the Codex thread (`b.threadID` retained across turns). Authentication is bootstrapped by reading `~/.codex/auth.json` (`chatgpt` mode) on `start` and forwarding the access token via `account/login/start`; if no auth file is present the bridge continues anyway and Codex falls back to its own ambient credentials.

## Build

This module uses a local `replace` directive for `github.com/kayushkin/llm-bridge`, so both repos must be checked out side-by-side:

```
repos/
├── llm-bridge/
└── llm-bridge-codex/
```

Then:

```bash
go build -o llm-bridge-codex .
```

> **Pre-publish note:** the `replace github.com/kayushkin/llm-bridge => ../llm-bridge` line in `go.mod` must be removed (or moved to a `go.work` file) before tagging a release, otherwise downstream `go get github.com/kayushkin/llm-bridge-codex@v…` will fail.

## Prerequisites

The `codex` CLI must be on `PATH` (or set `CODEX_PATH` to its absolute location). Install via:

```bash
npm install -g @openai/codex
# or follow the platform-specific install documented at https://github.com/openai/codex
```

Tested with `codex-cli 0.120.0`.

## Usage

```bash
# Normal mode — reads JSON-RPC requests from stdin, emits NDJSON events to stdout.
./llm-bridge-codex

# Discover stored sessions on disk.
./llm-bridge-codex -discover
```

Send a JSON-RPC request to start a session and run the first turn:

```json
{"method":"start","params":{"session_id":"sess-1","model":"gpt-5","prompt":"Refactor foo.go to extract helper.","work_dir":"/path/to/repo"}}
```

Subsequent turns:

```json
{"method":"message","params":{"content":"Now add a unit test."}}
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CODEX_PATH` | `codex` | Path to the codex binary. |
| `CODEX_WS_PORT` | `19836` | WebSocket port for the codex `app-server`. |
| `CODEX_MODEL` | — | Default model when `start.model` is not set. |
| `CODEX_WORKDIR` | current dir | Working directory passed to `thread/start.cwd`. |
| `CODEX_APPROVAL_MODE` | `never` | Codex approval policy. Accepted: `never`, `on-request`, `granular`, `untrusted`. |
| `CODEX_SANDBOX` | `workspace-write` | Codex sandbox mode. Accepted: `read-only`, `workspace-write`, `danger-full-access`. |
| `CODEX_EFFORT` | — | Codex reasoning effort, forwarded as `turn/start.effort`. |

All env defaults can be overridden per-session via `start` params (`model`, `work_dir`, `permission_mode`, `sandbox`, `effort`). Setting `auto_approve=true` on `start` forces `approval=never` + `sandbox=workspace-write` regardless of env values.

## Authentication

On `start`, the bridge reads `~/.codex/auth.json`. If it exists and `auth_mode == "chatgpt"`, the access token is forwarded to codex via `account/login/start` and an `account/read` round-trip confirms the session (logged to stderr as `[bridge] authenticated: plan=… auth=… email=…`). If the auth file is missing or malformed, the bridge logs the failure and continues — codex then falls back to whatever credentials are configured server-side.

Credentials are intentionally **not** routed through `aiauth` for this harness — codex manages its own auth surface and bypassing it would lose ChatGPT-Plus session features. A future iteration may unify this; for now the harness is the only one in the ecosystem that reads `~/.codex/auth.json` directly.

## JSON-RPC Methods

| Method | Description |
|--------|-------------|
| `start` | Initialize bridge, spawn / connect to codex `app-server`, optionally fork from a parent thread (`fork=<threadID>`) or resume one (`resume=true`), then run the first turn if `prompt` is set. |
| `message` | Run a follow-up turn on the existing thread with the supplied `content`. |
| `compact` | Trigger `thread/compact/start` for context compaction. |
| `resume` | Send a "Continue where you left off." turn on the existing thread. |
| `set_model` | Update the model used for subsequent turns (in-process; no codex call). |
| `set_permission_mode` | Update the approval policy used for subsequent turns. |
| `control` | Generic dispatch: `subtype` ∈ `set_model`, `set_permission_mode`, `interrupt`. |
| `config:<json>` | Mid-session config (e.g. `{"model":"…","effort":"…"}`). Codex-server-driven. |

### `start` parameters

Generic: `session_id`, `client_id`, `display_name`, `agent_id`, `prompt`, `resume`, `fork`, `work_dir`, `system_prompt`.

Codex-specific (all optional):

| Field | Notes |
|-------|-------|
| `model` | Forwarded as `thread/start.model` and `turn/start.model`. |
| `permission_mode` | Forwarded as `thread/start.approvalPolicy` / `turn/start.approvalPolicy`. |
| `sandbox` | Forwarded as `thread/start.sandbox` and converted to the tagged-enum form for `turn/start.sandboxPolicy`. |
| `effort` | Forwarded as `turn/start.effort`. |
| `auto_approve` | Convenience boolean — sets `approval=never`, `sandbox=workspace-write`. |

## Canonical Events Emitted

`session_state`, `stream`, `thinking`, `tool_call`, `tool_result`, `result`, `error`, `system`, `plan`, `approval`.

Codex notification → canonical event mapping (selected highlights — see `translate.go` for the full set):

| Codex notification | Canonical event |
|--------------------|-----------------|
| `thread/started`, `turn/started` | `session_state(running)` |
| `turn/completed` | `result` (with aggregated text + `TokenUsage`) + `session_state(completed)` |
| `turn/failed` | `error(TURN_FAILED)` + `session_state(error)` |
| `item/agentMessage/delta` (final-answer phase) | `stream(DeltaText)` (also accumulated into the `result.text`) |
| `item/reasoning/textDelta` / `…/summaryTextDelta` | `thinking` |
| `item/commandExecution/started` / `…/completed` | `tool_call(command_execution)` / `tool_result` |
| `item/fileChange/started` / `…/completed` | `tool_call(file_change)` / `tool_result` |
| `item/mcpToolCall/started` / `…/completed` | `tool_call(<server-tool>)` / `tool_result` |
| `item/webSearch/started` / `…/completed` | `tool_call(web_search)` / `tool_result` |
| `thread/tokenUsage/updated` | (no event — buffered, reported on next `result`) |
| `*ApprovalRequest` (command, file change, permissions, applyPatch, exec) | auto-`approval(approve)`, response `{approved:true}` |

All outbound events carry the original Codex notification payload in the `Raw` field for downstream debugging.

## Session Discovery

`./llm-bridge-codex -discover` walks `~/.codex/sessions/<year>/<month>/<day>/rollout-<timestamp>-<uuid>.jsonl` and emits a JSON array of `msg.StoredSession` entries (id, prompt snippet, project cwd, turn count, timestamps, file path). The session id is taken from the in-file `session_meta` payload, falling back to the trailing UUID in the filename. Used by `llm-bridge-server` to populate the "discoverable sessions" list on the bridge UI.

## Interrupt and Shutdown

- `SIGINT` to the bridge → `turn/interrupt` on the active thread, then emit `session_state(idle)`.
- `SIGTERM` to the bridge → graceful shutdown (close WebSocket, leave the codex `app-server` process running for the next session).
- stdin EOF → `turn/interrupt` on shutdown if a thread is active.

## Testing

```bash
go build ./...
```

There are no unit tests in this module. The harness is exercised end-to-end via the bridge-ui Conformance page (POST `http://localhost:8160/conformance/run`) and by spinning up a real session through `llm-bridge-server`.

## Known Gaps

- **Hardcoded 30-minute turn timeout** (`startTurn` in `handler.go`). Long-running codex turns will be cut short.
- **`HandleResume` is best-effort**: if `threadID` is set, it sends "Continue where you left off." as a turn; it does not call `thread/resume`. The dedicated `HandleResumeThread` does call `thread/resume` but is only reachable via the `start.resume=true` path on a fresh process.
- **`set_model` / `set_permission_mode` / `effort` updates are in-process only**: they take effect on the next `turn/start`, but no notification is sent to codex itself.
- **Auth is chatgpt-only**: `initAuth` only forwards `auth_mode == "chatgpt"` tokens. API-key flows or other auth modes are ignored — codex falls back to ambient credentials.
- **Single thread per process**: `b.threadID` is a single field. Resetting per `start` works, but parallel threads on one bridge process do not.
- **`discover` shape is brittle**: see `discover.go` — assumes Codex's on-disk layout (`~/.codex/sessions/Y/M/D/rollout-…jsonl`). If Codex changes that, discovery silently returns nothing.

## Part of the llm-bridge ecosystem

- [llm-bridge](https://github.com/kayushkin/llm-bridge) — canonical message types (`msg/`) and bridge interfaces.
- [llm-bridge-server](https://github.com/kayushkin/llm-bridge-server) — central HTTP gateway and session server that launches harness binaries like this one.
- [llm-bridge-claudecode](https://github.com/kayushkin/llm-bridge-claudecode), [llm-bridge-openclaw](https://github.com/kayushkin/llm-bridge-openclaw), [llm-bridge-aider](https://github.com/kayushkin/llm-bridge-aider) — sibling harness bridges for other agents.

## License

Apache 2.0. See [LICENSE](./LICENSE).
