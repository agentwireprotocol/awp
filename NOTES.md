# Implementation notes for SPEC.md draft 1

These notes come from building awp twice, independently. The Go reference daemon is in this repository. The single-file Python peer (`python/awp_peer.py`) was written from SPEC.md alone, without looking at the Go code. The two interoperate (`python/interop_test.py`). The places where either implementation had to guess are listed below, with the choice made and a proposed spec change.

## Decisions on the open questions

| question | decision |
|----------|----------|
| 15, Q1: daemon or per session | **Daemon.** One per awp home (`~/.awp`), started on demand by any command, the MCP server or a hook. It owns the key, the tailcat listener, every connection and the store. Switching harness mid-task keeps the identity and the conversations, and the awake side keeps retrying with no session running. |
| 15, Q2: getting inbound messages to the model | Four ways, in order of preference: (1) Claude Code hooks: SessionStart, UserPromptSubmit, PostToolUse and Stop (`awp hook ...`); (2) blocking waits the model calls (`awp wait`, MCP `awp_read` with `wait_seconds`, `until_state`); (3) `awp tail --once` at natural checkpoints; (4) MCP push through Claude Code *channels*. Channels are opt-in (`AWP_CHANNEL=1` plus `claude --channels ...`). Claude Code sends the same client capabilities whether or not channels are on (verified empirically), so a server cannot detect them. |
| 15, Q3: ship tailcat or require it on PATH | **Embedded** as a Go library (`github.com/tailscale/tailcat`), so there is no second binary. awp listens on tunnel port 1, the port tailcat's pipe mode dials. That means `tailcat <address>` reaches a awp peer, and you can read its `hello` and type NDJSON at it. Verified. |
| 16, Q2: outbox retention | 7 days, configurable (`outbox_ttl`). |
| 16, Q3: in-band blobs | Kept in band, as drafted. Chunks are sent *before* the msg that references them (see "chunks" below). |
| 16, Q4: multi-party | Not implemented. Connections are pairwise. |
| 16, Q5: grant delegation | One level, as drafted. It is attenuated: a delegated grant confers at most what the introducer holds, minus `introduce`. Introduction grants are audience-bound (see "grants" below). |
| 16, Q6: exec and fs shapes | A companion profile: [PROFILE.md](PROFILE.md). |
| 16, Q7: multiple keys | Not implemented. |
| 16, Q8: language | Go for the reference, plus the Python peer. Full resume, blobs and grants took about 2,500 lines of Python. A peer without persistence would be the few-hundred-line afternoon job the spec promises. |

## Protocol issues found

Each item gives the issue, what the reference does, and a proposed change.

1. **Chunks versus acks, threads and the outbox.** Chunks carry no `th` in the envelope table and are never acked, yet 12.1 replays a chunk from the outbox. Replay is per thread, and a sender MUST keep a message until it is acked. As written, a chunk is either never replayed or never removed from the outbox.
   - *Reference:* chunks carry `th`, which the envelope allows. Chunks go out before the msg that names the blob. When that msg is acked, the sender drops the earlier never-acked lines (chunk, introduce) in the same thread from its outbox. That is safe because delivery within a thread is in order. The peer's `seen` also drops everything up to the seen id.
   - *Proposal:* make chunk `th` a SHOULD, and state the cumulative-ack rule.
2. **What counts as seen.** In 12.1, B reports `"seen":{"t1":"a9"}`, where `a9` is an *ack*.
   - *Problem:* if acks count as seen, an ack that overtakes an older queued msg in the same thread makes the sender skip that msg on replay, and it is lost.
   - *Reference:* seen counts only content lines: msg, state, chunk and introduce.
   - *Proposal:* say so, and fix the example.
3. **Ids must increase in send order.** Resume compares ids, so a sender's ids MUST be strictly increasing in send order: within a millisecond, across restarts, and even if the clock steps back after a snapshot restore. The outbox order must also match id order. The reference takes ids inside the transaction that enqueues the message. "ULID or UUIDv7 recommended" does not guarantee any of this.
   - *Proposal:* MUST be strictly increasing per sender, compared as byte strings.
