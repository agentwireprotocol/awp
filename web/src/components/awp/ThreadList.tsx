import { Share2 } from "lucide-react"
import { Comet } from "loading-dev"

import { Badge, CheckMark, SpinnerRing, XMark } from "@/components/primitives/TaskRows"
import GlideMenu from "@/components/primitives/GlideMenu"
import { agentName, ago, isEnded } from "@/lib/format"
import { useNow } from "@/lib/store"
import type { Agent, Thread } from "@/lib/types"

import { AgentChip, StatePill } from "./chips"

function Lead({ t }: { t: Thread }) {
  const s = [t.a_state, t.b_state]
  if (s.includes("failed")) return <Badge tone="red"><XMark /></Badge>
  if (s.every(isEnded)) {
    return s.includes("done") ? (
      <Badge tone="green"><CheckMark /></Badge>
    ) : (
      <span className="flex size-5.5 items-center justify-center rounded-full bg-field text-ink-3 shadow-hairline"><CheckMark /></span>
    )
  }
  if (s.includes("working"))
    return (
      <span className="flex size-6 shrink-0 items-center justify-center">
        <Comet size={20} color="var(--accent)" />
      </span>
    )
  if (s.includes("waiting"))
    return (
      <SpinnerRing>
        <span className="size-1.5 rounded-full bg-orange" />
      </SpinnerRing>
    )
  return <SpinnerRing />
}

const LockIcon = (
  <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    <rect x="4" y="11" width="16" height="10" rx="2.5" />
    <path d="M8 11V8a4 4 0 0 1 8 0v3" />
  </svg>
)

export function ThreadList({
  threads,
  agents,
  selected,
  onOpen,
  onAgent,
  limit,
}: {
  threads: Thread[]
  agents: Map<string, Agent>
  selected?: string
  onOpen: (t: Thread) => void
  onAgent: (k: string) => void
  limit?: number
}) {
  const now = useNow(10000)
  const shown = limit ? threads.slice(0, limit) : threads
  if (!threads.length) {
    return (
      <div className="flex flex-col items-center gap-1 px-6 py-12 text-center">
        <span className="text-[13px] font-medium text-ink">No threads yet</span>
        <span className="max-w-72 text-[12px] text-ink-3">
          When agents start talking, every conversation on the network shows up here with both sides' states.
        </span>
      </div>
    )
  }
  return (
    <GlideMenu className="flex flex-col" highlightClassName="inset-x-0 rounded-[10px] bg-hover">
      {shown.map((t, i) => (
        <div
          key={t.id}
          data-menu-row
          role="button"
          tabIndex={0}
          onClick={() => onOpen(t)}
          onKeyDown={(e) => e.key === "Enter" && onOpen(t)}
          className={`relative z-10 flex cursor-pointer items-start gap-3 rounded-[10px] px-2.5 py-3 ${
            selected === t.id ? "bg-hover" : ""
          }`}
          style={{ animation: `fade-up 360ms var(--ease-out-strong) ${Math.min(i, 8) * 40}ms both` }}
        >
          <span className="mt-px">
            <Lead t={t} />
          </span>
          <div className="min-w-0 flex-1">
            <div className="flex items-baseline gap-2">
              <span className={`min-w-0 flex-1 truncate text-[13.5px] font-medium ${t.a_state === "closed" && t.b_state === "closed" ? "text-ink-2" : "text-ink"}`}>
                {t.subject || t.th}
              </span>
              {!t.local && t.shared_by && (
                <span className="shrink-0 text-ink-3" title={`Shared with this host by ${agentName(agents.get(t.shared_by), "one of its agents")}`}>
                  <Share2 size={11} />
                </span>
              )}
              {!t.local && !t.shared_by && (
                <span className="shrink-0 text-ink-3" title="Private to the two agents; this host sees its subject and states through gossip">
                  {LockIcon}
                </span>
              )}
              {!!t.unread && (
                <span className="shrink-0 rounded-full bg-accent px-1.5 text-[10.5px] font-semibold text-white tabular-nums">{t.unread}</span>
              )}
              <time className="shrink-0 text-[11.5px] text-ink-3 tabular-nums" dateTime={t.updated}>
                {ago(t.updated, now)}
              </time>
            </div>
            <div className="mt-1.5 flex flex-wrap items-center gap-x-1.5 gap-y-1">
              <span className="inline-flex min-w-0 items-center gap-1">
                <AgentChip agentKey={t.a} agent={agents.get(t.a)} onClick={() => onAgent(t.a)} />
                <StatePill state={t.a_state} />
              </span>
              <span className="text-[11px] text-ink-3" aria-hidden>
                ⇄
              </span>
              <span className="inline-flex min-w-0 items-center gap-1">
                <AgentChip agentKey={t.b} agent={agents.get(t.b)} onClick={() => onAgent(t.b)} />
                <StatePill state={t.b_state} />
              </span>
            </div>
          </div>
        </div>
      ))}
    </GlideMenu>
  )
}
