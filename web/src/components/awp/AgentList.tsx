import { Shimmer } from "@/components/atoms/Shimmer"
import GlideMenu from "@/components/primitives/GlideMenu"
import { activityText, agentName, ago, quietMinutes, splitName } from "@/lib/format"
import { useNow } from "@/lib/store"
import type { Agent } from "@/lib/types"

import { ModelTag } from "./chips"
import { Orb } from "./Orb"

const DOT: Record<Agent["status"], string> = {
  self: "var(--ink)",
  connected: "var(--green)",
  online: "var(--accent)",
  stale: "var(--ink-3)",
  offline: "var(--line-strong)",
}

export function AgentList({ agents, selected, onSelect }: { agents: Agent[]; selected?: string; onSelect: (k: string) => void }) {
  const now = useNow(10000)
  return (
    <GlideMenu className="flex flex-col" highlightClassName="inset-x-0 rounded-[10px] bg-hover">
      {agents.map((a) => {
        const { who, host } = splitName(agentName(a))
        const dim = a.status === "stale" || a.status === "offline"
        return (
          <button
            key={a.key}
            type="button"
            data-menu-row
            onClick={() => onSelect(a.key)}
            className={`relative z-10 flex w-full items-center gap-3 rounded-[10px] px-2.5 py-2.5 text-left ${selected === a.key ? "bg-hover" : ""}`}
          >
            <span className="relative">
              <Orb agentKey={a.key} size={34} working={a.working} dim={dim} harness={a.harness} />
              <span className="absolute -top-0.5 -right-0.5 size-2.5 rounded-full ring-2 ring-surface" style={{ background: DOT[a.status] }} />
            </span>
            <span className="min-w-0 flex-1">
              <span className="flex items-baseline gap-1.5">
                <span className="truncate text-[13.5px] font-medium text-ink">{who}</span>
                {host && <span className="truncate text-[12px] text-ink-3">@{host}</span>}
                {a.model && <ModelTag model={a.model} className="ml-0.5 min-w-0 shrink" />}
              </span>
              <span className="block truncate text-[12px] text-ink-3">
                {a.host && <span className="font-mono text-[11.5px]">{a.host} · </span>}
                {a.status === "self" ? "this host" : a.about || (a.direct ? "connected to this host" : "via gossip")}
              </span>
            </span>
            <span className="flex shrink-0 flex-col items-end gap-0.5 text-[11.5px]">
              {a.unreachable ? (
                <span className="font-medium text-orange" title={a.unreachable}>
                  can't reach
                </span>
              ) : quietMinutes(a, now) !== undefined ? (
                <span className="font-medium text-orange" title="Said it is working, but has done nothing through awp since">
                  no activity {quietMinutes(a, now)}m
                </span>
              ) : a.working ? (
                <span className="font-medium">
                  <Shimmer>working</Shimmer>
                </span>
              ) : a.waiting ? (
                <span className="font-medium text-orange">waiting</span>
              ) : (
                <span className="text-ink-3">{dim ? "quiet" : a.active ? `${a.active} active` : "idle"}</span>
              )}
              <span className="text-ink-3 tabular-nums" title={activityText(a, now) || undefined}>
                {a.listening ? "listening" : a.last_active ? ago(a.last_active, now) : a.status === "self" ? "" : ago(a.seen, now)}
              </span>
            </span>
          </button>
        )
      })}
    </GlideMenu>
  )
}
