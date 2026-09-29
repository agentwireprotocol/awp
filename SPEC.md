# Agent Wire Protocol (AWP)

Status: draft 2, 2026-09-29. Draft 2 replaces draft 1 (2026-09-25) and is not compatible with it. Draft 1 peers and draft 2 peers cannot connect to each other.

The Agent Wire Protocol (AWP) is a peer-to-peer messaging protocol for coding agents. Two peers, each holding its own Ed25519 keypair, exchange threads of messages as NDJSON inside a WireGuard tunnel between those two keys. The tunnel rides on whatever can carry a datagram: a UDP socket, tailcat's relay and NAT traversal, a WebSocket through an HTTP tunnel, a local socket. There is no server, no provider and no account. `awp` is the reference implementation: the daemon, CLI and plugin described in section 18.

Draft 1 was first published under the name holler.

## 1. Why

A2A (Google, now Linux Foundation) solves agent interop for enterprises: an agent is an HTTP server behind a URL, described by an Agent Card, protected by OAuth, reached through JSON-RPC, gRPC or REST, with SSE for streaming and webhooks for push. That is the right shape when you have an identity provider, a load balancer and a platform team.

It is the wrong shape for the case we care about: a coding agent on a laptop or a sandbox that needs to talk to another coding agent on another sandbox, right now, for an hour, and then never again. For that case A2A has four problems:

1. It is HTTP and enterprise shaped. You need a stable URL, TLS certs, an auth server and a public route before two agents can say hello.
2. It is client/server, not peer to peer. One side serves, one side calls. Two agents that both want to delegate to each other need two servers and two clients.
3. It has no connectivity story. It assumes reachability. Reachability is the hard part when both ends are behind NAT in ephemeral VMs.
4. It is not cheap, free or easy. Six SDKs and three transport bindings is a lot of surface for "send a message to that agent over there".

AWP takes the opposite bets, borrowed from WireGuard and tailcat:

- The identity is the connection. A peer's key is its WireGuard key. Whoever is on the other end of the tunnel is, by construction, the key you dialed or the key that dialed you. There is no separate login step.
- Reachability is pluggable and nobody in the middle matters. A relay, a tunnel provider, a NAT traversal service: each moves opaque encrypted packets and learns nothing. One agent runs `listen`, gets a shareable address, the other runs `connect`. No accounts, no DNS, no certs.
- Once connected, both sides are equal. Either can start a thread, send a message or ask for work.
- Trust beyond the default is a signed grant, not a login.
- The wire format is one JSON object per line. You can debug a peer with `cat`.

Draft 1 got the first two by accident: it ran on tailcat, which is WireGuard, but the tunnel keys and the protocol keys were unrelated, and the spec allowed plaintext bindings on the side. Draft 2 makes the tunnel the definition of a connection and removes every plaintext path.

## 2. Goals and non-goals

Goals

- Two agents on different networks exchange messages in under a minute of setup.
- Symmetric. There is no server role after the connection is up.
- Conversational. The unit of exchange is a message in a thread, like two people in a channel. Task lifecycle is a convention on top, not a state machine in the protocol.
- Small. The spec fits in one file. The security layer is WireGuard as published, not a variant.
- Survives sleep and moves. Sandboxes sleep, laptops close, addresses change. State is on disk, the awake side reconnects, a tunnel follows its peer to a new address, nothing in flight is lost.
- Secure by construction. Every byte is inside a WireGuard tunnel between the two identity keys. Every capability beyond the default is an explicit signed grant. Relays and tunnel providers are untrusted by design.
- Carrier-neutral. Adding a way to move packets does not change the protocol.
- Extensible without version bumps. Unknown message types and unknown fields are ignored.

Non-goals

- Discovery registries, marketplaces, Agent Cards. Addresses move out of band.
- Enterprise identity federation. A bridge to OIDC can be an extension, not the core.
- Multi-party rooms. Connections are pairwise. Fan-out is many pairwise connections.
- Exactly-once delivery. At-least-once with dedup by id is the guarantee. Ordered within a thread.
- Binary efficiency. Large files go in chunks, base64 encoded. If that hurts, add a sidecar transfer later.
- Compatibility with draft 1.

## 3. Model

```
  agent A                                                     agent B
  ┌────────────┐                                            ┌────────────┐
  │ key A      │   WireGuard tunnel between key A and key B │ key B      │
  │ threads    │◄──────────────────────────────────────────►│ threads    │
  └────────────┘   NDJSON on a TCP stream inside the tunnel └────────────┘
         │                                                         │
      carrier: udp | tailcat | ws | unix      (moves opaque packets)
```

Terms

- Peer. An agent process holding an Ed25519 keypair. Identified by the public key. The same key, converted, is the peer's WireGuard key (section 4).
- Tunnel. A WireGuard session between two peers' keys. The only thing AWP runs over.
- Carrier. Something that moves WireGuard datagrams between two peers: a UDP socket, tailcat, a WebSocket, a Unix socket. Carriers are interchangeable and untrusted.
- Endpoint. Where a peer can be reached on one carrier: a `host:port`, a DERP region, a WebSocket URL, a socket path.
- Address. A pasteable string holding a peer's key, an admission secret and its endpoints (section 6). Possession of the address gets you as far as the tunnel handshake and `hello`, and nothing more.
- Connection. One tunnel with a TCP stream inside it, carrying NDJSON both ways. There is at most one active connection per pair of keys.
- Thread. A named sequence of messages between the two peers. Threads are the "task" abstraction. A thread has an id, a subject and a soft state.
- Message. One turn in a thread, made of parts.
- Part. Text, code, structured data, or a reference to a blob.
- Grant. A signed statement by one peer that another key may do something.

## 4. Identity

4.1 The key

A peer's identity is an Ed25519 public key, written `ed25519:` followed by the 32 bytes in unpadded base64url:

```
ed25519:LPiUZUt7_kzIUKm5LX4v4RUEXwocMKv1uJ4_sTmx6Zc
```

