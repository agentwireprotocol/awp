// The dashboard's only loaders, from loading.dev: Ripple while waiting for
// the network to answer, Comet while something specific loads.
import { Comet, Ripple } from "loading-dev"

export function Loading({ label, kind = "comet", size }: { label?: string; kind?: "comet" | "ripple"; size?: number }) {
  return (
    <div role="status" className="flex flex-col items-center gap-3 text-ink-3">
      {kind === "ripple" ? <Ripple size={size ?? 44} color="var(--accent)" /> : <Comet size={size ?? 26} color="var(--ink-2)" />}
      {label && <span className="text-[13px] font-medium">{label}</span>}
    </div>
  )
}