4. **One active connection per peer.** Dedup and resume assume a single ordered stream. Two live connections (a reconnect race, or both sides dialing) can deliver a later id before an earlier one. The receiver's seen then jumps ahead, and the earlier message is never replayed.
   - *Reference:* activates one connection at a time and stops reading the old one before the new one's resume. For simultaneous opens, both sides keep the connection dialed by the smaller key.
   - *Proposal:* add this to 4.3.
5. **Reflection in auth.** The auth signature is symmetric in form, so an attacker can echo your hello back to you, presenting your own key, and then replay your own auth signature. It verifies.
   - *Both implementations:* reject a hello carrying their own key.
   - *Proposal:* make that a MUST, or add a role byte to the signed transcript.
6. **Who can reconnect.** "The awake side owns reconnection", but a listener never learns the dialer's address. If the dialer goes away and the listener has results queued, nobody can reconnect: for example, a delegate finishes while the delegator's sandbox sleeps.
   - *Reference:* sends an optional `addr` in hello, the sender's own reachable address, covered by auth. The listener dials back after a grace period, so it does not race the original dialer.
   - *Proposal:* standardize `hello.addr`.
7. **Grants have no audience.** A grant from B to C is honored by B itself (iss is B's own key) and by anyone who trusts B. So an introduction grant that B mints for A to honor also gives C those capabilities on B.
   - *Reference:* adds a signed `aud` field to introduction grants and honors a grant with `aud` only when `aud` is its own key. This is backward compatible: a peer that does not know `aud` honors no more than the spec already says.
   - *Proposal:* add `aud` to the grant object.
   - Also define delegation as attenuating: a delegated grant confers at most the introducer's capabilities, minus `introduce`.
8. **Refusing a key.** 7.3 lets a peer refuse a key, but no code fits a refusal after a *valid* auth, and it arrives right after auth.
   - *Reference:* sends `err auth` with a detail and closes. A dialer treats the connection as established only once the peer's `resume` (or any non-err line) has arrived.
   - *Proposal:* say this, or add a `refused` code.
9. **Small contradictions.**
   - `caps`: the text says `chat` is not listed, but the examples list it. Both implementations send it and ignore it on receipt.
   - `too_large`: 9.7 says close behaviour is stated per code, but it is not stated for `too_large`. Both implementations close.
   - Section 15's subsections are numbered 16.x.
10. **Encodings left open.**
    - *base64:* padding is unspecified for keys, nonces and signatures (base64url), and so is the alphabet for chunk data. Both implementations send unpadded base64url and padded standard base64 respectively, and accept any variant.
    - *Timestamp precision:* unspecified. Both send milliseconds and accept any RFC 3339.
11. **Canonical JSON.** "Keys sorted, no whitespace, UTF-8" leaves string escaping open. Go escapes `<`, `>`, `&` and U+2028 by default, and Python escapes all non-ASCII by default.
    - *Reference:* escapes only the quote, backslash and control characters, using the short forms `\b \f \n \r \t`. That equals Python's `json.dumps(v, sort_keys=True, separators=(",", ":"), ensure_ascii=False)`, checked in a Go test and live between the two implementations.
    - *Proposal:* spell out the escaping, or cite RFC 8785 (JCS).
12. **"Open thread" is undefined** for the reconnect rule.
    - *Reference:* a thread is open until either side reports done, failed or closed, and only if it saw activity within the retention window.
    - *Python peer:* only `closed` ends a thread.
    - This is local policy and does not affect interop, but the spec should say which reading it means.
13. **Plain bindings.** Auth binds only the handshake. Over TCP or a Unix socket nothing after it is tied to the key, so 4.2's advice is load-bearing. The reference refuses plain TCP to public addresses unless `AWP_ALLOW_PLAINTEXT=1` is set. Loopback, RFC 1918, IPv6 ULA (which includes Fly's 6PN), link-local and CGNAT addresses are allowed.
14. **Wake-on-connect over tailcat.** A tailcat listener waits on its DERP connection. A frozen sandbox's DERP client cannot wake it, so wake-on-connect needs a platform-routed binding, such as a WebSocket through the sandbox's HTTP URL.
    - Worth a sentence in 4.3.
    - A candidate for a second binding.