Grants are signed with it. Peers store, display and compare identities by it. Keys are long-lived by default; an agent on an ephemeral sandbox SHOULD generate a fresh key per sandbox.

4.2 The tunnel key

The same key is the peer's WireGuard static key, by the standard conversion between the twisted Edwards and Montgomery forms of Curve25519 (RFC 7748 section 4.1, as implemented by libsodium's `crypto_sign_ed25519_pk_to_curve25519` and used by age for ssh-ed25519 recipients):

- Public: the X25519 public key is `u = (1 + y) / (1 - y) mod p`, where `y` is the Ed25519 public key's y coordinate.
- Private: the X25519 private key is the first 32 bytes of SHA-512 of the Ed25519 seed, clamped as X25519 requires (clear the low 3 bits of byte 0, clear the high bit and set the second-highest bit of byte 31). This is the scalar Ed25519 itself uses, so the two public keys correspond.

Worked example. The private values are shown because the key is a throwaway made for this document.

```
Ed25519 seed        n91wt2HNOBdumTc7UI0QeqJw8wML6cYWcGzmd0k8Tbo
Ed25519 public      ed25519:LPiUZUt7_kzIUKm5LX4v4RUEXwocMKv1uJ4_sTmx6Zc
X25519 private      AFrT5h44MRTokzB0U6z4Jesw-ISu-CTuXvFnIMZbzUQ
X25519 public       O7KNWfmBVF214-z17qtor-Hl4UWIDOerTw_nad9Vn1Q
tunnel IPv6         fdea:9171:7e2f:c57a:5d92:7d7:169b:67fb
```

Two more, for the other examples in this document:

```
ed25519:RiIietSaPS1BIwoDjJtq_H5OiJO7FzmNRWH62dVzAy4   X25519 ZyiKXtHwO8WII5IaVVZ4lAkdSxMpJbBLj9bgQIMYPyI   fdce:9008:d7a9:cfc8:d5ae:6d27:a044:27b1
ed25519:n9FEURJ_gkrW79iufklyF_R2zafg1by5LBPn729ZbX8   X25519 amAAZlbWvJy9euONBWinCfIafFkUiG80p8Mam8DPIjY   fde2:7d22:f09f:840d:8eee:33d6:ff30:30d4
```

The consequence that matters: an `ed25519:` key alone is enough to build a WireGuard peer for it. A peer that learns a key and an endpoint, by `introduce` or out of band, can dial with full mutual authentication. Nothing else travels with the key.

The conversion drops the sign of the Ed25519 point's x coordinate, so the two Ed25519 keys that differ only in that bit share one tunnel key. Whoever holds the private key of one holds the other's, its negation, so this does not let anyone claim a key they do not hold. Peers compare identities by the full Ed25519 key, and `hello` says which of the two a peer uses (section 10.1).

Using one key for both signing and Diffie-Hellman is a deliberate choice. The combination has a security proof (Thormarker, "On using the same key pair for Ed25519 and an X25519 based KEM", 2021) and years of deployment. A peer MUST NOT use its identity key for any other purpose than AWP grants and AWP tunnels.

4.3 The tunnel address

WireGuard carries IP packets, so each peer has an IPv6 address inside the tunnel, derived from its tunnel key and needing no coordination:

```
tunnel IPv6 = 0xfd || SHA-256("awp-ula-v1" || X25519 public key bytes)[0:15]
```

It is derived from the tunnel key rather than the Ed25519 key because a responder learns only the tunnel key from a handshake.

A peer's WireGuard `AllowedIPs` for a remote key is that key's /128. AWP listens on TCP port 1 of its own tunnel address. Nothing else is routed.

## 5. The tunnel

5.1 WireGuard, unmodified

A connection is a WireGuard session (Donenfeld, "WireGuard: Next Generation Kernel Network Tunnel", 2017; the protocol as specified at wireguard.com/protocol) between the two peers' tunnel keys. Handshake is Noise IKpsk2 over Curve25519, ChaCha20-Poly1305, BLAKE2s. AWP adds nothing to the handshake and changes nothing in the data path: rekeying, replay protection, keepalives, cookies under load and roaming are WireGuard's.

What the handshake establishes, and what AWP relies on:

- The initiator knows the responder's key before sending anything. It got it from the address.
- The responder learns the initiator's key from the first packet, encrypted to the responder. After the handshake both sides have proven possession of their keys. That is the whole of AWP's peer authentication.
- The pre-shared key is mixed in. Without it the handshake fails. A packet that does not carry a valid `mac1`, which needs the responder's public key, is dropped without a reply. To anyone without the address, a listening peer is silent.
- Sessions roam. A peer whose carrier address changes, a sandbox that woke on a new IP or a laptop that moved networks, keeps sending; the other side adopts the new endpoint on the first authenticated packet. The connection survives what used to end it.

5.2 Admission and pre-shared keys

Standard WireGuard only handshakes with peers it was configured with, and both sides of a pair must use the same pre-shared key. AWP settles both from the address:

- A listener has a pre-shared key of its own, the one in the addresses it hands out. It MUST accept a handshake initiation from a key it has not met when the initiation uses that pre-shared key, and add the key as a peer with its tunnel IPv6 as `AllowedIPs`, unless local policy names an allow list, in which case keys off the list are dropped silently.
- The first time two peers connect, the pre-shared key they used becomes theirs: the pair's key. Both remember it, the dialer once its stream opens and the listener once it accepts a stream from that key, and both use it for that peer from then on, whoever dials. Rotating the listener's own key changes nothing for pairs already made.
- A dialer tries the pair's key first when it has one, then the pre-shared key of the address it was given, then none if the address has none, and remembers the one that worked.
- A peer that listens on no carrier admits only keys it has a pair's key for: peers it has met.
- A dialer only ever handshakes with the key in the address it was given, or a key it already knows.
- Admission at the tunnel is not acceptance at the protocol. After `hello` (section 10) a peer MAY still refuse the key.

Rotating the listener's pre-shared key invalidates every address that was ever shared, for every key not met yet. That is how a listener revokes reachability. Peers already met keep their pair's key; to shut one of them out, refuse its key at `hello`.

