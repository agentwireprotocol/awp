# Agent Wire Protocol (AWP)

Status: draft 1, 2026-09-25.

The Agent Wire Protocol (AWP) is a peer-to-peer messaging protocol for coding agents. Two peers, each holding its own Ed25519 keypair, exchange threads of messages as NDJSON over one reliable byte stream, a tailcat tunnel by default. There is no server, no provider and no account. `awp` is the reference implementation: the daemon, CLI and plugin described in section 15.

Draft 1 was first published under the name holler.

## 1. Why

A2A (Google, now Linux Foundation) solves agent interop for enterprises: an agent is an HTTP server behind a URL, described by an Agent Card, protected by OAuth, reached through JSON-RPC, gRPC or REST, with SSE for streaming and webhooks for push. That is the right shape when you have an identity provider, a load balancer and a platform team.

It is the wrong shape for the case we care about: a coding agent on a laptop or a sandbox that needs to talk to another coding agent on another sandbox, right now, for an hour, and then never again. For that case A2A has four problems:

1. It is HTTP and enterprise shaped. You need a stable URL, TLS certs, an auth server and a public route before two agents can say hello.
2. It is client/server, not peer to peer. One side serves, one side calls. Two agents that both want to delegate to each other need two servers and two clients.
3. It has no connectivity story. It assumes reachability. Reachability is the hard part when both ends are behind NAT in ephemeral VMs.
4. It is not cheap, free or easy. Six SDKs and three transport bindings is a lot of surface for "send a message to that agent over there".

AWP takes the opposite bets, borrowed from tailcat:

- The transport solves connectivity. One agent runs `listen`, gets a shareable address, the other runs `connect`. No accounts, no DNS, no certs.
- Once connected, both sides are equal. Either can start a thread, send a message or ask for work.
- Identity is a keypair the agent generated itself. Trust is a signed grant, not a login.
- The wire format is one JSON object per line. You can implement a peer in any language in an afternoon and debug it with `cat`.

## 2. Goals and non-goals

Goals

- Two agents on different networks exchange messages in under a minute of setup.
- Symmetric. There is no server role after the connection is up.
- Conversational. The unit of exchange is a message in a thread, like two people in a channel. Task lifecycle is a convention on top, not a state machine in the protocol.
- Small. A complete peer is a few hundred lines. The spec fits in one file.
- Survives sleep. Sandboxes sleep, laptops close. State is on disk, the awake side reconnects, nothing in flight is lost.
- Secure by construction. Every byte is inside the tailcat tunnel, every peer proves key possession, every capability beyond the default is an explicit signed grant.
- Extensible without version bumps. Unknown message types and unknown fields are ignored.

Non-goals

- Discovery registries, marketplaces, Agent Cards. Addresses move out of band.
- Enterprise identity federation. A bridge to OIDC can be an extension, not the core.
- Multi-party rooms in v0. Connections are pairwise. Fan-out is many pairwise connections.
- Exactly-once delivery. At-least-once with dedup by id is the guarantee. Ordered within a thread.
- Binary efficiency. Large files go in chunks, base64 encoded. If that hurts, add a sidecar transfer later.

## 3. Model

```
  agent A                                      agent B
  ┌────────────┐   tailcat (WireGuard p2p)   ┌────────────┐
  │ keypair A  │◄───────────────────────────►│ keypair B  │
  │ threads    │   NDJSON, both directions   │ threads    │
  └────────────┘                             └────────────┘
```

Terms

- Peer. An agent process holding an Ed25519 keypair. Identified by the public key.
- Address. The tailcat string that reaches a listening peer. Opaque to AWP. Possession of the address gets you as far as the hello handshake and nothing more.
- Connection. One tailcat (or other reliable ordered byte stream) between two peers, carrying NDJSON both ways.
- Thread. A named sequence of messages between the two peers. Threads are the "task" abstraction. A thread has an id, a subject and a soft state.
- Message. One turn in a thread, made of parts.
- Part. Text, code, structured data, or a reference to a blob.
- Grant. A signed statement by one peer that another key may do something.

## 4. Transport

4.1 Primary binding: tailcat