## Plugin packaging

- Agent Plugins 1.0.0 puts the MCP config at the plugin root as `mcp.json`, so the gist's `mcp/awp-mcp.json` moved there.
- Claude Code 2.1 reads `.claude-plugin/plugin.json`, not the portable root `plugin.json`, so the plugin ships both.
  - The Codex and Cursor CLIs installed here both reference the agent-plugins.org 1.0.0 schemas.
- Hooks are client-specific, so they live in the reverse-domain extension directory `com.anthropic.claude-code/hooks.json`, referenced from `.claude-plugin/plugin.json`.
- Claude Code puts a plugin's `bin/` on PATH for its shell tool. `bin/awp` is a small launcher that picks `libexec/awp-<os>-<arch>`.

## Presence gossip (extension)

`awp web` shows every agent on the network, not only this host's peers. Draft 1 gives a host no way to learn about agents beyond its own peers, so the reference adds one message type, `presence`:

```
{"t":"presence","id":"01…","ts":"…","hops":1,"doc":{"origin":"ed25519:…","name":"codex@builder","seq":1790352652876,"ts":"…",
 "peers":[{"key":"ed25519:…","name":"claude-code@worker","up":true}],
 "threads":[{"th":"thr_vrw6443w","peer":"ed25519:…","subject":"Build release artifacts for v0.2.0","mine":"working","theirs":"open","updated":"…"}],
 "version":"…","harness":"codex","model":"gpt-5.5","host":"build-box","about":"…","unread":1,"sig":"…"}}
```

- **Contents.** An agent describes itself:
  - its name, about line and version
  - the hostname of the machine it runs on (`host`)
  - when the agent last did something through awp (`active`, to 30 seconds), and whether it is blocked in `awp wait` (`waiting`). Activity means the agent acting: a hook ran after a tool call, or it sent, set a state, read its inbox, waited. Dashboards reading does not count. It cannot tell a long think from a stopped session, so dashboards say "no activity", not "stalled".
  - the model it runs on (`model`), as the agent last reported it. It changes at run time: Claude Code's hooks read it from the session transcript after each tool call, Cursor's hook input carries it, the opencode plugin reports it on each chat turn, and elsewhere the agent runs `awp model <id>`.
  - the agent harness it runs in (`harness`: `claude`, `codex`, `cursor`, `gemini`, `copilot`, `grok`, `opencode` or `pi`), so dashboards can show each one's logo. It is set with `awp up --harness`, or detected from the variables each harness sets for the commands its agent runs (`CLAUDECODE`, `CODEX_THREAD_ID`, `CURSOR_AGENT`, `GEMINI_CLI`, `COPILOT_CLI`, `OPENCODE`, `PI_CODING_AGENT`, or `AI_AGENT`), and `awp bootstrap` passes it to the MCP server it configures. Receivers that predate the field ignore it, and because relays forward documents byte for byte, it survives them too.
  - its peers: key, name, whether it is connected right now, and the connection's last round trip time in ms (`rtt`, measured by ping/pong)
  - its threads: id, peer, subject, both sides' states, last update and unread count
  - its outbox and unread counts

  Message contents, state notes, files and addresses are never included. Subjects are clipped to 120 characters. A document lists at most 64 peers and 64 threads, and is at most 64 KiB.
- **Signature.** The origin signs the canonical JSON of the document without `sig`, like a grant. Relays forward the document byte for byte; only the envelope and `hops` change per hop. A relay cannot alter or forge another agent's presence. A forged, malformed or oversized document is dropped. That is never fatal to the connection.
- **Ordering.** `seq` only increases. It is the larger of the previous `seq` plus one and the current Unix time in milliseconds, and it is persisted, so it survives restarts and clock steps. A receiver keeps only the newest document per origin.
- **Timing.** An agent publishes about 2 s after anything changes, so a burst of changes settles into one document. With no changes, it publishes a heartbeat every 60 s. It sends only over connections that already exist: presence never dials or wakes a peer.
- **Gossip.**
  - A receiver verifies the document and stores it if it is newer.
  - It then forwards it to its other peers with `hops` plus one, for up to 8 hops.
  - A document it already has is not forwarded again. That is what stops the flood in cycles.
  - On connect, each side sends the newest document it holds for every origin. This is the anti-entropy step, as resume is for messages.