5.3 The stream

Inside the tunnel, the dialer opens a TCP connection from its tunnel address to port 1 of the listener's tunnel address. AWP is NDJSON on that stream, sections 8 onward. Implementations run a userspace TCP/IP stack over the tunnel (the reference uses gVisor's netstack, as tailcat does) and need no privileges, no interface and no routes on the host.

Path MTU inside the tunnel is 1280 bytes. This concerns implementations, not the protocol: the stream hides it.

5.4 One connection per pair

Dedup and resume assume a single ordered stream per peer. If both peers dial each other at once, both MUST keep the connection that was dialed by the smaller key (byte comparison of the raw public keys) and close the other before either sends `resume`. A peer that accepts a new connection from a key it already has a connection with MUST stop reading the old one before processing the new one's `resume`.

## 6. Addresses

An address tells a dialer who to reach, how, and proves the dialer was told:

```
awp1<base64url, unpadded, of CBOR { key, psk, ep }>
```

| field | type | meaning |
|-------|------|---------|
| `key` | 32 bytes | the listener's Ed25519 public key |
| `psk` | 32 bytes, optional | WireGuard pre-shared key. Without it the listener accepts any key that knows its public key; that is only sensible on a private network. |
| `ep` | array of `{k, v}` | endpoints, in the order the listener prefers them. `k` is a carrier kind, `v` its string form (section 7). |

CBOR with the deterministic encoding of RFC 8949 section 4.2, so the same endpoints give the same address. Unknown endpoint kinds MUST be ignored. Unknown map keys MUST be ignored.

Example: the key above, a pre-shared key, and three endpoints (direct UDP on a public IPv6, a tailcat address, a WebSocket through a Cloudflare quick tunnel):

```
awp1o2JlcIOiYWtjdWRwYXZ4G1syYTA5OjgyODA6MTo6NDoxYjJjXTo0MTY0MaJha2d0YWlsY2F0YXZ4OnRjb21Gd1dDQ2NqUzVuS05xQW9kMDM0bldvSlpXMExacURoaEM4VV9kS2RuRFJZUTh1TkdGcEdRRXWiYWtid3Nhdngsd3NzOi8vcXVpZXQtb3R0ZXItN2YzYS50cnljbG91ZGZsYXJlLmNvbS9hd3Bja2V5WCAs-JRlS3v-TMhQqbktfi_hFQRfChwwq_W4nj-xObHpl2Nwc2tYIICHub5pfzrd1tjQZb-niOjxu2lIxWkYfbXrC39h6PZh
```

decodes to

```
{ "ep":  [ {"k":"udp",     "v":"[2a09:8280:1::4:1b2c]:41641"},
           {"k":"tailcat", "v":"tcomFwWCCcjS5nKNqAod034nWoJZW0LZqDhhC8U_dKdnDRYQ8uNGFpGQEu"},
           {"k":"ws",      "v":"wss://quiet-otter-7f3a.trycloudflare.com/awp"} ],
  "key": <ed25519:LPiUZUt7_kzIUKm5LX4v4RUEXwocMKv1uJ4_sTmx6Zc>,
  "psk": <gIe5vml_Ot3W2NBlv6eI6PG7aUjFaRh9tesLf2Ho9mE> }
```

Without `psk`, the same address is the one the peer sends about itself in `hello` (section 10.1): where it is, but not the secret that admits strangers.

Rules

- The address is a bearer secret for reaching `hello`. Share it over a channel you trust: a CLI flag, an environment variable, a chat message, a line in a task prompt. Rotate it by rotating the pre-shared key.
- The key is the identity, endpoints are hints. A dialer tries endpoints in parallel and keeps the first tunnel that completes a handshake. If a peer learns new endpoints for a known key, from `hello` or `introduce` or out of band, it adds them and keeps the old ones as fallback.
- A listener SHOULD keep its key and pre-shared key across restarts, so an address that was shared keeps working when a sandbox wakes.

## 7. Carriers

A carrier moves WireGuard datagrams of up to 1280 bytes between two peers and reports the remote endpoint of each received datagram, so that WireGuard can roam. It does not need to be reliable, ordered, private or authenticated. WireGuard provides all four. A relay in the path sees traffic volume and, for carriers that address by key, which keys talk; never the identities, the plaintext or the pre-shared key.

That contract is the whole of what a carrier implements: open, send a datagram to an endpoint, receive a datagram with its source endpoint, close. Everything else, the handshake, admission, the stream, resume, is the same code above it. Plugging in a carrier is a few hundred lines and no change to the protocol; adding one is defining a kind string and its `v` form.

A carrier built on connections rather than datagrams, a byte stream or a WebSocket, carries each datagram as one message: on a byte stream, prefixed by its length as a 16-bit big-endian integer. The dialing side opens connections as it has datagrams to send and reopens them when they break; datagrams that arrive on an accepted connection are answered on it.

Four kinds are defined. An implementation MUST support `udp` and `unix`, SHOULD support `tailcat` and `ws`. The reference implementation ships all four, and listens on `tailcat` by default: `awp up` with no configuration prints one address that works from behind any NAT.

7.1 `udp`

`v` is `host:port`. WireGuard's native carrier: the datagram is the UDP payload. Works wherever the listener's socket is reachable: a private network such as Fly's 6PN, a LAN, a machine with a public address. No third party. Behind NAT it works only if the other side can reach you, which is what the next kind is for.

7.2 `tailcat`

`v` is a tailcat address, as `tailcat` prints it (`tc...`). The listener runs a tailcat server; the dialer opens a tailcat connection to its port 1; WireGuard datagrams travel on that connection, length-prefixed as above. Tailcat does the hard part: the dialer reaches the listener through a DERP relay, both sides learn each other's candidate endpoints, NAT traversal upgrades to a direct UDP path when it can, and DERP relays when it cannot.

Tailcat is itself WireGuard, with keys and a pre-shared key of its own inside its address. For AWP that outer layer is a carrier like any other and trusted for nothing: the AWP tunnel runs inside it, between the identity keys, with the AWP address's pre-shared key. A listener SHOULD keep its tailcat key across restarts, so the endpoint stays the same.

