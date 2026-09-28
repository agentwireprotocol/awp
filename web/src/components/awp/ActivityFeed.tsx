import { useEffect, useMemo, useRef, useState } from "react"

import { StreamText } from "@/components/atoms/StreamText"
import GlideMenu from "@/components/primitives/GlideMenu"
import { ago, clock } from "@/lib/format"
import { getStore, useNow } from "@/lib/store"
import type { Activity, Agent } from "@/lib/types"

import { AgentChip, StatePill } from "./chips"

type Filter = "all" | "messages" | "states" | "network"

const FILTERS: { id: Filter; label: string }[] = [
  { id: "all", label: "All" },
  { id: "messages", label: "Messages" },
  { id: "states", label: "States" },
  { id: "network", label: "Network" },
]

function matches(a: Activity, f: Filter): boolean {
  switch (f) {
    case "messages":
      return a.kind === "msg" || a.kind === "blob"
    case "states":
      return a.kind === "state" || a.kind === "thread"
    case "network":
      return ["joined", "left", "connected", "disconnected", "introduce", "grant", "bye", "error"].includes(a.kind)
  }
  return true
}

const VERB: Record<Activity["kind"], string> = {
  msg: "messaged",
  state: "is",
  thread: "opened a thread with",
  joined: "joined the network",
  left: "went quiet",
  connected: "connected to",
  disconnected: "disconnected from",
  blob: "sent a file to",
  grant: "granted",
  introduce: "introduced",
  bye: "said bye to",
  error: "error with",
}

function Glyph({ kind }: { kind: Activity["kind"] }) {
  const common = "size-3.5"
  const p = { fill: "none", stroke: "currentColor", strokeWidth: 2, strokeLinecap: "round" as const, strokeLinejoin: "round" as const }
  switch (kind) {
    case "msg":
      return <svg viewBox="0 0 24 24" className={common} {...p}><path d="M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.6A8 8 0 1 1 21 12z" /></svg>
    case "state":
      return <svg viewBox="0 0 24 24" className={common} {...p}><circle cx="12" cy="12" r="3" /><path d="M12 3v3M12 18v3M3 12h3M18 12h3" /></svg>
    case "thread":
      return <svg viewBox="0 0 24 24" className={common} {...p}><path d="M12 5v14M5 12h14" /></svg>
    case "joined":
    case "connected":
      return <svg viewBox="0 0 24 24" className={common} {...p}><path d="M5 12h14M13 6l6 6-6 6" /></svg>
    case "left":
    case "disconnected":
    case "bye":
      return <svg viewBox="0 0 24 24" className={common} {...p}><path d="M19 12H5M11 6l-6 6 6 6" /></svg>
    case "blob":
      return <svg viewBox="0 0 24 24" className={common} {...p}><path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z" /><path d="M14 3v6h6" /></svg>
    case "error":
      return <svg viewBox="0 0 24 24" className={common} {...p}><path d="M12 8v5M12 16.5v.5" /><circle cx="12" cy="12" r="9" /></svg>
    default:
      return <svg viewBox="0 0 24 24" className={common} {...p}><path d="M12 2l2.4 7.2L22 12l-7.6 2.8L12 22l-2.4-7.2L2 12l7.6-2.8z" /></svg>
  }
}

/** Long tokens (tailcat addresses, keys) shortened to their ends. */
function tidy(text: string): string {
  return text.replace(/\S{40,}/g, (t) => `${t.slice(0, 22)}…${t.slice(-6)}`)
}

function Row({
  a,
  agents,
  live,
  now,
  onAgent,
  onThread,
}: {
  a: Activity
  agents: Map<string, Agent>
  live: boolean
  now: number
  onAgent: (k: string) => void
  onThread: (a: Activity) => void
}) {
  const tone =
    a.kind === "error" ? "text-red bg-red-tint" : a.kind === "state" ? "text-accent-ink bg-accent-tint" : "text-ink-2 bg-field"
  const clickable = !!a.th
  return (
    <div
      data-menu-row
      role={clickable ? "button" : undefined}
      tabIndex={clickable ? 0 : undefined}
      onClick={clickable ? () => onThread(a) : undefined}
      onKeyDown={clickable ? (e) => e.key === "Enter" && onThread(a) : undefined}
      className={`relative z-10 flex gap-3 rounded-[10px] px-2.5 py-2.5 ${clickable ? "cursor-pointer" : ""}`}
      style={live ? { animation: "rise-in 420ms var(--ease-out-strong) both" } : undefined}
    >
      <span className={`mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full shadow-hairline ${tone}`}>
        <Glyph kind={a.kind} />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-[13px] leading-5 text-ink-2">
          <AgentChip agentKey={a.from} agent={agents.get(a.from)} onClick={() => onAgent(a.from)} />
          <span>{VERB[a.kind]}</span>
          {a.kind === "state" && <StatePill state={a.state} />}
          {a.to && a.kind !== "state" && a.kind !== "joined" && a.kind !== "left" && (
            <AgentChip agentKey={a.to} agent={agents.get(a.to)} onClick={() => onAgent(a.to!)} />
          )}
          {a.kind === "state" && a.to && (
            <>
              <span className="text-ink-3">with</span>
              <AgentChip agentKey={a.to} agent={agents.get(a.to)} onClick={() => onAgent(a.to!)} />
            </>
          )}
        </div>
        {a.text && a.kind !== "joined" && a.kind !== "left" && !(a.kind === "msg" && !a.local) && (
          <p className="mt-1 line-clamp-2 text-[13px] leading-5 text-ink">
            {live ? <StreamText text={tidy(a.text)} caret={false} charsPerTick={3} /> : tidy(a.text)}
          </p>
        )}
        {a.kind === "msg" && !a.local && (
          <p className="mt-1 text-[12px] text-ink-3 italic">contents private to the two agents</p>
        )}
        <div className="mt-1 flex min-w-0 items-center gap-1.5 text-[11.5px] text-ink-3">
          {a.subject && <span className="truncate">{a.subject}</span>}
          {a.subject && <span aria-hidden>·</span>}
          <time dateTime={a.at} title={clock(a.at)} className="shrink-0 tabular-nums">
            {ago(a.at, now)}
          </time>
          {!a.local && (
            <span className="shrink-0 rounded-full bg-field px-1.5 text-[10.5px] font-medium text-ink-3 shadow-hairline" title="Learned through presence gossip, not seen first-hand by this host">
              gossip
            </span>
          )}
        </div>
      </div>
    </div>
  )
}

