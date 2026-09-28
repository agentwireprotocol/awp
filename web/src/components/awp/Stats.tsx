import { lazy, Suspense } from "react"

import { useNow } from "@/lib/store"
import type { Activity, State } from "@/lib/types"

const PulseChart = lazy(() => import("./PulseChart"))

function Eq() {
  return (
    <span aria-hidden className="ml-1 inline-flex h-3.5 items-end gap-[2px]">
      {[0, 180, 90, 260].map((d, i) => (
        <span
          key={i}
          className="w-[3px] origin-bottom rounded-full bg-accent"
          style={{ height: "100%", animation: `eq-bounce 900ms ease-in-out ${d}ms infinite` }}
        />
      ))}
    </span>
  )
}

function Stat({ label, value, sub, extra }: { label: string; value: number | string; sub?: string; extra?: React.ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col justify-between rounded-[14px] bg-surface p-4 shadow-card">
      <span className="flex items-center text-[12px] font-medium text-ink-3">
        {label}
        {extra}
      </span>
      <span className="mt-3 flex items-baseline gap-1.5">
        <span className="text-[30px] leading-none font-semibold tracking-[-0.035em] text-ink tabular-nums">{value}</span>
        {sub && <span className="truncate text-[12px] text-ink-3">{sub}</span>}
      </span>
    </div>
  )
}

export function Stats({ state, activity }: { state: State; activity: Activity[] }) {
  const s = state.stats
  const now = useNow(2000)
  const perMin = activity.filter((a) => now - Date.parse(a.at) < 60_000).length
  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-4 xl:grid-cols-[repeat(4,minmax(0,1fr))_minmax(0,1.6fr)]">
      <Stat label="Agents" value={s.agents} sub={`${s.up} up`} />
      <Stat label="Threads" value={s.active} sub={`active of ${s.threads}`} />
      <Stat label="Working" value={s.working} extra={s.working > 0 ? <Eq /> : undefined} />
      <Stat label="Waiting" value={s.waiting} sub={s.waiting ? "for a reply or review" : undefined} />
      <div className="col-span-2 flex min-w-0 flex-col overflow-hidden rounded-[14px] bg-surface shadow-card md:col-span-4 xl:col-span-1">
        <div className="flex items-baseline justify-between px-4 pt-4">
          <span className="text-[12px] font-medium text-ink-3">Network pulse</span>
          <span className="text-[12px] text-ink-3 tabular-nums">
            <span className="font-semibold text-ink">{perMin}</span> events/min
          </span>
        </div>
        <div className="h-[68px] min-w-0">
          <Suspense fallback={null}>
            <PulseChart activity={activity} />
          </Suspense>
        </div>
      </div>
    </div>
  )
}