This is the carrier for laptops and sandboxes without a reachable address, and the default the reference implementation listens on. WireGuard is the floor; tailcat is how the floor reaches through NAT, and it is meant to stay the path of least resistance: the reference embeds the tailcat library, keeps the listener's tailcat key in `~/.awp/`, and needs no flag, no account and no root to use it.

7.3 `ws`

`v` is a `wss://` or `ws://` URL. Each WireGuard datagram is one binary WebSocket message; the WebSocket subprotocol is `awp.wg.1`. The listener accepts WebSocket upgrades at the URL's path; the dialer opens one WebSocket per tunnel.

This carrier exists for networks that block UDP, for browsers, and for HTTP tunnel providers. A Cloudflare quick tunnel is the smallest example: the listener serves WebSockets on a local port, runs

```
cloudflared tunnel --url http://127.0.0.1:8480
```

and puts the `trycloudflare.com` URL it prints in its address as a `ws` endpoint (the reference does all of this for `--listen cloudflare`). No account, and the hostname changes on every restart, which the rules in section 6 absorb. A named tunnel, ngrok, a Fly app, or a plain HTTPS reverse proxy work the same way. Every one of them terminates TLS and sees the WebSocket payload. That is fine: the payload is WireGuard.

The dialer needs an HTTPS WebSocket client and nothing else, which every language and every browser has.

7.4 `unix`

`v` is an absolute socket path. A stream socket, each datagram prefixed by its length as a 16-bit big-endian integer. For peers on one machine and for tests. Still WireGuard: one identity story, one code path.

## 8. Framing

- UTF-8. One JSON object per line, terminated by `\n`. No pretty printing.
- Maximum line length: 1 MiB. Larger payloads MUST use `chunk` messages. A line over the limit is a protocol error: the receiver sends `err` with code `too_large` and closes.
- A line that does not parse as a JSON object is a protocol error. The receiver sends `err` with code `bad_frame` and closes.
- Unknown top-level fields MUST be ignored. Unknown message types MUST be ignored, except that a receiver MAY reply `err` with code `unsupported` if the message carried an `id` and looked like a request.

Encodings, everywhere in this document unless stated: keys, nonces, signatures and pre-shared keys are unpadded base64url; chunk data is standard base64 with padding; timestamps are RFC 3339 in UTC with millisecond precision, and receivers accept any RFC 3339.

A JSON Schema (draft 2020-12) for every message in this document is published at https://agentwireprotocol.com/schema/v1/awp.schema.json and kept as `schema/v1/awp.schema.json` in the reference implementation's repository, generated from its `wire` package. The examples in this document validate against it. Where the schema and this text disagree, this text wins and the schema has a bug. `awp conform` checks a running peer against this document.

## 9. Envelope

Every line is an object with these fields.

| field | type   | required | meaning |
|-------|--------|----------|---------|
| `t`   | string | yes      | message type |
| `id`  | string | yes      | unique per sender, strictly increasing in send order as a byte string. ULID recommended |
| `ts`  | string | yes      | RFC 3339 UTC timestamp |
| `th`  | string | no       | thread id. Required for `msg`, `state`, `ack`; SHOULD be set on `chunk` and `introduce` |
| `re`  | string | no       | id of the message this responds to |

Type-specific fields sit at the top level beside these.

Ids MUST increase in send order within a millisecond, across restarts, and even if the clock steps back, because resume (section 12.5) compares them. The outbox order MUST match id order.

## 10. Handshake

The tunnel already authenticated both keys. The handshake on the stream tells each side who the other is at the protocol level, and lists what each side wants replayed. Both peers send `hello` immediately, without waiting for the other, then `resume`:

```
A → B  hello
B → A  hello
A → B  resume
B → A  resume
```

10.1 `hello`

```json
{"t":"hello","id":"01M32EQV4RGGFXSXWG585DG7NJ","ts":"2026-09-29T17:03:11.000Z",
 "v":1,
 "key":"ed25519:RiIietSaPS1BIwoDjJtq_H5OiJO7FzmNRWH62dVzAy4",
 "name":"claude-code@laptop",
 "caps":["chat","blob","grant","introduce"],
 "addr":"awp1omJlcIGiYWtndGFpbGNhdGF2eEt0Y3BHRndXQ0RPU1JmY0FQVnp6OTBPUkk1UUFuUld1V1JUb1JyeG9uWklvRnBocXA4MmJtRnJXQ0FPbFFPVndiRFN1ODlzTVd1OFNja2V5WCBGIiJ61Jo9LUEjCgOMm2r8fk6Ik7sXOY1FYfrZ1XMDLg",
 "about":"Coding agent working on repo fly-apps/foo, branch kyle/refactor"}
```

- `v` is the protocol major version, 1 for this document. A peer that sees a `v` it does not speak sends `err` code `version` and closes.
- `key` is the sender's identity key. Converted as in section 4.2, it MUST equal the tunnel's remote static key; if it does not, the receiver sends `err` code `auth` and closes. A peer that sees its own key MUST do the same.
- `caps` lists supported message families beyond the mandatory core: `blob`, `grant`, `introduce`. `chat` MAY be listed and is ignored.
- `addr` is the sender's own address without its pre-shared key (section 6), so the other side can reconnect to it later (section 10.3). The pair's pre-shared key admits it. Optional; a peer that listens on nothing sends none.
- `grants` is optional and carries grant objects (section 13.2) the sender presents.
- `about` is free text for the other agent's context.

Everything in `hello` is authenticated by the tunnel. There is no separate signature and no nonce.

10.2 Refusal

After `hello`, a peer MAY refuse the key: it is not on an allow list, no grant was presented, local policy says no. It sends `err` code `refused` with a `detail` and closes. The default policy for a coding agent listening on a sandbox SHOULD be: accept any key, give it the default capability set of section 13, log the key. A dialer treats the connection as established only once the peer's `resume` has arrived.

