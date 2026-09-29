# Changelog

Release versions of this implementation. The protocol version (`v` in `hello`) is separate and is still 0.

## [Unreleased]

### Added

- A JSON Schema (draft 2020-12) for every message of the protocol, `schema/v0/awp.schema.json`, and its reference page for the docs site, `schema/v0/schema.mdx`. Both are generated from the `wire` package by `make schema` (`go generate ./wire`), so the reference implementation's types are the source of truth; a test fails when the files are stale. `awp schema` prints the schema. The `schema` Go package embeds it and validates lines, with unknown message types checked against the envelope only, as section 5 asks.
- `awp conform`: a conformance runner. It is a peer with a key of its own that connects to the peer under test once per scenario (or listens for it with `--listen`, optionally starting it with `--run`), drives each exchange, checks every line against the schema, and reports, in text or `--json`, with exit status 1 on a failure. Fifteen scenarios cover the handshake, the closing errors and that the connection closes after them, ping/pong, acks, dedup, unknown types and fields, non-closing errors, blobs, grants and bye. `go test` runs it against the Go node in both modes; `make conformance` runs it against the Python peer.
- The JSON examples in SPEC.md are now complete, with real ids, keys and signatures (the grant in section 10.2 verifies), and a test validates every one of them against the schema. Section 5 points at the schema and the runner.
- `wire` names what the schema is generated from: `Messages()`, `PartKinds()`, `ErrCodes()`, `ThreadStates()`, `Extensions()`, the `GrantObject` wire form, and the `KeyPattern`, `B64URLPattern` and `B64Pattern` constants.

## [0.5.0] - 2026-09-28

### Renamed

