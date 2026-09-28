import { Comet, Ripple } from "loading-dev"

import { getStore, type Conn } from "@/lib/store"

export function Wordmark() {
  return (
    <span className="flex items-center gap-2">
      <span
        aria-hidden
        className="orb size-[22px]"
        style={{ "--o1": "oklch(0.92 0.07 60)", "--o2": "oklch(0.7 0.15 290)", "--o3": "oklch(0.84 0.1 220)" } as React.CSSProperties}
      />
      <span className="text-[17px] font-semibold tracking-[-0.03em] text-ink">awp</span>
    </span>
  )
}

export function Connection({ conn }: { conn: Conn }) {
  if (conn === "live") {
    return (
      <span className="inline-flex h-8 items-center gap-2 rounded-full bg-surface pr-3 pl-2 text-[12.5px] font-medium text-ink-2 shadow-hairline" title="Streaming live from the host">
        <Ripple size={16} color="var(--green)" />
        Live
      </span>
    )
  }
  if (conn === "connecting") {
    return (
      <span className="inline-flex h-8 items-center gap-2 rounded-full bg-surface pr-3 pl-2 text-[12.5px] font-medium text-ink-3 shadow-hairline">
        <Comet size={14} color="var(--ink-3)" />
        Connecting
      </span>
    )
  }
  return (
    <button
      type="button"
      onClick={() => getStore().reconnect()}
      title="The stream dropped; retrying with backoff. Click to retry now."
      className="inline-flex h-8 items-center gap-2 rounded-full bg-orange-tint pr-3 pl-2 text-[12.5px] font-medium text-orange shadow-[0_0_0_1px_color-mix(in_oklch,var(--orange)_28%,transparent)] transition-colors hover:brightness-95"
    >
      <Comet size={14} color="var(--orange)" />
      Reconnecting
    </button>
  )
}