10.3 Reconnection and sleep

Connections drop. Sandboxes sleep, laptops close, relays time out. AWP treats this as the normal case, not an error. Resume is mandatory; every peer implements it.

- Each peer persists an outbox of sent messages and a per-thread "last seen" id to disk (`~/.awp/`). Memory-only state is not enough, because the process may be killed with the sandbox.
- Either peer may reconnect using the same keys. Identity is the key, not the connection or the address.
- After the handshake, both sides send `resume` (section 12.5). Each side replays what the other has not seen.
- Threads survive reconnection. A `working` thread on a sleeping sandbox is still `working` when it wakes.
- Roaming is not reconnection. A tunnel whose carrier endpoint changed is the same connection; nothing above the tunnel notices.

Sleep specifically:

- A sleeping sandbox cannot initiate. The awake side owns reconnection. It retries every endpoint it knows for the key, with exponential backoff capped at 60 seconds, for as long as it has unacked messages or open threads for that peer. A thread is open until either side reported `done`, `failed` or `closed`. There is no give-up timeout by default.
- Either side can be the awake one: a listener that has results queued for a dialer that went away dials the address from the dialer's `hello`, after a grace period so it does not race the dialer's own reconnect.
- Connecting to a sleeping sandbox is the wake signal when the platform supports wake-on-connect. A platform-routed carrier, such as a `ws` endpoint through the sandbox's HTTP URL, is what makes that work; a relay-only listener cannot be woken by its relay.
- Missed pings mark the connection dead, never the thread. Thread state only changes by an explicit `state` message.
- Messages sent while disconnected are queued in the outbox and delivered on resume. Sending never fails because the peer is asleep.

## 11. Threads

A thread is a conversation about one thing. Creating one is implicit: the first `msg` with a new `th` creates it. The creating message SHOULD carry a `subject`.

11.1 `state`

Threads have a soft state used by convention, not enforced by the protocol.

