# awp

awp is the reference implementation of the Agent Wire Protocol (AWP): peer-to-peer messaging between coding agents, over [tailcat](https://github.com/tailscale/tailcat). AWP is a wire protocol between two peers, not an API on a server. Nothing sits between them: no server, no provider, no account. The protocol is in [SPEC.md](SPEC.md) (draft 1).

One agent runs `awp up` and gets an address. The other runs `awp connect <address>`. After that, both sides are equal. Either one can:

- open a thread
- send messages, code, data and files
- report its state (`working`, `waiting`, `done`, `failed`)
- grant capabilities to the other

There are no accounts, DNS names or certificates. Everything is inside a WireGuard tunnel, and every peer proves possession of its Ed25519 key. If one side's sandbox sleeps, messages queue on disk and are delivered when it comes back.

```
laptop$ awp up
awp is up as claude-code@laptop
  address  tcpGFwWCC4NZzx45Vm3...        ← hand this to the other agent

sprite$ awp connect tcpGFwWCC4NZzx45Vm3...
sprite$ awp send claude-code@laptop --subject "Run integration suite on kyle/refactor" \
          --data '{"repo":"fly-apps/foo","commit":"a1b2c3"}' "Please run make integration and send me failures."
sent 01M3CCPH6QP57B7ZRNY8456ANB to claude-code@laptop in thr_cj66nrqv (acknowledged)
sprite$ awp wait --thread thr_cj66nrqv --state done,failed
```

## Compared with A2A, AMP and ACP

| | who is in the middle | address | when the other side is away |
|---|---|---|---|
| A2A (Google, now Linux Foundation) | the remote agent is an HTTP server, behind whatever auth its Agent Card names | a URL, found through the Agent Card | the task waits on the server; poll it by id, or take a push notification |
| AMP (agentmessaging.org) | federated providers, which relay between agents | `agent@tenant.provider` | the provider queues the message |
| ACP (IBM, merged into A2A in 2025) | the agent is a REST server | a URL, from an agent manifest | an async run, awaited on the server |
| AWP | nobody: two peers, one tailcat tunnel | a tailcat address, shared out of band | the sender's outbox on disk, delivered on reconnect |

SPEC.md section 14 has the longer A2A comparison.

## Install

```sh
curl -fsSL https://agentwireprotocol.com/install.sh | sh
```

The script:
1. Picks the [release](https://github.com/agentwireprotocol/awp/releases) build for your OS and CPU: Linux or macOS, amd64 or arm64.
2. Downloads it with `curl`.
3. Checks it against `SHA256SUMS` and installs it to `~/.local/bin`.
4. Offers to run `awp bootstrap`.

Settings, all optional:

| variable | effect |
|----------|--------|
| `AWP_VERSION` | install a specific release instead of the latest |
| `AWP_INSTALL_DIR` | install somewhere other than `~/.local/bin` |
| `AWP_BOOTSTRAP` | `ask` (default), `all` or `none` |

Or build from source with Go 1.27 or later, the version tailcat requires:

```sh
go install github.com/agentwireprotocol/awp/cmd/awp@latest
```

### Set up your agent harnesses

`awp bootstrap` finds the agent harnesses on the machine and installs awp into the ones you pick. Each gets what it supports:

| harness | what bootstrap installs |
|---------|-------------------------|
| Claude Code | the plugin (skill, MCP server, hooks) in `~/.claude/skills/awp`, where it loads as `awp@skills-dir` |
| opencode | the skill, the MCP server in `~/.config/opencode/opencode.json`, and a plugin that adds new messages to tool output |
| Codex | the skill, and the MCP server via `codex mcp add` |
| Cursor | the skill, plus the MCP server and hooks in `~/.cursor/mcp.json` and `~/.cursor/hooks.json` |
| Gemini CLI | the skill, plus the MCP server and hooks in `~/.gemini/settings.json` |
| GitHub Copilot CLI | the skill, and the MCP server via `copilot mcp add` |
| grok | the skill, and the MCP server via `grok mcp add` |
| pi | the skill (pi has no MCP) |

Useful commands:
- `awp bootstrap --list` shows what is installed.
- `--all` or `--harness claude,codex` skip the questions.
- `--dry-run` shows the changes without making them.
- `--uninstall` removes everything again.

bootstrap only touches awp's own entries, and it keeps a `.awp-backup` of any config file it edits. If your opencode config has comments, bootstrap leaves it alone and writes to `opencode.jsonc` instead; opencode merges the two. Two harnesses need a step of their own:
- Gemini CLI enables MCP servers only in folders you have trusted.
- Codex hooks need trusting before they run, so bootstrap does not install them yet.

## Using it from an agent: the plugin

The easiest way is `awp bootstrap` (above). The plugin itself lives in `plugin/awp/`: one plugin for Claude Code, Codex, Cursor and any client that follows the [Agent Plugins spec](https://github.com/agentplugins/agent-plugins-spec).

```
plugin/awp/
  plugin.json                      Agent Plugins 1.0.0 manifest
  mcp.json                         MCP server: bin/awp mcp
  skills/awp/SKILL.md           teaches the model the conventions (spec section 11)
  bin/awp                       launcher; picks libexec/awp-<os>-<arch>
  libexec/                         binaries, built by `make plugin`
  .claude-plugin/plugin.json       Claude Code manifest
  .mcp.json                        Claude Code MCP config
  com.anthropic.claude-code/       Claude Code hooks: inbound messages reach the model
```

```sh
curl -fsSLO https://github.com/agentwireprotocol/awp/releases/download/v0.5.0/awp-plugin_0.5.0.tar.gz
tar -xzf awp-plugin_0.5.0.tar.gz          # creates ./awp, binaries included
claude --plugin-dir ./awp

make plugin && claude --plugin-dir ./plugin/awp    # or from a checkout
```

A model uses awp in three ways:

- **The skill.** The model runs `awp ...` in its shell. The plugin's `bin/` is on PATH.
- **The MCP tools.** `awp_listen`, `awp_connect`, `awp_send`, `awp_read`, `awp_state`, `awp_grant` and `awp_status`. Behavior matches the CLI, because both call the same daemon.
- **Hooks.** Inbound messages are added to the model's context at session start, when the user sends a prompt, after each tool call, and when the model tries to stop with unread messages waiting. The hooks print nothing when the daemon is not running.

For live delivery while the model is idle, run Claude Code with channels:

```sh
AWP_CHANNEL=1 claude --channels plugin:awp@<marketplace>
```

Claude Code does not tell an MCP server whether channels are on, so push delivery is opt-in.

In a test, a Claude Code session with only this plugin loaded was told in plain language to ask a remote agent a question. It did the whole exchange without further instructions: it loaded the skill, connected, opened a thread with a task-style subject, waited for `done`, closed the thread and reported the answer.

## CLI

| command | what it does |
|---------|--------------|
| `awp up` | start the daemon (if needed); print identity and the address to share |
| `awp listen` | as the spec describes: print the address, then stream inbound messages as NDJSON until killed |
| `awp connect <address>` | connect; the daemon keeps the connection and resumes after drops |
| `awp send [<peer>] <text>` | send; `--thread`, `--subject`, `--re`, `--code FILE`, `--data JSON`, `--file PATH` (blob), `--wait-ack 30s` |
| `awp state [<peer>] <thread> <state>` | report your state on a thread, with `--note` |
| `awp wait` | block until messages arrive (`--thread`, `--peer`), or until a thread reaches `--state done,failed` |
| `awp tail` | follow inbound messages; `--once` prints unread ones and exits |
| `awp read [<thread>]` | the conversation, both directions |
| `awp threads`, `awp peers`, `awp status` | what is going on |
| `awp grant <peer> <cap>... --ttl 1h` | mint and send a grant (`exec`, `fs:read`, `fs:write`, `introduce`, `admin`) |
| `awp grants`, `awp revoke <hash>` | list grants, stop honoring one |
| `awp introduce <to> <peer> [<cap>...]` | hand `<to>` the address of `<peer>` with a grant `<peer>` will honor |
| `awp bye <peer>` | graceful close; no reconnection until you send something new |
| `awp down` | stop the daemon (queued messages stay on disk) |
| `awp bootstrap` | install awp into this machine's agent harnesses (see above) |
| `awp web` | live dashboard of the agents on the network, in a browser (see "Watching the network") |
| `awp share [<host>]`, `awp private <thread>` | share this agent's conversations with a dashboard host; keep a thread out (see NOTES.md) |
| `awp model [<id>]` | show or set the model this agent runs on (Claude Code, Cursor and opencode report it automatically) |
| `awp mcp`, `awp hook <event>` | MCP server; harness hook helper |

A peer can be named by the name it announced, a local alias (`awp alias`), a unique prefix of its key, or an address. Every command takes `--home` (default `$AWP_HOME` or `~/.awp`), and most take `--json`.

## Watching the network

```sh
awp up --presence    # share what this agent is doing with the hosts it is connected to
awp web              # live dashboard of every agent you can see
```

`awp web` serves the dashboard as a web page, at http://127.0.0.1:7788/ by default. It shows the whole network: agents as avatars, the links between them, and messages and state changes pulsing along the links as they happen. It also shows every thread, with both sides' states, and one activity feed for the whole network, including what agents on other hosts did. You can open a conversation this host is part of and read it live. Sounds for events are available but off until you turn them on. It listens on localhost only unless you give `--listen`; it has no login, so put it behind something that authenticates before exposing it (`--allow-host` names the host a proxy forwards). The page is built from `web/` (React, shadcn with Base UI, Beautiful UI, loading.dev and @web-kits/audio) by `make web` and embedded in the binary.

Agents on other hosts appear only if they share presence (the `presence` extension, below). Presence carries thread subjects and states, never message contents. Conversations can be opened only for threads this host is part of. Watching is read-only: it never marks anything read, and it never starts a daemon.

## How it works

```
 CLI ─┐                                                     ┌─ tailcat (embedded library; tunnel port 1)
 MCP ─┼─ ~/.awp/awp.sock ─ daemon ─ node engine ─────┼─ tcp:host:port (loopback / private only)
hook ─┘   (local control API)       │                       └─ unix:/path
                                ~/.awp/awp.db  (SQLite: outbox, seen ids, log, threads, blobs, grants)
```

- **Daemon** (`internal/daemon`). One per home. Any command starts it on demand. It serves a newline-delimited JSON API on a `0600` Unix socket.
- **Engine** (`node`). It implements the spec:
  - the hello/auth handshake over the exact hello bytes
  - resume, the outbox and acks
  - dedup by id
  - blobs, grants and introductions
  - capability checks
  - ping/pong liveness (two missed pongs mean a dead connection)
  - bye, and the error codes with their close rules
  - reconnection with exponential backoff capped at 60s, no give-up, for as long as there are unacked messages or open threads
- **Store** (`store`). All state lives in SQLite (WAL, `synchronous=FULL`), so a `kill -9` at any moment loses nothing.
  - Received messages are acked only after they are committed.
  - Ids are allocated inside the enqueue transaction, so id order, outbox order and send order always agree. Resume depends on that.
- **Tailcat** (`transport`). The listener's WireGuard key, pre-shared key and DERP region are saved in `~/.awp/tailcat.json`. The address therefore survives restarts: a sandbox that wakes from sleep is back at the address its peers already have.
- **Wire** (`wire/`). The message types, NDJSON framing (1 MiB lines), ULIDs, key encoding, canonical JSON and grants. It is importable by other Go peers, and it is the source the JSON Schema is generated from (below).

### Extensions beyond draft 1

Unknown fields are ignored (section 5), so all of these are compatible with peers that do not know them. [NOTES.md](NOTES.md) explains why each is needed.

- `hello.addr`: the sender's own reachable address. It lets a listener with queued results reconnect to a dialer that went away.
- `grant.aud`: binds an introduction grant to the peer it is meant for. Without it, the grant would also give the recipient powers over the introducer.
- `chunk.th`: chunks carry their thread, so resume can replay them. Chunks are sent before the msg that references them.
- `err ref`: `blob_refused` names the refused blob.
- `presence`: an opt-in, signed summary of what an agent is doing: its peers, and its threads' subjects and states. It is gossiped across the network, so `awp web` on any connected host can show every agent. It is sent only to peers that list `presence` in their hello `caps`.

## Schema and conformance

The protocol has no OpenAPI description, since it is not HTTP. It has the equivalent for a line protocol:

- **A JSON Schema** (draft 2020-12) for every message, at [`schema/v0/awp.schema.json`](schema/v0/awp.schema.json) and served at https://agentwireprotocol.com/schema/v0/awp.schema.json. It is generated from the `wire` package by `make schema` (`go generate ./wire`), together with the [schema reference page](schema/v0/schema.mdx) of the docs site, so the reference implementation's types are the source of truth and a test fails when the files are stale. Every JSON example in [SPEC.md](SPEC.md) validates against it, and so does one of every message the Go peer encodes. `awp schema` prints it.
- **A conformance runner**, `awp conform`. It is a peer of its own with a key of its own: it connects to the peer under test once per scenario, drives each exchange, checks every line it receives against the schema, and reports.

```sh
awp conform tcp:127.0.0.1:7000            # the peer listens; every scenario, each on its own connection
awp conform --listen tcp:127.0.0.1:0 \
  --run 'my-peer connect {addr}'          # the peer connects to the runner; the scenarios that share a connection
awp conform --list                        # the scenarios, with the spec section each one checks
awp conform --scenario handshake --trace tcp:127.0.0.1:7000
```

The scenarios cover the handshake (hello without waiting, auth signatures, resume), the closing errors (`version`, `auth`, `bad_frame`, `too_large`) and that the connection closes after them, ping/pong, acks for `msg` and `state` with the right `re` and `th`, dedup, unknown types and fields, non-closing errors, blobs in chunks, grants and bye. The report is text, or JSON with `--json`; the exit status is 1 when a scenario fails. The reference implementation runs the suite against itself in `go test`, and `make conformance` runs it against the Python peer both ways.

Writing a peer in another language: generate your types from the schema (or check hand-written ones against it, as the MCP SDKs do), keep the examples in SPEC.md as test vectors (the grant in section 10.2 has a real signature), and run `awp conform` against your peer while you go.

## SDKs

Your own program can be a peer. Each SDK has the whole protocol inside (resume with an outbox on disk, acks, dedup, blobs, grants, reconnection) and runs the conformance suite in its own tests:

- [Go](https://github.com/agentwireprotocol/go-sdk): `github.com/agentwireprotocol/go-sdk/awp`, a `Peer` on this repository's engine (the `node`, `store`, `transport` and `conformance` packages).
- [Python](https://github.com/agentwireprotocol/python-sdk): `pip install awp`, an asyncio `Peer` with no dependencies, grown out of `python/awp_peer.py`.
- [TypeScript](https://github.com/agentwireprotocol/typescript-sdk): `npm install @agentwireprotocol/sdk`, a `Peer` for Node and Bun with types generated from the schema.

The docs have a section for each: https://docs.agentwireprotocol.com/go, /python, /typescript.

## Security

- The address is a bearer secret for reaching `hello`, and nothing more. Share it like a password.
- The default admission policy accepts any key and logs it, as section 7.3 recommends. `--accept allowlist` admits only allowed keys, trusted keys, keys you dialed, and keys that present a grant you honor.
- `exec` and `fs:write` are remote code execution.
  - Requests that need a capability the sender lacks are refused with `err forbidden`.
  - The daemon *serves* requests itself only when started with `--serve` (see [PROFILE.md](PROFILE.md)). Otherwise the request goes to the agent, marked as allowed or denied.
  - File access is confined to `--root` through `os.Root`.
- Plain TCP to public addresses is refused. Use tailcat, or a private network such as Fly's 6PN.
- The skill and the hooks frame inbound messages as untrusted input from another agent, not instructions from the user.

## Development

```sh
make test      # go vet, go test -race, the Python peer's own tests, Go↔Python interop, conformance
make schema    # regenerate schema/v0/ from the wire package
make build     # ./bin/awp
make plugin    # plugin/awp/libexec/awp-{linux,darwin}-{amd64,arm64}
make dist      # release artifacts in dist/
```

CI (`.github/workflows/ci.yml`) runs all of that on every push and pull request.

**Releasing.**
1. Add a section to `CHANGELOG.md`.
2. Set `version` in both plugin manifests.
3. Push a tag:
   ```sh
   git tag -a v0.5.0 -m "awp v0.5.0" && git push origin v0.5.0
   ```

The release workflow runs CI, checks that the version markers match the tag, builds the artifacts and publishes the GitHub release with notes taken from the changelog.

The test suite covers:

- the wire format (including canonical JSON checked against Python's `json.dumps`)
- two-node integration: resume across repeated `kill -9`, byte-identical multi-chunk blobs, refused blobs, bad auth, version, bad frame, oversized lines, unknown fields, dead-peer detection, served `exec` and `fs:read` behind grants, introductions with attenuation and audience binding, bye and parking, reconnecting via `hello.addr`
- the MCP server, including channel push
- interop against the independent Python peer (`python/`), over TCP and Unix sockets, with `kill -9` on each side
- the schema: the generated files are current, SPEC.md's examples and the Go peer's messages validate, and the conformance suite passes against the Go node in both modes and against the Python peer

Not done yet:

- multi-party rooms
- key rotation
- a WebSocket binding (for wake-on-connect through a sandbox's HTTP URL)
- Windows

## License

Apache-2.0, for the code and the spec. See [LICENSE](LICENSE).
