import { Liveline, type LivelinePoint } from "liveline"
import { useEffect, useMemo, useRef, useState } from "react"

import { useTheme } from "@/components/theme-provider"
import type { Activity } from "@/lib/types"

const WINDOW = 300 // seconds shown
const SPAN = 60 // the rate is events in the last minute

function rateAt(times: number[], t: number): number {
  let n = 0
  for (let i = times.length - 1; i >= 0; i--) {
    if (times[i] > t) continue
    if (times[i] <= t - SPAN) break
    n++
  }
  return n
}

function useDark(): boolean {
  const { theme } = useTheme()
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"))
  useEffect(() => {
    const el = document.documentElement
    const mo = new MutationObserver(() => setDark(el.classList.contains("dark")))
    mo.observe(el, { attributes: true, attributeFilter: ["class"] })
    return () => mo.disconnect()
  }, [theme])
  return dark
}

/** Network activity per minute, drawn live. */
export default function PulseChart({ activity }: { activity: Activity[] }) {
  const dark = useDark()
  const times = useMemo(
    () => activity.map((a) => Date.parse(a.at) / 1000).filter((t) => !Number.isNaN(t)).sort((a, b) => a - b),
    [activity],
  )
  const timesRef = useRef(times)
  useEffect(() => {
    timesRef.current = times
  }, [times])

  const [points, setPoints] = useState<LivelinePoint[]>(() => {
    const now = Date.now() / 1000
    const out: LivelinePoint[] = []
    for (let t = now - WINDOW; t <= now; t += 5) out.push({ time: t, value: rateAt(times, t) })
    return out
  })

  useEffect(() => {
    const id = setInterval(() => {
      const now = Date.now() / 1000
      setPoints((ps) => [...ps.filter((p) => p.time > now - WINDOW - 10), { time: now, value: rateAt(timesRef.current, now) }])
    }, 1000)
    return () => clearInterval(id)
  }, [])

  const value = points.at(-1)?.value ?? 0
  return (
    <Liveline
      data={points}
      value={value}
      theme={dark ? "dark" : "light"}
      color={dark ? "oklch(0.78 0.12 255)" : "oklch(0.55 0.19 258)"}
      window={WINDOW}
      grid={false}
      badge={false}
      fill
      pulse
      scrub={false}
      lineWidth={2}
      padding={{ top: 8, right: 4, bottom: 4, left: 4 }}
      formatValue={(v) => `${Math.round(v)}/min`}
    />
  )
}