- The listening peer runs `tailcat listen` and obtains an address. It shares the address out of band: CLI flag, environment variable, chat message, a line in a task prompt.
- The connecting peer runs `tailcat connect <address>` and obtains a bidirectional byte stream.
- AWP runs over that stream. It does not care which side listened.

4.2 Other bindings

Any reliable, ordered, bidirectional byte stream is a valid binding: a Unix socket, a TCP connection on a Fly private network (6PN), a WebSocket. These bindings do not get tailcat's encryption for free. Peers MUST still do the hello and auth handshake. Peers SHOULD refuse to run over plaintext TCP across untrusted networks.

4.3 Reconnection and sleep

Connections drop. Sandboxes sleep, laptops close, tailcat links time out. AWP treats this as the normal case, not an error. Resume is mandatory in v0; every peer implements it.

- Each peer persists an outbox of sent messages and a per-thread "last seen" id to disk (`~/.awp/`). Memory-only state is not enough, because the process may be killed with the sandbox.
- Either peer may reconnect using the same keys. Identity is the key, not the connection or the address.
- After the handshake, both sides send `resume` (section 9.5). Each side replays what the other has not seen.
- Threads survive reconnection. A `working` thread on a sleeping sandbox is still `working` when it wakes.

Sleep specifically:

- A sleeping sandbox cannot initiate. The awake side owns reconnection. It retries `connect` with exponential backoff, capped at 60 seconds, for as long as it has unacked messages or open threads for that peer. There is no give-up timeout by default.
- Connecting to a sleeping sandbox is the wake signal, when the platform supports wake-on-connect. The awake side's retries are therefore also the wake mechanism.
- The listener's address MAY change across sleep if tailcat restarts. Peers SHOULD treat the address as a hint and the key as the identity. If a peer learns a new address for a known key, by `introduce` or out of band, it uses it and keeps the old one as fallback.
- Missed pings mark the connection dead, never the thread. Thread state only changes by an explicit `state` message.
- Messages sent while disconnected are queued in the outbox and delivered on resume. Sending never fails because the peer is asleep.

## 5. Framing

- UTF-8. One JSON object per line, terminated by `\n`. No pretty printing.
- Maximum line length: 1 MiB. Larger payloads MUST use `chunk` messages.
- A line that does not parse as a JSON object is a protocol error. The receiver sends `err` with code `bad_frame` and closes.
- Unknown top-level fields MUST be ignored. Unknown message types MUST be ignored, except that a receiver MAY reply `err` with code `unsupported` if the message carried an `id` and looked like a request.

## 6. Envelope

Every line is an object with these fields.

| field | type   | required | meaning |
|-------|--------|----------|---------|
| `t`   | string | yes      | message type |
| `id`  | string | yes      | unique per sender. ULID or UUIDv7 recommended so ids sort by time |
| `ts`  | string | yes      | RFC 3339 UTC timestamp |
| `th`  | string | no       | thread id. Required for `msg`, `state`, `ack` |
| `re`  | string | no       | id of the message this responds to |

Type-specific fields sit at the top level beside these.

## 7. Handshake

Both peers send `hello` immediately on connect, without waiting for the other. Then each proves possession of its key by signing a transcript that includes the other side's nonce. Order on the wire:

```
A → B  hello
B → A  hello
A → B  auth
B → A  auth
A → B  resume
B → A  resume
```

7.1 `hello`

```json
{"t":"hello","id":"01J9...","ts":"2026-09-25T17:03:11Z",
 "v":0,
 "key":"ed25519:base64url(32 bytes)",
 "name":"claude-code@sprite-7f3a",
 "nonce":"base64url(32 random bytes)",
 "caps":["chat","blob","grant"],
 "about":"Coding agent working on repo fly-apps/foo, branch kyle/refactor"}
```

- `v` is the protocol major version. A peer that sees a `v` it does not speak sends `err` code `version` and closes.
- `key` is the long-term public key. Its byte encoding is the peer identity.
- `caps` lists supported message families. `chat` and resume are mandatory and not listed. Everything else is optional.
- `about` is free text for the other agent's context. It is not authenticated until `auth` completes.

7.2 `auth`

```json
{"t":"auth","id":"01J9...","ts":"...",
 "sig":"base64url(ed25519 signature)",
 "grants":[ ...optional grant objects... ]}
```

