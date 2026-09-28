import { EntityChip } from "@/components/atoms/EntityChip"
import { ValuePill } from "@/components/atoms/ValuePill"
import { agentName, stateTone } from "@/lib/format"
import type { Agent, ThreadState } from "@/lib/types"

import Avatar from "boring-avatars"

import { agentPalette } from "@/lib/avatar"

import { HarnessLogo } from "./HarnessLogo"

/** An agent inline: its orb as the monogram, then its name. */
export function AgentChip({
  agentKey,
  agent,
  className = "",
  onClick,
}: {
  agentKey: string
  agent?: Agent
  className?: string
  onClick?: () => void
}) {
  const name = agentName(agent, agentKey.slice(8, 18))
  // Known harnesses show their logo on a plain disc; others a tiny marble.
  const chip = (
    <EntityChip
      name={name}
      color={agent?.harness ? "var(--card)" : "transparent"}
      monogram={
        agent?.harness ? (
          <HarnessLogo harness={agent.harness} size={11} />
        ) : (
          <Avatar name={agentKey} variant="marble" colors={agentPalette(agentKey)} size={16} />
        )
      }
      className={`mx-0 max-w-full [&>span:last-child]:truncate ${className}`}
    />
  )
  if (!onClick) return chip
  return (
    <button
      type="button"
      onClick={(e) => {
        e.stopPropagation()
        onClick()
      }}
      className="inline-flex max-w-full min-w-0 rounded-full transition-[filter] hover:brightness-[0.97] dark:hover:brightness-110"
    >
      {chip}
    </button>
  )
}

const DOT: Record<string, string> = {
  accent: "var(--accent)",
  orange: "var(--orange)",
  green: "var(--green)",
  red: "var(--red)",
  neutral: "var(--ink-3)",
}

/** A thread state as a pill with a status dot. Working pulses. */
export function StatePill({ state, className = "" }: { state?: ThreadState; className?: string }) {
  if (!state) return null
  const tone = stateTone(state)
  return (
    <ValuePill tone={tone} className={`mx-0 gap-1 ${className}`}>
      <span className="relative flex size-1.5">
        {state === "working" && (
          <span className="absolute inset-0 rounded-full" style={{ background: DOT[tone], animation: "orb-ping 1.4s ease-out infinite" }} />
        )}
        <span className="relative size-1.5 rounded-full" style={{ background: DOT[tone] }} />
      </span>
      {state}
    </ValuePill>
  )
}

/** The model an agent runs on, as a small monospace tag. */
export function ModelTag({ model, className = "" }: { model?: string; className?: string }) {
  if (!model) return null
  return (
    <span
      title={`Runs on ${model}`}
      className={`inline-flex max-w-full items-center truncate rounded-[5px] bg-field px-1.5 py-px font-mono text-[10.5px] leading-[1.5] text-ink-2 shadow-hairline ${className}`}
    >
      {model}
    </span>
  )
}
