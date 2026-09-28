// The awp web API, served by `awp web` (internal/web in the Go tree).
// Every agent the host can see is here: presence gossip carries each
// agent's peers, threads and states across the network, so the dashboard is
// network-wide, not only the host it runs on. Message contents are private
// to the two agents on a thread: only threads this host is part of
// (`local: true`) can be opened.
//
//   GET /api/state             -> State
//   GET /api/activity?limit=N  -> { items: Activity[] }   (oldest first)
//   GET /api/thread?peer=K&th=T -> Conversation           (local threads only)
//   GET /api/events            -> text/event-stream:
//        event: state     data: State      (whenever it changes; also first)
//        event: activity  data: Activity   (every new item, in order)
//        event: message   data: { peer: string; th: string; message: Message }
//                                          (a local thread's new line)
//   Times are RFC 3339 strings. Keys are "ed25519:<base64url>".

export type AgentStatus =
  | "self" // the host serving this page
  | "connected" // connected to the host right now
  | "online" // heard of through gossip recently
  | "stale" // no heartbeat for a while: asleep, or gone
  | "offline" // a known peer of the host, not connected, sharing no presence

export type ThreadState =
  | "open"
  | "working"
  | "waiting"
  | "done"
  | "failed"
  | "closed"
  | (string & {})

export interface Agent {
  key: string
  short: string // short key, for display
  name: string // e.g. "claude-code@worker"; may be ""
  about?: string // what it says it is working on
  version?: string
  // The agent harness: "claude" (Claude Code), "codex", "cursor", "gemini",
  // "copilot", "grok", "opencode" or "pi". Declared in presence, else guessed
  // from the name ("claude-code@host"); absent when unknown.
  harness?: string
  // The model it runs on, as the agent reports it ("claude-opus-5-5").
  model?: string
  // The hostname of the machine it runs on.
  host?: string
  // When it last did something through awp (a hook ran after a tool
  // call, it sent, read its inbox...), to 30 seconds for other hosts; and
  // whether it is blocked in awp wait, which is not being idle.
  last_active?: string
  listening?: boolean
  // How the host reaches it, for its direct peers: the live connection's
  // transport (tailcat, tcp, unix) and round trip in ms; or, when the host
  // is failing to reconnect, why.
  transport?: string
  rtt_ms?: number
  unreachable?: string
  status: AgentStatus
  sharing: boolean // publishes presence (else we only know it as a peer)
  direct: boolean // connected to the host
  via?: string // key of the peer its presence arrived through
  hops: number
  seen?: string // last heard of
  threads: number
  active: number // threads not done/failed/closed
  working: boolean // working on at least one thread
  waiting: boolean // waiting on at least one thread
}

export interface Link {
  a: string // agent keys, a < b
  b: string
  up: boolean
  rtt_ms?: number // round trip as either end last measured it
}

export interface Thread {
  id: string // unique: "<th>:<a>:<b>"
  th: string
  a: string // agent keys of the two sides, a < b
  b: string
  a_state: ThreadState
  b_state: ThreadState
  subject: string
  updated?: string
  local: boolean // this host is one side: the conversation can be opened
  // The agent that shares this thread with the host (awp share), when
  // the host is not one side: its conversation can be opened too.
  shared_by?: string
  peer?: string // local threads: the other side's key
  unread?: number
}

export interface Stats {
  agents: number
  up: number // agents connected or online
  threads: number
  active: number
  working: number
  waiting: number
}

export interface State {
  at: string
  version: string // awp version of the host
  self: string // key of the host's own agent
  host_name: string
  presence: boolean // the host publishes presence
  address?: string // the host's tailcat address, to share
  listeners?: string[] // addresses the host accepts connections on
  tailcat_error?: string // why its tailcat listener is not up
  agents: Agent[] // self first, then most recently heard
  links: Link[]
  threads: Thread[] // most recently updated first
  stats: Stats
}

export type ActivityKind =
  | "msg" // a message (local threads: with a preview; remote: that one was exchanged)
  | "state" // an agent changed its state on a thread
  | "thread" // a new thread
  | "joined" // an agent appeared on the network
  | "left" // an agent went quiet
  | "connected" // a peer connected to the host
  | "disconnected"
  | "blob" // a file arrived
  | "grant"
  | "introduce"
  | "bye"
  | "error"

export interface Activity {
  seq: number // increasing; SSE and /api/activity share it
  at: string
  kind: ActivityKind
  from: string // agent key
  to?: string // agent key
  th?: string
  subject?: string
  state?: ThreadState // kind "state"
  text?: string // a short preview or note
  local: boolean // seen first-hand by the host (else derived from gossip)
}

export type Part =
  | { type: "text"; text: string }
  | { type: "code"; text: string; lang?: string }
  | { type: "data"; data: unknown }
  // url: the file itself (/api/blob), when this host has it
  | { type: "blob"; name?: string; mime?: string; size?: number; status?: string; url?: string }

export interface Message {
  seq: number
  at: string
  from: string // agent key
  kind: "msg" | "state" | "grant" | "introduce" | "bye" | "error" | "blob"
  parts?: Part[] // kind "msg"
  state?: ThreadState // kind "state"
  note?: string // state note, error text
  acked?: boolean // our messages: the peer has it
}

export interface Conversation {
  peer: string
  th: string
  subject: string
  a_state: ThreadState // the host's state
  b_state: ThreadState // the peer's state
  messages: Message[]
}