`sig` is the Ed25519 signature over the byte string:

```
"awp-auth-v0" || 0x00 || my_hello_line || 0x00 || peer_hello_line
```

where the hello lines are the exact bytes received or sent, without the trailing newline. Each side verifies the other's signature with the key from that side's `hello`. If verification fails, send `err` code `auth` and close.

After both `auth` messages verify, the connection is established. Each peer now knows the other's key and self-described name and capabilities.

7.3 What possession of the address buys

Only the ability to reach `hello`. A peer MAY refuse to continue after `hello` if the presented key is not on an allow list and no grant is presented. The default policy for a coding agent listening on a sandbox SHOULD be: accept any key, give it the default capability set described in section 10, log the key.

## 8. Threads

A thread is a conversation about one thing. Creating one is implicit: the first `msg` with a new `th` creates it. The creating message SHOULD carry a `subject`.

8.1 `state`

Threads have a soft state used by convention, not enforced by the protocol.

```json
{"t":"state","id":"...","ts":"...","th":"thr_9k2","state":"working","note":"running tests"}
```

Recommended state vocabulary. Peers MAY use others.

| state          | meaning |
|----------------|---------|
| `open`         | default after creation |
| `working`      | the sender is actively doing something for this thread |
| `waiting`      | the sender needs a reply before continuing |
| `done`         | the sender considers the thread complete |
| `failed`       | the sender gave up, `note` says why |
| `closed`       | no further messages expected from either side |

A `state` message from one side describes that side's view. Two sides can disagree. That is fine.

## 9. Messages

9.1 `msg`

The workhorse. A turn in a thread.

```json
{"t":"msg","id":"01J9...","ts":"...","th":"thr_9k2","re":"01J8...",
 "subject":"Port the auth middleware to the new router",
 "parts":[
   {"k":"text","text":"Here is the diff so far. Can you run the integration suite on your side and tell me what breaks?"},
   {"k":"code","lang":"diff","text":"--- a/auth.go\n+++ b/auth.go\n..."},
   {"k":"data","mime":"application/json","data":{"branch":"kyle/refactor","commit":"a1b2c3"}},
   {"k":"blob","ref":"blob_44","name":"test-output.log","mime":"text/plain","size":183422}
 ]}
```

Part kinds

| `k`    | fields | notes |
|--------|--------|-------|
| `text` | `text` | markdown by convention |
| `code` | `text`, `lang` | fenced code without the fence |
| `data` | `data`, `mime` | inline JSON |
| `blob` | `ref`, `name`, `mime`, `size` | refers to a blob sent via `chunk` before or after this message |

`subject` is only meaningful on the first message of a thread. `re` points at a specific earlier message when the reply is to one message in particular.

9.2 `ack`

```json
{"t":"ack","id":"...","ts":"...","th":"thr_9k2","re":"01J9..."}
```

Mandatory for `msg` and `state`. Says "I have durably received this". Acks drive outbox pruning: a sender MAY drop a message from its outbox once acked, and MUST keep it until then. Acks themselves are not acked.

9.3 `chunk`

Carries part of a blob. Blobs are identified by a sender-chosen `ref`, chunked in order, base64 encoded.

```json
{"t":"chunk","id":"...","ts":"...","ref":"blob_44","n":0,"last":false,"data":"base64..."}
```

- `n` is the chunk index from 0. `last` is true on the final chunk.
- Chunks for one `ref` MUST arrive in order. Chunks for different refs MAY interleave.
- Recommended chunk payload size: 256 KiB before encoding.
- A receiver that does not want a blob sends `err` code `blob_refused` with `ref`. The sender stops.

9.4 `ping` and `pong`

```json
{"t":"ping","id":"...","ts":"..."}
{"t":"pong","id":"...","ts":"...","re":"<ping id>"}
```

Peers SHOULD ping when idle for 30 seconds and treat two missed pongs as a dead connection.

9.5 `resume`

Sent by both sides after every handshake, including the first. Lists, per thread, the last message id the sender has durably received. An empty `seen` on a first connection is normal.

```json
{"t":"resume","id":"...","ts":"...","seen":{"thr_9k2":"01J9...","thr_0aa":"01J7..."}}
```

