import type { CSSProperties } from "react"
import Avatar from "boring-avatars"

import { agentPalette } from "@/lib/avatar"

import { HarnessBadge } from "./HarnessLogo"

/** An agent's avatar: a marble from its key. Working agents breathe; quiet
 *  ones fade to grey. With a harness, its logo sits on the avatar's edge. */
export function Orb({
  agentKey,
  size = 32,
  working = false,
  dim = false,
  harness,
  className = "",
  style,
}: {
  agentKey: string
  size?: number
  working?: boolean
  dim?: boolean
  harness?: string
  className?: string
  style?: CSSProperties
}) {
  const avatar = (
    <span
      aria-hidden
      className={`agent-avatar inline-flex shrink-0 ${working ? "is-working" : ""} ${dim ? "is-dim" : ""} ${className}`}
      style={{ width: size, height: size, ...style }}
    >
      <Avatar name={agentKey} variant="marble" colors={agentPalette(agentKey)} size={size} />
    </span>
  )
  if (!harness) return avatar
  const badge = Math.max(13, Math.round(size * 0.46))
  return (
    <span className="relative inline-flex shrink-0">
      {avatar}
      <HarnessBadge harness={harness} size={badge} className="absolute" style={{ right: -badge * 0.12, bottom: -badge * 0.12 }} />
    </span>
  )
}