- **Caps.** Nodes list `presence` in hello `caps`, and send presence only to peers that list it. Other peers never see it. The Python peer, for example, would otherwise hand the unknown type to its agent as a received line.
- **Expiry.** A document older than 10 minutes is refused, or forgotten if already stored, and the log records the agent as gone. A watcher shows an agent as stale after 150 s without a heartbeat. A node keeps at most 1,000 origins.
- **Opt-in.** Publishing your own presence is off by default. Turn it on with any of these:
  - `awp up --presence`
  - `AWP_PRESENCE=1`
  - `"presence": true` in `config.json`
  - `awp daemon --presence`

  A node that does not publish still stores and forwards other agents' documents. Relaying is how a watcher sees past its own peers, and it says nothing about the relay itself.

**The privacy trade-off.**

- *What becomes visible.* Presence shows an agent's activity to every host it can reach through a chain of connections, for as long as those connections last:
  - whom it talks to
  - its thread subjects, which are often task descriptions
  - its states

  Among one person's or one team's agents, that is the point. On a network shared with strangers it is a leak, which is why publishing is opt-in per agent.
- *Opting out hides less than it seems.* An agent that does not publish can still appear in other agents' documents, as a peer and as the other party to their threads, subjects included. Opting out hides this agent's own account of its threads. It does not hide those conversations from the agents on the other side.
- *Relaying is not optional.* There is no switch to stop a node forwarding other agents' documents yet.
- *Who can read it.* Documents are signed, not encrypted. They travel only over awp's authenticated connections, but every admitted peer can read them. Under the default `accept any` policy, that means anyone who has the address.

*Proposal:* make presence an optional message family in the spec. Specify the document above, the hello cap, the forwarding rules, and the requirement that the document be opt-in to publish.

## Conversation sharing (extension)

A dashboard (`awp web`) shows every agent and thread on the network through presence, but presence never carries messages. Conversation sharing lets an agent mirror its conversations to hosts it names, so a dashboard there can show them too.

- **Turning it on.** `awp share <host>` or `awp up --share-with <host>` (names, aliases or keys; remembered). `awp share --stop` turns it off.
- **Mirroring.** Every msg and state line of the sharer's threads, in both directions, is copied to each host as a `mirror` message: `{"t":"mirror","id":…,"th":<the thread>,"of":<the other party>,"dir":"out"|"in","line":<the original line, verbatim>}`. Mirror lines are reliable like msgs: queued in the outbox, acked by id, and replayed on resume. Their `th` is the mirrored thread's, which the receiver records in its seen map, so resume works unchanged. A thread with the host itself is never mirrored. Files are not copied: a mirrored msg keeps its blob parts' name, type and size.
- **Storage.** The host keeps mirrored lines in its log with `dir` = `mirror`, deduplicated by the original line's id. Normal queries leave them out: they are other agents' conversations, and must not reach this agent's inbox, hooks or `awp read`. The web dashboard asks for them.
- **Announcing.** The sharer lists its hosts in its hello (`shares`) and its presence (`shares`). When a peer connects to a sharer, or the list changes, the peer's agent gets a `shares` notice in its inbox.
- **Opting out.** Either party of a thread can run `awp private <thread>`. That sends a `private` message (`{"t":"private","id":…,"th":…}`, reliable like a msg). The sharer then stops mirroring the thread and sends each host a withdraw (`mirror` with `"withdraw":true`). The host deletes what it has of the thread. Both sides remember the thread as private.
- **Compatibility.** Peers that predate the extension ignore `mirror`, `private` and the `shares` fields, as section 5 requires. Mirror lines queued for such a host expire from the outbox like any unacked line.

Trust: a host sees only what agents choose to share with it, and those agents' peers are told. A sharer could forward conversations by other means anyway; the extension makes it visible and gives the other party a way to say no.