The receiver re-sends any outbox messages in those threads with ids greater than the seen id, plus every outbox message in threads the other side did not list, in original order, with original ids and timestamps. Replayed messages MUST be acked like new ones. Receivers MUST deduplicate by id, since a message can be replayed after it was received but before its ack got through.

9.6 `bye`

```json
{"t":"bye","id":"...","ts":"...","reason":"done"}
```

Graceful close. After sending `bye` a peer sends nothing else and closes after the other side's `bye` or after 5 seconds.

9.7 `err`

```json
{"t":"err","id":"...","ts":"...","re":"<offending id, optional>","code":"unsupported","detail":"human readable"}
```

Codes: `bad_frame`, `version`, `auth`, `unsupported`, `forbidden`, `blob_refused`, `too_large`, `internal`. Whether an `err` closes the connection is stated per code above. `forbidden`, `unsupported`, `blob_refused` and `internal` do not close.

## 10. Capabilities and grants

10.1 Default capabilities

After a successful handshake, a peer has the default set unless local policy says otherwise:

- send and receive `msg`, `state`, `ack`, `ping`, `pong`, `bye`, `err`
- send blobs up to a local size limit (recommended 50 MiB)

Anything beyond that is named by a capability string and requires a grant. Capability strings are application defined. Suggested initial vocabulary for coding agents:

| capability      | meaning |
|-----------------|---------|
| `exec`          | may ask this peer to run commands, via a `msg` with a `data` part of mime `application/vnd.awp.exec+json` |
| `fs:read`       | may ask for file contents |
| `fs:write`      | may ask for file writes |
| `introduce`     | may hand this peer's address and a derived grant to a third peer |
| `admin`         | may change this peer's policy |

10.2 Grant object

```json
{"iss":"ed25519:...", "sub":"ed25519:...", "caps":["exec","fs:read"],
 "exp":"2026-09-25T20:00:00Z", "nonce":"base64url", "sig":"base64url"}
```

- `iss` is the granting key, `sub` is the receiving key.
- `sig` is `iss`'s signature over the canonical JSON of the object without `sig`. Canonical means keys sorted, no whitespace, UTF-8.
- A peer honors a grant if `iss` is its own key, or `iss` is a key it has been configured to trust, or `iss` itself holds a grant containing `introduce` from a key it trusts (one level of delegation in v0).
- Grants travel in the `auth` message or in a `grant` message later.

10.3 `grant` message

```json
{"t":"grant","id":"...","ts":"...","grant":{ ...grant object... }}
```

10.4 `introduce` message

```json
{"t":"introduce","id":"...","ts":"...","th":"thr_9k2",
 "peer":{"key":"ed25519:...","name":"codex@sprite-11","address":"tailcat:..."},
 "grant":{ ...grant issued by the introducer to the recipient, iss = introducer... }}
```

The recipient may connect to the introduced peer and present the grant. The introduced peer honors it only if it trusts the introducer with `introduce`.

## 11. Conventions for coding agents

These are not protocol, they are how we expect to use it. They are here so two implementations built independently still feel alike.

- Delegation. Open a thread with a `subject` that reads like a task title. First `msg` carries the ask in `text`, the context in `data` (repo, branch, commit, paths). Delegate sends `state: working`, streams progress as `msg` with short `text` parts, attaches results as `code` or `blob`, ends with `state: done` and a final summary `msg`.
- Questions mid-task. Delegate sends `state: waiting` and a `msg` asking. Delegator answers in the same thread. Delegate resumes with `state: working`.
- Many workers. Orchestrator opens one connection per worker, one thread per unit of work. No fan-out primitive in the protocol.
- Human in the loop. An agent that needs a human answer forwards the `msg` to its own user interface and replies when the human does. The protocol does not know.

## 12. Full example

A on a laptop delegates a test run to B on a sprite. B listened; A connected. Lines abbreviated. `>` is A to B, `<` is B to A.