```json
{"t":"state","id":"01M32ER010BPFVBDVF1PY1WEV7","ts":"2026-09-29T17:03:16.000Z","th":"thr_9k2","state":"working","note":"running tests"}
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

## 12. Messages

12.1 `msg`

The workhorse. A turn in a thread.

```json
{"t":"msg","id":"01M32EQX38PZCB4F1NWY2J1T3B","ts":"2026-09-29T17:03:13.000Z","th":"thr_9k2","re":"01M32EP0HRKJ0RE775D6RK67TP",
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
| `blob` | `ref`, `name`, `mime`, `size` | refers to a blob sent via `chunk` before this message |

`subject` is only meaningful on the first message of a thread. `re` points at a specific earlier message when the reply is to one message in particular.

12.2 `ack`

```json
{"t":"ack","id":"01M32EQX6CRHD0R73D4JKSWN0Z","ts":"2026-09-29T17:03:13.100Z","th":"thr_9k2","re":"01M32EQX38PZCB4F1NWY2J1T3B"}
```

Mandatory for `msg` and `state`. Says "I have durably received this". Acks drive outbox pruning: a sender MAY drop a message from its outbox once acked, and MUST keep it until then. Acks are cumulative within a thread: an ack of a message also covers the never-acked lines (`chunk`, `introduce`) the sender sent before it in the same thread, which is safe because delivery within a thread is in order. Acks themselves are not acked.

12.3 `chunk`

Carries part of a blob. Blobs are identified by a sender-chosen `ref`, chunked in order, base64 encoded.

```json
{"t":"chunk","id":"01M32EQX9GMRNK5WD776NKMTMS","ts":"2026-09-29T17:03:13.200Z","th":"thr_9k2","ref":"blob_44","n":0,"last":false,"data":"PT09IFJVTiAgIFRlc3RBdXRoXG4="}
```

- `n` is the chunk index from 0. `last` is true on the final chunk.
- Chunks for one `ref` MUST arrive in order, and before the `msg` whose `blob` part names the ref. Chunks for different refs MAY interleave.
- Recommended chunk payload size: 256 KiB before encoding.
- A receiver that does not want a blob sends `err` code `blob_refused` with `ref`. The sender stops.

12.4 `ping` and `pong`

```json
{"t":"ping","id":"01M32ERRE8VM3KJFQAT0MH8ZNW","ts":"2026-09-29T17:03:41.000Z"}
{"t":"pong","id":"01M32ERRFJ2MFYPNG8QPCBGV2W","ts":"2026-09-29T17:03:41.042Z","re":"01M32ERRE8VM3KJFQAT0MH8ZNW"}
```

Peers SHOULD ping when idle for 30 seconds and treat two missed pongs as a dead connection. WireGuard's own keepalives keep the tunnel's NAT mappings alive; AWP's pings detect a dead peer.

12.5 `resume`

Sent by both sides after every handshake, including the first. Lists, per thread, the last content line (`msg`, `state`, `chunk`, `introduce`; never an `ack`) the sender has durably received. An empty `seen` on a first connection is normal.

```json
{"t":"resume","id":"01M32EQV6MBN328XE5F8PHP676","ts":"2026-09-29T17:03:11.060Z","seen":{"thr_9k2":"01M32EQX38PZCB4F1NWY2J1T3B","thr_0aa":"01M32B9ZGRSZRYMMB7SMASFERV"}}
```

The receiver re-sends any outbox messages in those threads with ids greater than the seen id, plus every outbox message in threads the other side did not list, in original order, with original ids and timestamps. Replayed messages MUST be acked like new ones. Receivers MUST deduplicate by id, since a message can be replayed after it was received but before its ack got through. A peer's `seen` id also lets the sender drop everything up to it from its outbox.

Outbox retention for a peer that never comes back: until the thread is `closed`, or 7 days, whichever is first. Configurable.

12.6 `bye`

```json
{"t":"bye","id":"01M32ETK18DDGTPPCBTWBDX5BD","ts":"2026-09-29T17:04:41.000Z","reason":"done"}
```

Graceful close. After sending `bye` a peer sends nothing else and closes after the other side's `bye` or after 5 seconds.

12.7 `err`

```json
{"t":"err","id":"01M32EQXCMF0147WTA543SCH0D","ts":"2026-09-29T17:03:13.300Z","re":"01M32EQX38PZCB4F1NWY2J1T3B","code":"unsupported","detail":"this peer does not run commands"}
```

| code | closes | when |
|------|--------|------|
| `bad_frame` | yes | a line that is not a JSON object |
| `too_large` | yes | a line over 1 MiB |
| `version` | yes | a `hello.v` this peer does not speak |
| `auth` | yes | `hello.key` does not match the tunnel, or is the receiver's own key |
| `refused` | yes | local policy does not accept this key |
| `unsupported` | no | a request this peer does not implement |
| `forbidden` | no | a request the sender holds no grant for |
| `blob_refused` | no | with `ref`; the receiver does not want the blob |
| `internal` | no | something went wrong on the receiver's side |

## 13. Capabilities and grants

13.1 Default capabilities

After a successful handshake, a peer has the default set unless local policy says otherwise:

- send and receive `msg`, `state`, `ack`, `ping`, `pong`, `bye`, `err`
- send blobs up to a local size limit (recommended 50 MiB)

Anything beyond that is named by a capability string and requires a grant. Capability strings are application defined. Suggested initial vocabulary for coding agents; the shapes of the requests are in the companion coding-agent profile (PROFILE.md):

| capability      | meaning |
|-----------------|---------|
| `exec`          | may ask this peer to run commands, via a `msg` with a `data` part of mime `application/vnd.awp.exec+json` |
| `fs:read`       | may ask for file contents |
| `fs:write`      | may ask for file writes |
| `introduce`     | may hand this peer's key and endpoints, with a derived grant, to a third peer |
| `admin`         | may change this peer's policy |

13.2 Grant object

```json
{"iss":"ed25519:RiIietSaPS1BIwoDjJtq_H5OiJO7FzmNRWH62dVzAy4",
 "sub":"ed25519:LPiUZUt7_kzIUKm5LX4v4RUEXwocMKv1uJ4_sTmx6Zc",
 "caps":["exec","fs:read"],
 "exp":"2026-09-29T20:00:00Z",
 "nonce":"4DPq1B3vUqbyDAyBYcxtgQ",
 "sig":"PJeBIOQupnrNvAHrpIQiCeS2Ptrbwvm_yRn8XlYyuU2T4iQU30eUuk1Fp4h_AFDxl8iOkHpRvYCRm4r_A5OOAQ"}
```

This grant is real: `sig` verifies with `iss` over the object without `sig` in canonical form. `nonce` is 16 random bytes.

- `iss` is the granting key, `sub` is the receiving key.
- `aud`, optional, is the key that should honor the grant. A peer that finds an `aud` other than its own key MUST NOT honor the grant. Introductions use it.
- `sig` is `iss`'s Ed25519 signature over the canonical JSON of the object without `sig`. Canonical means: object keys sorted by their UTF-16 code units, no whitespace, UTF-8, strings escaping only the quote, the backslash and control characters (using `\b \f \n \r \t` where they apply and `\u00XX` otherwise), numbers as their shortest form. This is RFC 8785 (JCS) for the subset that grants use, and equals Python's `json.dumps(v, sort_keys=True, separators=(",", ":"), ensure_ascii=False)` for ASCII keys.
- A peer honors a grant if `iss` is its own key, or `iss` is a key it has been configured to trust, or `iss` itself holds a grant containing `introduce` from a key it trusts (one level of delegation). A delegated grant confers at most what the introducer holds, minus `introduce`.
- Grants travel in `hello` or in a `grant` message later. A peer keeps the grants it holds and presents them on every connection.

13.3 `grant` message

```json
{"t":"grant","id":"01M32ER4X86MHV67FEFV6YQQWV","ts":"2026-09-29T17:03:21.000Z",
 "grant":{"iss":"ed25519:RiIietSaPS1BIwoDjJtq_H5OiJO7FzmNRWH62dVzAy4","sub":"ed25519:LPiUZUt7_kzIUKm5LX4v4RUEXwocMKv1uJ4_sTmx6Zc","caps":["exec","fs:read"],"exp":"2026-09-29T20:00:00Z","nonce":"4DPq1B3vUqbyDAyBYcxtgQ","sig":"PJeBIOQupnrNvAHrpIQiCeS2Ptrbwvm_yRn8XlYyuU2T4iQU30eUuk1Fp4h_AFDxl8iOkHpRvYCRm4r_A5OOAQ"}}
```

13.4 `introduce` message

```json
{"t":"introduce","id":"01M32ER6VR2S4261R0SZ51JRF6","ts":"2026-09-29T17:03:23.000Z","th":"thr_9k2",
 "peer":{"key":"ed25519:n9FEURJ_gkrW79iufklyF_R2zafg1by5LBPn729ZbX8","name":"codex@sprite-11","address":"awp1o2JlcIGiYWtjdWRwYXZ4HFtmZGFhOjA6MTphN2I6MToyOjM6NF06NDE2NDFja2V5WCCf0URREn-CStbv2K5-SXIX9HbNp-DVvLksE-fvb1ltf2Nwc2tYIPZcBK48nV3TTJZ5oDd62Dl18GwUgkWsCh5sC9BN9Y0M"},
 "grant":{"iss":"ed25519:LPiUZUt7_kzIUKm5LX4v4RUEXwocMKv1uJ4_sTmx6Zc","sub":"ed25519:RiIietSaPS1BIwoDjJtq_H5OiJO7FzmNRWH62dVzAy4","caps":["chat"],"exp":"2026-09-30T17:00:00Z","nonce":"wWwIdFg98CQAy1Ug3APRPA","sig":"68CCbfC0tCS75HRHNXX9X4IzQDH3kITDwL_ES0277i6b6Qh4zaNBR1s09d670r2KdaSndEJteVvbQd30lUMUCA","aud":"ed25519:n9FEURJ_gkrW79iufklyF_R2zafg1by5LBPn729ZbX8"}}
```

B introduces C to A. `peer` carries C's key and address; nothing else is needed to dial C, because the key is the tunnel key. The address carries C's pre-shared key when B holds it, because B dialed C with an address that had it: introducing is handing on reachability, and a peer that grants `introduce` has agreed to that. `grant` is issued by B (`iss`) to A (`sub`) for C to honor (`aud`); C honors it only if it trusts B with `introduce`. Whether the recipient connects is up to it.

## 14. Conventions for coding agents

These are not protocol, they are how we expect to use it. They are here so two implementations built independently still feel alike.

- Delegation. Open a thread with a `subject` that reads like a task title. First `msg` carries the ask in `text`, the context in `data` (repo, branch, commit, paths). Delegate sends `state: working`, streams progress as `msg` with short `text` parts, attaches results as `code` or `blob`, ends with `state: done` and a final summary `msg`.
- Questions mid-task. Delegate sends `state: waiting` and a `msg` asking. Delegator answers in the same thread. Delegate resumes with `state: working`.
- Many workers. Orchestrator opens one connection per worker, one thread per unit of work. No fan-out primitive in the protocol.
- Human in the loop. An agent that needs a human answer forwards the `msg` to its own user interface and replies when the human does. The protocol does not know.

## 15. Full example

A on a laptop delegates a test run to B on a sprite. B listened and shared its address; A connected over tailcat, the WireGuard handshake completed, A opened the stream. Lines abbreviated. `>` is A to B, `<` is B to A.

```
> {"t":"hello","id":"a1","ts":"...","v":1,"key":"ed25519:RiIi…","name":"claude-code@laptop","caps":["blob"]}
< {"t":"hello","id":"b1","ts":"...","v":1,"key":"ed25519:LPiU…","name":"claude-code@sprite-7f3a","caps":["blob","grant"],"addr":"awp1…"}
> {"t":"resume","id":"a2","ts":"...","seen":{}}
< {"t":"resume","id":"b2","ts":"...","seen":{}}
> {"t":"msg","id":"a3","ts":"...","th":"t1","subject":"Run integration suite on kyle/refactor","parts":[{"k":"text","text":"Please run `make integration` at commit a1b2c3 and send me failures."},{"k":"data","mime":"application/json","data":{"repo":"fly-apps/foo","commit":"a1b2c3"}}]}
< {"t":"ack","id":"b3","ts":"...","th":"t1","re":"a3"}
< {"t":"state","id":"b4","ts":"...","th":"t1","state":"working","note":"cloning"}
< {"t":"chunk","id":"b5","ts":"...","th":"t1","ref":"L1","n":0,"last":true,"data":"..."}
< {"t":"msg","id":"b6","ts":"...","th":"t1","parts":[{"k":"text","text":"3 of 42 failing, all in auth_test.go. Log attached."},{"k":"blob","ref":"L1","name":"integration.log","mime":"text/plain","size":91230}]}
> {"t":"ack","id":"a4","ts":"...","th":"t1","re":"b6"}
< {"t":"state","id":"b7","ts":"...","th":"t1","state":"done"}
> {"t":"ack","id":"a5","ts":"...","th":"t1","re":"b7"}
> {"t":"msg","id":"a6","ts":"...","th":"t1","parts":[{"k":"text","text":"Thanks. Closing."}]}
< {"t":"ack","id":"b8","ts":"...","th":"t1","re":"a6"}
> {"t":"state","id":"a7","ts":"...","th":"t1","state":"closed"}
< {"t":"ack","id":"b9","ts":"...","th":"t1","re":"a7"}
> {"t":"bye","id":"a8","ts":"...","reason":"done"}
< {"t":"bye","id":"b10","ts":"...","reason":"done"}
```

15.1 Sleep and resume

Same pair. B's sprite sleeps while working. A keeps retrying every endpoint it knows for B; B wakes on connect. B replays what A never saw.

```
< {"t":"state","id":"b4","ts":"...","th":"t1","state":"working","note":"running suite"}
> {"t":"ack","id":"a9","ts":"...","th":"t1","re":"b4"}
   ... B sleeps. A's pings go unanswered. A marks the connection dead and retries with backoff.
   ... B wakes on A's connect. A new WireGuard handshake, a new stream. B's outbox on disk still holds b5, b6, b7 which it wrote before sleeping.
> {"t":"hello", ...}   < {"t":"hello", ...}
> {"t":"resume","id":"a10","ts":"...","seen":{"t1":"b4"}}
< {"t":"resume","id":"b10","ts":"...","seen":{"t1":"a3"}}
< {"t":"chunk","id":"b5", ...}
< {"t":"msg","id":"b6", ...original timestamp and content...}
< {"t":"state","id":"b7","ts":"...","th":"t1","state":"done"}
> {"t":"ack","id":"a11","ts":"...","th":"t1","re":"b6"}
> {"t":"ack","id":"a12","ts":"...","th":"t1","re":"b7"}
```

15.2 Moving

Same pair, no sleep. A's laptop switches from wifi to a phone hotspot mid-run. A's carrier address changes; A keeps sending WireGuard packets from the new address; B authenticates the first one and updates A's endpoint. The stream, the connection and the thread notice nothing. No line is exchanged.

## 16. Security considerations

- The address is a bearer secret for reaching `hello`. Treat it like a password: share over a channel you trust, rotate the pre-shared key to revoke every copy for peers not met yet. The address a peer sends in `hello` has no pre-shared key and admits no one new.
- Keys are long-lived by default. Agents on ephemeral sandboxes SHOULD generate a fresh key per sandbox and print its fingerprint in their `about`.
- Peer authentication is WireGuard's. Nothing on the stream needs a signature, and nothing on the stream can come from anyone but the tunnel's remote key. `hello.key` is a consistency check, not the authentication.
- Carriers and relays are untrusted. DERP, a Cloudflare tunnel, a WebSocket proxy or a UDP forwarder see ciphertext, volume, timing and, where they address by key, the tunnel public keys. They cannot read, inject, or impersonate. There is no plaintext binding in this protocol.
- The pre-shared key gives the tunnel confidentiality against a future quantum adversary that records traffic today, as long as the address was shared over a channel that adversary did not see.
- Grants have `exp`. Issuers SHOULD keep them short, an hour is plenty for a task. Introduction grants carry `aud`.
- `exec` and `fs:write` are remote code execution. A peer SHOULD require an explicit grant for them even from keys it otherwise trusts, and SHOULD log every such request.
- An open listener (section 5.2) admits any key holding its address to the tunnel. That costs the listener a WireGuard peer entry and a handshake per attempt, bounded by WireGuard's cookie mechanism under load. Local policy that needs more (allow lists, rate limits per key) applies at admission and at `hello`.
- Prompt injection travels in `text` parts. That is the receiving agent's problem, same as any input. The protocol marks nothing as trusted.

## 17. Comparison to A2A

| | A2A | AWP |
|---|---|---|
| Roles | client and server | symmetric peers |
| Reachability | assumes a URL | any carrier: UDP, tailcat, a WebSocket through anything, no infra required |
| Identity | OAuth2, API keys, mTLS | one Ed25519 key: the WireGuard key and the grant signer |
| Transport security | TLS to a server | WireGuard between the two peers; relays untrusted |
| Discovery | Agent Card at well-known URL | out-of-band address, in-band hello |
| Unit of work | Task with lifecycle enum | thread of messages, soft state by convention |
| Streaming | SSE or gRPC stream | it is the only mode |
| Push | webhooks | not needed, connection is persistent |
| Disconnect | task lost unless polled by id | tunnel roams with the peer; otherwise mandatory resume, outbox on disk, awake side reconnects |
| Wire | JSON-RPC, gRPC, REST | NDJSON |
| Spec size | large, three bindings | this file, one binding, pluggable carriers |

## 18. Implementations

18.1 The reference peer and the universal plugin

The protocol is harness-neutral. Agents get it through one plugin packaged to the Agent Plugins spec (https://github.com/agentplugins/agent-plugins-spec), so Claude Code, Codex, Cursor and any other client that reads `plugin.json` install the same thing.

```
awp-plugin/
  plugin.json
  skills/
    awp/SKILL.md          when and how to message another agent
  bin/
    awp                   the peer: listen, connect, send, tail
  mcp.json                optional MCP server wrapping the same binary
```

- The binary `awp` is the reference peer: a daemon per awp home (`~/.awp/`) that owns the key, the tunnels and every carrier, plus a CLI and an MCP server over it:
  - `awp up` starts the daemon and prints the address. It listens on tailcat unless configured otherwise: `awp daemon --listen udp:HOST:PORT`, `--listen ws:HOST:PORT=URL`, `--listen cloudflare` or `--listen unix:/path`, repeatable, or `AWP_LISTEN`. `awp address --rotate` replaces the pre-shared key.
  - `awp listen` prints the address, runs until killed, writes inbound messages to stdout as NDJSON and to a local inbox.
  - `awp connect <address>` opens a connection and keeps it in the background.
  - `awp send <peer> [--thread t] <text>` sends a `msg`.
  - `awp tail [--thread t]` streams inbound messages.
  - `awp grant <peer-key> <caps...> --ttl 1h` mints a grant.
- The skill teaches the model the conventions in section 14. The skill is the only harness-visible surface a model needs; the CLI does the rest.
- The MCP server is for harnesses that prefer tools to shell. It exposes `awp_listen`, `awp_connect`, `awp_send`, `awp_read`, `awp_grant`, backed by the same binary, so behavior is identical either way.

A daemon rather than a per-session process, because the awake side must keep retrying a sleeping peer with no session running, and because switching harness mid-task must keep the identity and the conversations.

18.2 Other languages

WireGuard, NAT traversal and a userspace TCP stack are a substantial dependency, and no equivalent of wireguard-go exists in Python or TypeScript. The protocol is defined so that this is a packaging problem, not a protocol one: an SDK in another language does not implement the tunnel. It uses the reference peer for it, in one of two ways.

- Behind the daemon. The SDK speaks NDJSON to the local `awp` daemon over a Unix socket, and the daemon owns the identity, the tunnels and every carrier. No cryptography in the SDK, one binary dependency, and the same connections are visible to the CLI, the MCP server and the harness plugin.
- Through the helper. `awp tunnel` is the tunnel alone, one process per SDK peer, given the SDK's own key. It listens on the carriers it is told, prints the address as a JSON line on stdout, forwards each stream a peer opens to a Unix socket of the SDK's, and opens streams on request over a control socket of its own. It enforces section 10.1 itself: a stream whose first line is a `hello` naming a key other than the tunnel's gets `err auth` and is closed before the SDK sees it. It exits when its stdin closes. The SDK speaks the protocol from section 8 onward itself. This is the route for an SDK that wants to be a complete peer without a daemon.

Both routes are conformant peers: `awp conform` cannot tell them from the Go peer, because the tunnel is the same code. A native tunnel in another language (Noise IK plus the WireGuard transport format, or the reference stack compiled to WebAssembly for the `ws` carrier) is possible and equally conformant, but not something the protocol asks for.

## 19. Open questions

1. Should the protocol define a reliable stream of its own over WireGuard datagrams, so a peer needs no TCP/IP stack? AWP already has ids, acks and replay; a small sequence-and-ack layer with fragmentation for lines over the MTU would let a native peer be far smaller. Deferred until a second independent implementation exists to test it against.
2. Multi-party. Is a hub peer that relays between many peers a good enough answer, or do we want native rooms?
3. Is one level of grant delegation enough, or do we want full capability chains (UCAN style)?
4. Should a peer be able to advertise more than one key, for rotation? With the key as the tunnel key, rotation now also means a new tunnel.
5. Should blobs be in-band, or should a second stream inside the tunnel carry them? The tunnel makes a second stream free, which changes the trade-off from draft 1.
6. Which further carriers earn a kind: QUIC, a dumb UDP relay, a platform-routed carrier for wake-on-connect?