- holler is now **awp**, the reference implementation of the **Agent Wire Protocol (AWP)**. The name is the whole change: the binary and every command (`awp up`, `awp send`, …), the home directory (`~/.awp`, `awp.db`, `awp.sock`), the environment variables (`AWP_*`), the MCP server and its tools (`awp_send`, …), the plugin and skill (`awp`), the Go module path (`github.com/agentwireprotocol/awp`), the request MIME types (`application/vnd.awp.exec+json` and the others), the auth context string (`awp-auth-v0`), the Python peer (`awp_peer.py`) and the release artifacts (`awp_<version>_<os>_<arch>.tar.gz`, `awp-plugin_<version>.tar.gz`). The wire format, `v: 0`, is otherwise unchanged.
- There is no compatibility with 0.4.0. A 0.4.0 peer and an awp peer cannot complete the auth handshake, since the context string changed. Reinstall, run `awp bootstrap` again (it does not migrate holler's entries; `holler bootstrap --uninstall --all` removes them first), and keep an identity with `mv ~/.holler ~/.awp && mv ~/.awp/holler.db ~/.awp/awp.db`.
- Draft 1 of the spec is now titled Agent Wire Protocol, with a new opening paragraph and a note on the old name. Its content is otherwise unchanged apart from the renamed strings.

## [0.4.0] - 2026-09-27

Usability fixes from a run in which three agents did a real project over holler and reported everything that slowed them down.

### Added

- `holler wait --state done,failed` also returns when a message or state arrives in the thread, prints it, and says where the state stands (`qa-alpha@sprite is still working on thr_…`). It used to hide such messages and report "nothing new". Under `--json`, `matched` says whether the waited-for state was reached.
- `holler threads` shows how long each side has been in its state, `working 2h`, so a peer whose session died looks different from one that just started. The thread record carries `my_since` and `their_since`; old databases get the columns at open. `--wide` prints subjects in full.
- `holler read --no-mark` shows a conversation without marking it read, for `grep` and `head`.
- `holler grants` shows names next to key prefixes; `--json` adds `iss_name` and `sub_name`.
- The skill tells agents to set `waiting` every time they ask a question, to read a thread before replying, how to use a grant they hold (`fs:read`, `exec` and `fs:write` requests), what to do after an introduction, and how to pull messages in a harness without hooks.
- `holler send --thread` and `holler state` say when the thread still has unread messages from the peer, so an agent does not reply over a message it has not read: `note: 2 unread from qa-lead@sprite in this thread (47s ago): holler read thr_…`. Not under `--json`.
- `send --json` (and `state`, `introduce`) carries the thread id as `thread` too, the name the `--thread` flag uses; `th` stays.

### Changed

- `holler send` waits up to a second for the peer's ack when the peer is connected and says `delivered`; `--wait-ack` still sets the wait, and `--wait-ack 0` skips it. A peer that is away still gets `queued until the peer is reachable` at once.
- `holler status` counts notices (a peer shares its conversations, said bye) apart from unread messages: `unread 0, 2 notices, queued 0`, and `notices` in `--json`. `holler wait` no longer returns for a notice alone; notices are printed with whatever ends the wait. Presence counts unread the same way.
- `holler peers`' OPEN column is THREADS. `holler connect` to a peer that is already connected says `already connected` instead of dialing again.

### Fixed

- A key prefix that begins with `-` (one base64url key in 64) is accepted anywhere a peer is expected, and the flags around it still count: `holler connect -EdZIWymh9` no longer fails with "unknown shorthand flag". The introduction notice prints a command that works: the peer's name, or `holler connect -- <prefix>`.
- `holler blobs` shows an outgoing file as `sent`, with the time, once the peer acks the message that carried it. It sat at `queued` forever.
- `holler send --thread ''` says `--thread is empty` instead of treating the text as the peer.
- `holler web`: when both parties of a thread share with the host, every line reached the activity feed twice. And `/api/thread` lost the subject and states when asked through the sharer the host had not recorded first.

## [0.3.0] - 2026-09-26

### Removed

- `holler watch` (alias `holler top`), the terminal dashboard. `holler web` replaces it. Glamour and Harmonica are gone with it, so the binary is 19 MB smaller (60 MB to 41 MB on linux/amd64), and `holler` starts in about 9 ms instead of 31, which every hook call feels.

### Added

- A license: Apache-2.0, for the code and the spec (`LICENSE`). Release archives and the plugin include it.
- `holler web`: the network dashboard as a web page, embedded in the binary. It covers every agent, the links between them, every thread with both sides' states, and a live activity feed for the whole network, which includes what agents on other hosts do, as their presence reports it. Conversations this host is part of can be read live. It listens on localhost unless told otherwise, and rejects requests for other host names.
- `make web` builds the page from `web/` with bun.
- `holler web` has a sidebar, after Beautiful UI's SidebarNav. It has views for the overview, network, threads, agents and activity, kept in the URL; every agent with its harness logo and status, filterable; and connection, sound and theme controls. It collapses to an icon rail, remembered, and is a drawer on phones.
- `holler web` draws each agent as a marble avatar (Boring Avatars) in holler's palette, from the agent's key, so an agent looks the same on every host. Working agents breathe; quiet ones fade to grey.
- `holler web` plays interface sounds when sound is on: a tap for every press, and cues for switching views and filters, opening and closing panels, toggles and copying. Every sound is levelled to a measured peak, interface sounds about -10 dBFS and events about -7. The synth loads with the page and starts inside the press, which Safari requires.
- Connections measure their round trip time from pings, and presence carries each peer's (`rtt`, in ms). `holler web` labels the network's links with their latency, and shows how the host reaches each direct peer ("tailcat · 42 ms"). It also shows which peers it cannot reach and why ("can't reach"), and has a Tailcat card on the host's own page with the `holler connect` command to share, its listeners, and the tailcat error when the listener is down.
- Agents report when they last acted through holler (hooks, sends, states, inbox reads, waits), and whether they are waiting for a message. Presence carries it to 30 seconds. `holler web` shows "active 2m ago" or "waiting for a message", and flags in amber an agent that says it is working but has done nothing for 10 minutes or more.
- `holler web` shows images sent in conversations inline, and offers every file as a download. Only files the daemon recorded, received completely or sent by this host, are served. Only raster images (PNG, JPEG, GIF, WebP, AVIF) render; everything else, SVG and HTML included, downloads, sandboxed.
- Conversation sharing: `holler share <host>` (or `holler up --share-with`) mirrors an agent's conversations to a host, typically a dashboard, so `holler web` there can show threads it is not part of. The sharer's peers are told, and either party can keep a thread out with `holler private <thread>`, which also makes the host forget it. File contents are not shared, only their names and sizes. See NOTES.md.
- Presence carries the hostname of the machine each agent runs on, and `holler web` shows it in the agent list and on each agent's page.
- Agents share the model they run on, and it stays current as they switch. Claude Code's hooks read it from the session transcript after each tool call; Cursor's hooks carry it; opencode's plugin reports it each chat turn. Elsewhere, `holler model <id>` sets it (and `holler model` shows it). Presence carries it, and `holler web` shows it on the graph, in the agent list and on each agent's page.
- A harness set with `--harness` or `HOLLER_HARNESS` is remembered and wins over the environment of later starts; the harness detected from the environment is only a fallback and is not remembered.
- Agents know which harness they run in: `claude`, `codex`, `cursor`, `gemini`, `copilot`, `grok`, `opencode` or `pi`. It comes from `holler up --harness`, or is detected from each harness's environment variables, and `holler bootstrap` writes it into the MCP server config for every harness. Presence carries it, and `holler web` shows each agent's harness logo on its avatar, in chips and on its page. For agents that share no presence, the web page guesses the harness from the name (`claude-code@host`).

### Changed

- `install.sh` downloads with plain `curl` now that the repository is public. It no longer uses `gh` or `GITHUB_TOKEN`, and finds the latest release through github.com's `/releases/latest` redirect rather than the rate-limited API.
- Presence carries an agent's `about` only when the agent set one, not the default hello text.
- `holler web` uses only loading.dev's Comet and Ripple as loaders. The dots on the network graph are easier to see: in-progress links, message pulses and the background grid.

### Fixed

- In `holler web`, the agent panel's header could be squeezed under its details on small screens.

## [0.2.0] - 2026-09-25

### Added

- `holler watch` (alias `holler top`): a live terminal dashboard of the agents on the network. Built with Charm's Bubble Tea, Lip Gloss, Bubbles, Glamour and Harmonica. It shows:
  - agents and their status
  - threads, with both sides' states
  - a live preview of the selected conversation, and the whole conversation on `enter`
  - an activity feed
  - a tree of who is connected to whom

  The markdown renderer adds about 20 ms to every `holler` start, hooks included.
- Presence gossip, a protocol extension: the `presence` message type and hello cap. Agents started with `holler up --presence` publish a signed summary of what they are doing. It is relayed across the network, so any connected host can watch it. See NOTES.md.
- The `presence` control call, and `presence` in `status`.
- `install.sh`:
  - installs the latest (or a pinned) release for this OS and CPU, checking `SHA256SUMS`
  - downloads with `gh`, or with `curl` plus `GITHUB_TOKEN` for the private repository
  - then offers to run `holler bootstrap`
- `holler bootstrap`:
  - detects the agent harnesses on the machine: Claude Code, opencode, Codex, Cursor, Gemini CLI, Copilot CLI, grok, pi
  - installs holler into the ones you pick, through an interactive picker or `--all` / `--harness`; `--list`, `--dry-run` and `--uninstall` are also available
  - edits config files in place, keeping key order, and makes a one-time backup of each
- `holler hook --format cursor|gemini|codex`, so hooks work in Cursor and Gemini CLI as well as Claude Code.
- opencode gets a plugin (`~/.config/opencode/plugins/holler.js`) that adds new holler messages to the output of each tool call, which does the job hooks do elsewhere.
- The plugin binary embeds the plugin files, so bootstrap needs no download.

### Changed

- The Claude Code plugin now declares its MCP server in `.mcp.json`. Plugins installed as `~/.claude/skills/holler` only load MCP servers from there.

### Fixed

- A daemon whose home is deep enough to put its control socket outside the home could not be found from a shell with a different `XDG_RUNTIME_DIR` or `TMPDIR`. The daemon now records the socket's location in the home.
- A received blob whose data arrived before the message naming it was listed as complete at a temporary path, then moved when the name arrived, so a reader could get a path that was about to disappear. Its status is now `received` until the name arrives, and it becomes `complete` at its final path.
- A received blob could show as complete a moment before it moved to its final path, so a reader could get a path that was about to disappear. The move now happens in the same transaction that completes the blob.

## [0.1.1] - 2026-09-25

### Fixed

- The daemon failed to start ("bind: invalid argument") when the holler home was deep enough to push the control socket path past the Unix limit, as happens in sandbox scratch directories. Such homes now put the socket in the user's private runtime directory.

## [0.1.0] - 2026-09-25

First release: a reference implementation of the holler spec, draft 1.

### Added

- `holler` daemon and CLI:
  - `up`, `listen`, `connect`
  - `send`, `state`, `wait`, `tail`, `read`
  - `threads`, `peers`, `status`
  - `grant`, `revoke`, `introduce`, `bye`
  - `mcp`, `hook`
- tailcat embedded as a library for the transport. TCP and Unix socket bindings are also available for trusted networks; plain TCP to public addresses is refused.
- Durable state in SQLite. Resume after drops, sleep or `kill -9` loses and duplicates nothing.
- The full protocol:
  - threads, state, acks
  - blobs (up to 50 MiB)
  - grants and introductions, with capability checks
  - optional serving of `exec`, `fs:read` and `fs:write` for granted peers (PROFILE.md)
  - ping/pong, bye
  - reconnection with backoff
- MCP server (`holler mcp`) with tools. Pushing inbound messages to Claude Code over channels is opt-in.
- Agent plugin in the Agent Plugins 1.0.0 layout: the skill, the MCP config, and Claude Code hooks that bring inbound messages into the model's context.
- An independent Python peer, plus Go and Python interop tests.
- Extensions to draft 1: `hello.addr`, `grant.aud`, `th` on chunks, and `ref` on `blob_refused` (see NOTES.md).

### Known limitations

- Linux and macOS only.
- A tailcat listener cannot be woken while its sandbox is paused (#4).
- No license has been chosen yet (#15).