```
> {"t":"hello","id":"a1","ts":"...","v":0,"key":"ed25519:AAA","name":"claude-code@laptop","nonce":"nA","caps":["chat","blob"]}
< {"t":"hello","id":"b1","ts":"...","v":0,"key":"ed25519:BBB","name":"claude-code@sprite-7f3a","nonce":"nB","caps":["chat","blob","grant"]}
> {"t":"auth","id":"a2","ts":"...","sig":"..."}
< {"t":"auth","id":"b2","ts":"...","sig":"..."}
> {"t":"resume","id":"a2r","ts":"...","seen":{}}
< {"t":"resume","id":"b2r","ts":"...","seen":{}}
> {"t":"msg","id":"a3","ts":"...","th":"t1","subject":"Run integration suite on kyle/refactor","parts":[{"k":"text","text":"Please run `make integration` at commit a1b2c3 and send me failures."},{"k":"data","mime":"application/json","data":{"repo":"fly-apps/foo","commit":"a1b2c3"}}]}
< {"t":"ack","id":"b3","ts":"...","th":"t1","re":"a3"}
< {"t":"state","id":"b4","ts":"...","th":"t1","state":"working","note":"cloning"}
< {"t":"msg","id":"b5","ts":"...","th":"t1","parts":[{"k":"text","text":"3 of 42 failing, all in auth_test.go. Log attached."},{"k":"blob","ref":"L1","name":"integration.log","mime":"text/plain","size":91230}]}
< {"t":"chunk","id":"b6","ts":"...","ref":"L1","n":0,"last":true,"data":"..."}
> {"t":"ack","id":"a4","ts":"...","th":"t1","re":"b5"}
< {"t":"state","id":"b7","ts":"...","th":"t1","state":"done"}
> {"t":"ack","id":"a4b","ts":"...","th":"t1","re":"b7"}
> {"t":"msg","id":"a5","ts":"...","th":"t1","parts":[{"k":"text","text":"Thanks. Closing."}]}
< {"t":"ack","id":"b7a","ts":"...","th":"t1","re":"a5"}
> {"t":"state","id":"a6","ts":"...","th":"t1","state":"closed"}
< {"t":"ack","id":"b7b","ts":"...","th":"t1","re":"a6"}
> {"t":"bye","id":"a7","ts":"...","reason":"done"}
< {"t":"bye","id":"b8","ts":"...","reason":"done"}
```

12.1 Sleep and resume

Same pair. B's sprite sleeps while working. A keeps retrying and B wakes on connect. B replays what A never saw.

```
< {"t":"state","id":"b4","ts":"...","th":"t1","state":"working","note":"running suite"}
> {"t":"ack","id":"a9","ts":"...","th":"t1","re":"b4"}
   ... B sleeps. A's pings go unanswered. A marks the connection dead and retries connect with backoff.
   ... B wakes on A's connect. B's outbox on disk still holds b5, b6, b7 which it wrote before sleeping.
> {"t":"hello", ...}   < {"t":"hello", ...}   > {"t":"auth", ...}   < {"t":"auth", ...}
> {"t":"resume","id":"a10","ts":"...","seen":{"t1":"b4"}}
< {"t":"resume","id":"b10","ts":"...","seen":{"t1":"a9"}}
< {"t":"msg","id":"b5", ...original timestamp and content...}
< {"t":"chunk","id":"b6", ...}
< {"t":"state","id":"b7","ts":"...","th":"t1","state":"done"}
> {"t":"ack","id":"a11","ts":"...","th":"t1","re":"b5"}
> {"t":"ack","id":"a12","ts":"...","th":"t1","re":"b7"}
```

## 13. Security considerations

- The address is a bearer secret for reaching `hello`. Treat it like a password: share over a channel you trust, rotate by restarting `listen`.
- Keys are long-lived by default. Agents on ephemeral sandboxes SHOULD generate a fresh key per sandbox and print its fingerprint in their `about`.
- `auth` binds both hello lines, so an attacker cannot replay a hello or swap capabilities after the fact.
- Grants have `exp`. Issuers SHOULD keep them short, an hour is plenty for a task.
- `exec` and `fs:write` are remote code execution. A peer SHOULD require an explicit grant for them even from keys it otherwise trusts, and SHOULD log every such request.
- Over non-tailcat bindings there is no transport encryption. Do not run AWP over plaintext TCP across the internet.
- Prompt injection travels in `text` parts. That is the receiving agent's problem, same as any input. The protocol marks nothing as trusted.