export function ActivityFeed({
  activity,
  agents,
  query,
  onAgent,
  onThread,
  className = "",
  titled = true,
}: {
  activity: Activity[]
  agents: Map<string, Agent>
  query: string
  onAgent: (k: string) => void
  onThread: (a: Activity) => void
  /** false on the Activity page, whose title says it already */
  titled?: boolean
  className?: string
}) {
  const [filter, setFilter] = useState<Filter>("all")
  const now = useNow(5000)
  const liveSince = useRef(0)
  const [liveSeqs, setLiveSeqs] = useState<Set<number>>(new Set())

  useEffect(() => {
    liveSince.current = Date.now()
    return getStore().onActivity((a) =>
      setLiveSeqs((s) => {
        const n = new Set(s)
        n.add(a.seq)
        if (n.size > 50) n.delete(n.values().next().value!)
        return n
      }),
    )
  }, [])

  const items = useMemo(() => {
    const q = query.trim().toLowerCase()
    const out: Activity[] = []
    for (let i = activity.length - 1; i >= 0 && out.length < 200; i--) {
      const a = activity[i]
      if (!matches(a, filter)) continue
      if (q) {
        const hay = [a.text, a.subject, agents.get(a.from)?.name, a.to && agents.get(a.to)?.name, a.state, a.kind]
          .filter(Boolean)
          .join(" ")
          .toLowerCase()
        if (!hay.includes(q)) continue
      }
      out.push(a)
    }
    return out
  }, [activity, filter, query, agents])

  const idx = FILTERS.findIndex((f) => f.id === filter)

  return (
    <section className={`flex min-h-0 flex-col ${className}`}>
      {titled && (
        <div className="flex items-center justify-between gap-3 px-4 pt-4 pb-3">
          <div>
            <h2 className="text-[15px] font-semibold tracking-[-0.01em] text-ink">Activity</h2>
            <p className="text-[12px] text-ink-3">Everything happening on the network, live</p>
          </div>
        </div>
      )}
      <div className={`px-4 pb-2 ${titled ? "" : "pt-4"}`}>
        <div className="relative grid grid-cols-4 rounded-[10px] bg-field p-0.5 shadow-hairline" role="tablist">
          <span
            aria-hidden
            className="absolute top-0.5 bottom-0.5 rounded-[8px] bg-surface shadow-btn"
            style={{
              left: `calc(${idx * 25}% + 2px)`,
              width: "calc(25% - 4px)",
              transition: "left 260ms var(--ease-out-strong)",
            }}
          />
          {FILTERS.map((f) => (
            <button
              key={f.id}
              type="button"
              role="tab"
              data-sound="select"
              aria-selected={filter === f.id}
              onClick={() => setFilter(f.id)}
              className={`relative z-10 h-7 rounded-[8px] text-[12px] font-medium transition-colors ${
                filter === f.id ? "text-ink" : "text-ink-3 hover:text-ink-2"
              }`}
            >
              {f.label}
            </button>
          ))}
        </div>
      </div>
      <div className="fade-mask-b min-h-0 flex-1 overflow-y-auto px-2 pb-8">
        {items.length === 0 ? (
          <div className="flex flex-col items-center gap-1 px-6 py-14 text-center">
            <span className="text-[13px] font-medium text-ink">{query ? "Nothing matches" : "Quiet so far"}</span>
            <span className="text-[12px] text-ink-3">
              {query ? "Try another search." : "Messages, state changes and agents coming and going will show up here."}
            </span>
          </div>
        ) : (
          <GlideMenu className="flex flex-col" highlightClassName="inset-x-0 rounded-[10px] bg-hover">
            {items.map((a) => (
              <Row
                key={a.seq}
                a={a}
                agents={agents}
                live={liveSeqs.has(a.seq)}
                now={now}
                onAgent={onAgent}
                onThread={onThread}
              />
            ))}
          </GlideMenu>
        )}
      </div>
    </section>
  )
}