## 14. Comparison to A2A

| | A2A | AWP |
|---|---|---|
| Roles | client and server | symmetric peers |
| Reachability | assumes a URL | tailcat address, no infra |
| Identity | OAuth2, API keys, mTLS | Ed25519 keypair, signed grants |
| Discovery | Agent Card at well-known URL | out-of-band address, in-band hello |
| Unit of work | Task with lifecycle enum | thread of messages, soft state by convention |
| Streaming | SSE or gRPC stream | it is the only mode |
| Push | webhooks | not needed, connection is persistent |
| Disconnect | task lost unless polled by id | mandatory resume, outbox on disk, awake side reconnects |
| Wire | JSON-RPC, gRPC, REST | NDJSON |
| Spec size | large, three bindings | this file |

## 15. Harness integration: the universal plugin

The protocol is harness-neutral. Agents get it through one plugin packaged to the Agent Plugins spec (https://github.com/agentplugins/agent-plugins-spec), so Claude Code, Codex, Cursor and any other client that reads `plugin.json` install the same thing.

16.1 What the plugin contains

```
awp-plugin/
  plugin.json
  skills/
    awp/SKILL.md          when and how to message another agent
  bin/
    awp                   the peer: listen, connect, send, tail
  mcp/
    awp-mcp.json          optional MCP server wrapping the same binary
```

- The binary `awp` is the reference peer. It owns the keypair, runs tailcat, speaks NDJSON, and exposes a tiny local CLI:
  - `awp listen` prints the address, runs until killed, writes inbound messages to stdout as NDJSON and to a local inbox.
  - `awp connect <address>` opens a connection and keeps it in the background.
  - `awp send <peer> [--thread t] <text>` sends a `msg`.
  - `awp tail [--thread t]` streams inbound messages.
  - `awp grant <peer-key> <caps...> --ttl 1h` mints a grant.
- The skill teaches the model the conventions in section 11: open a thread with a task-like subject, watch state, attach results as code or blob. The skill is the only harness-visible surface a model needs; the CLI does the rest.
- The MCP server is for harnesses that prefer tools to shell. It exposes `awp_listen`, `awp_connect`, `awp_send`, `awp_read`, `awp_grant`, backed by the same binary, so behavior is identical either way.

16.2 Why a binary plus a skill, not a per-harness integration

- Harnesses differ in how they surface skills, tools and hooks. A binary with a stable CLI is the lowest common denominator every harness can call.
- Keys, inbox and outbox live in one place per machine (`~/.awp/`), so switching harness mid-task does not lose the connection or the identity.
- The Agent Plugins spec covers skills and MCP servers today. Hooks, if a harness supports them, can auto-run `awp tail` on session start so inbound messages surface without the model polling.

16.3 Open plugin questions

1. Does the peer run as a long-lived daemon that harness sessions attach to, or per session? Daemon fits the "switch harness mid-task" story and is required for the awake side to keep retrying a sleeping peer with no session running.
2. How does an inbound message reach the model's context in a harness with no hook support? Options: the skill tells the model to `awp tail --once` at natural checkpoints, or the MCP server pushes a notification where MCP supports it.
3. Should the plugin ship tailcat, or require it on PATH?

## 16. Open questions

1. Resolved: the protocol is the Agent Wire Protocol (AWP); the reference implementation is `awp`.
2. Resolved: resume is mandatory. Remaining sub-question: how long should the outbox be retained for a peer that never comes back? Proposal: until the thread is `closed` or 7 days.
3. Should blobs be in-band (this draft) or should the protocol point at a second tailcat address for bulk transfer?
4. Multi-party. Is a hub peer that relays between many peers a good enough answer, or do we want native rooms?
5. Is one level of grant delegation enough, or do we want full capability chains (UCAN style)?
6. Do we specify the `exec` and `fs:*` request shapes in this spec or in a companion "coding agent profile"?
7. Should a peer be able to advertise more than one key, for rotation?
8. Resolved: Go. Reference implementation language. It is now also the plugin binary, which strengthens the case for Go: one static binary that bundles or shells out to tailcat. Go matches tailcat and Fly tooling. A single-file Python or TypeScript peer would make the "afternoon" claim concrete.
