import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react"

import { Shimmer } from "@/components/atoms/Shimmer"
import { agentName, splitName, quietMinutes } from "@/lib/format"
import { agentColor } from "@/lib/avatar"
import { getStore } from "@/lib/store"
import type { Activity, Agent, State } from "@/lib/types"

import { Orb } from "./Orb"
import { ModelTag } from "./chips"

type Pt = { x: number; y: number }

/** Radial layout: the host in the middle, its peers on the first ring, and
 *  everyone else fanned out behind the agent they are reached through. */
function layout(state: State, w: number, h: number): Map<string, Pt> {
  const keys = new Set(state.agents.map((a) => a.key))
  const adj = new Map<string, string[]>()
  const add = (a: string, b: string) => {
    if (!keys.has(a) || !keys.has(b)) return
    adj.set(a, [...(adj.get(a) ?? []), b])
  }
  for (const l of state.links) {
    add(l.a, l.b)
    add(l.b, l.a)
  }
  // BFS tree from the host, preferring the route presence says it came by
  const parent = new Map<string, string>()
  const depth = new Map<string, number>([[state.self, 0]])
  const order = [state.self]
  for (let i = 0; i < order.length; i++) {
    const cur = order[i]
    for (const n of (adj.get(cur) ?? []).sort()) {
      if (depth.has(n)) continue
      depth.set(n, depth.get(cur)! + 1)
      parent.set(n, cur)
      order.push(n)
    }
  }
  for (const a of state.agents) {
    if (depth.has(a.key)) continue
    const p = a.via && depth.has(a.via) ? a.via : state.self
    depth.set(a.key, depth.get(p)! + 1)
    parent.set(a.key, p)
    order.push(a.key)
  }
  const children = new Map<string, string[]>()
  for (const k of order.slice(1)) {
    const p = parent.get(k)!
    children.set(p, [...(children.get(p) ?? []), k])
  }
  const leaves = new Map<string, number>()
  const count = (k: string): number => {
    const c = children.get(k) ?? []
    const n = c.length ? c.reduce((s, x) => s + count(x), 0) : 1
    leaves.set(k, n)
    return n
  }
  count(state.self)
  const maxDepth = Math.max(1, ...depth.values())
  const cx = w / 2
  const cy = h / 2
  // rings spread over the whole card, outer rings a little closer together
  const R = Math.max(80, w / 2 - 80)
  const RY = Math.max(60, h / 2 - 66)
  const ring = (d: number) => Math.pow(d / maxDepth, 0.8)
  const pos = new Map<string, Pt>([[state.self, { x: cx, y: cy }]])
  const place = (k: string, from: number, to: number) => {
    const c = children.get(k) ?? []
    const total = c.reduce((s, x) => s + leaves.get(x)!, 0)
    let a = from
    for (const ch of c) {
      const span = ((to - from) * leaves.get(ch)!) / total
      const mid = a + span / 2
      const d = depth.get(ch)!
      pos.set(ch, { x: cx + Math.cos(mid) * R * ring(d), y: cy + Math.sin(mid) * RY * ring(d) })
      place(ch, a, a + span)
      a += span
    }
  }
  place(state.self, -Math.PI * 0.9, Math.PI * 1.1)

  // Relax: nodes push apart until their labels clear each other, links
  // pull their ends together, the host stays put. Deterministic, so the
  // picture only moves when the network does.
  const ks = [...pos.keys()]
  const edges = state.links.filter((l) => pos.has(l.a) && pos.has(l.b))
  const minX = 80
  const maxX = w - 80
  const minY = 64
  const maxY = h - 92
  for (let it = 0; it < 240; it++) {
    const cool = 1 - it / 240
    for (let i = 0; i < ks.length; i++) {
      for (let j = i + 1; j < ks.length; j++) {
        const p = pos.get(ks[i])!
        const q = pos.get(ks[j])!
        let dx = q.x - p.x
        let dy = (q.y - p.y) * 1.35 // labels make nodes taller than wide
        let d = Math.hypot(dx, dy)
        if (d < 0.01) {
          dx = 1
          dy = 0
          d = 1
        }
        const want = w < 520 ? 118 : 150
        if (d >= want) continue
        const push = ((want - d) / d) * 0.5 * cool
        const mx = dx * push
        const my = (dy / 1.35) * push
        if (ks[i] !== state.self) {
          p.x -= mx
          p.y -= my
        }
        if (ks[j] !== state.self) {
          q.x += mx
          q.y += my
        }
      }
    }
    for (const l of edges) {
      const p = pos.get(l.a)!
      const q = pos.get(l.b)!
      const dx = q.x - p.x
      const dy = q.y - p.y
      const d = Math.hypot(dx, dy) || 1
      const pull = ((d - 190) / d) * 0.04 * cool
      if (l.a !== state.self) {
        p.x += dx * pull
        p.y += dy * pull
      }
      if (l.b !== state.self) {
        q.x -= dx * pull
        q.y -= dy * pull
      }
    }
    for (const k of ks) {
      const p = pos.get(k)!
      p.x = Math.min(maxX, Math.max(minX, p.x))
      p.y = Math.min(maxY, Math.max(minY, p.y))
    }
  }
  // Fit the picture to the card: stretch a compact network to use the
  // space (up to a point), and centre it.
  if (ks.length > 1) {
    const xs = ks.map((k) => pos.get(k)!.x)
    const ys = ks.map((k) => pos.get(k)!.y)
    const [x0, x1, y0, y1] = [Math.min(...xs), Math.max(...xs), Math.min(...ys), Math.max(...ys)]
    const sx = x1 - x0 > 1 ? Math.min(2.2, (maxX - minX) / (x1 - x0)) : 1
    const sy = y1 - y0 > 1 ? Math.min(2.2, (maxY - minY) / (y1 - y0)) : 1
    const s = Math.min(sx, sy * 1.6, Math.max(sx, sy))
    const scx = Math.min(sx, s * 1.4)
    const scy = Math.min(sy, s)
    const mx = (x0 + x1) / 2
    const my = (y0 + y1) / 2
    for (const k of ks) {
      const p = pos.get(k)!
      p.x = (minX + maxX) / 2 + (p.x - mx) * scx
      p.y = (minY + maxY) / 2 + (p.y - my) * scy
    }
  }
  return pos
}

// The midpoint of curve(p, q), where a link's latency is labelled.
function curveMid(p: Pt, q: Pt): Pt {
  const bend = 0.08
  return { x: (p.x + q.x) / 2 - ((q.y - p.y) * bend) / 2, y: (p.y + q.y) / 2 + ((q.x - p.x) * bend) / 2 }
}

function curve(p: Pt, q: Pt): string {
  const mx = (p.x + q.x) / 2
  const my = (p.y + q.y) / 2
  const dx = q.x - p.x
  const dy = q.y - p.y
  // a gentle, consistent bend
  const bend = 0.08
  return `M${p.x},${p.y} Q${mx - dy * bend},${my + dx * bend} ${q.x},${q.y}`
}

interface Pulse {
  id: number
  path: string
  color: string
  dur: number
}

const PULSE_COLOR: Partial<Record<Activity["kind"], string>> = {
  msg: "var(--ink)",
  error: "var(--red)",
}

function statusLine(a: Agent): { text: string; tone: string; shimmer?: boolean } {
  if (a.status === "stale") return { text: "quiet", tone: "text-ink-3" }
  if (a.unreachable) return { text: "can't reach", tone: "text-orange" }
  if (a.status === "offline") return { text: "offline", tone: "text-ink-3" }
  const quiet = quietMinutes(a)
  if (quiet !== undefined) return { text: `no activity ${quiet}m`, tone: "text-orange" }
  if (a.working) return { text: "working", tone: "", shimmer: true }
  if (a.waiting) return { text: "waiting", tone: "text-orange" }
  if (a.active) return { text: `${a.active} active`, tone: "text-ink-2" }
  return { text: "idle", tone: "text-ink-3" }
}

export function NetworkGraph({
  state,
  selected,
  onSelect,
}: {
  state: State
  selected?: string
  onSelect: (key: string) => void
}) {
  const ref = useRef<HTMLDivElement>(null)
  const [size, setSize] = useState({ w: 800, h: 440 })
  const [pulses, setPulses] = useState<Pulse[]>([])
  const [pings, setPings] = useState<Record<string, number>>({})
  const [hover, setHover] = useState<string>()

  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const ro = new ResizeObserver(([e]) => setSize({ w: e.contentRect.width, h: e.contentRect.height }))
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  const pos = useMemo(() => layout(state, size.w, size.h), [state, size.w, size.h])
  const posRef = useRef(pos)
  useEffect(() => {
    posRef.current = pos
  }, [pos])

  // Live activity travels along the network as a pulse, and lands as a
  // ripple on the agent it reached.
  useEffect(() => {
    let id = 0
    const timers = new Set<ReturnType<typeof setTimeout>>()
    const later = (fn: () => void, ms: number) => {
      const t = setTimeout(() => {
        timers.delete(t)
        fn()
      }, ms)
      timers.add(t)
    }
    const ping = (k: string) => setPings((p) => ({ ...p, [k]: Date.now() }))
    const off = getStore().onActivity((a) => {
      const p = posRef.current
      const from = p.get(a.from)
      const to = a.to ? p.get(a.to) : undefined
      if (!from) return
      if (!to) {
        ping(a.from)
        return
      }
      const color =
        PULSE_COLOR[a.kind] ?? (a.kind === "state" ? agentColor(a.from) : "var(--accent)")
      const dist = Math.hypot(to.x - from.x, to.y - from.y)
      const dur = Math.min(1400, 450 + dist * 1.6)
      const pulse = { id: ++id, path: curve(from, to), color, dur }
      setPulses((ps) => [...ps.slice(-24), pulse])
      ping(a.from)
      later(() => ping(a.to!), dur)
      later(() => setPulses((ps) => ps.filter((x) => x.id !== pulse.id)), dur + 200)
    })
    return () => {
      off()
      timers.forEach(clearTimeout)
    }
  }, [])

  // links with a thread in progress carry a slow flow
  const busy = useMemo(() => {
    const s = new Set<string>()
    for (const t of state.threads) {
      if (t.a_state === "working" || t.b_state === "working") s.add(`${t.a}|${t.b}`)
    }
    return s
  }, [state.threads])
  const focus = hover ?? selected

  return (
    <div ref={ref} className="relative h-full w-full select-none">
      <svg className="absolute inset-0 h-full w-full overflow-visible" aria-hidden>
        <defs>
          <filter id="pulse-glow" x="-200%" y="-200%" width="500%" height="500%">
            <feGaussianBlur stdDeviation="3" />
          </filter>
        </defs>
        {state.links.map((l) => {
          const p = pos.get(l.a)
          const q = pos.get(l.b)
          if (!p || !q) return null
          const lit = focus && (l.a === focus || l.b === focus)
          const flowing = busy.has(`${l.a}|${l.b}`)
          const d = curve(p, q)
          return (
            <g key={`${l.a}|${l.b}`}>
              <path
                d={d}
                fill="none"
                stroke={lit ? "var(--ink-3)" : "var(--line-strong)"}
                strokeWidth={lit ? 1.6 : 1.2}
                strokeDasharray={l.up ? undefined : "3 5"}
                style={{ transition: "stroke 200ms, d 600ms var(--ease-out-strong)" }}
              />
              {l.up && !!l.rtt_ms && (
                <text
                  x={curveMid(p, q).x}
                  y={curveMid(p, q).y - 4}
                  textAnchor="middle"
                  className="fill-ink-3 font-mono text-[10px] [paint-order:stroke] [stroke-width:3px] [stroke:var(--surface)]"
                >
                  {l.rtt_ms} ms
                </text>
              )}
              {flowing && l.up && (
                <path
                  d={d}
                  fill="none"
                  stroke="var(--accent)"
                  strokeOpacity={0.9}
                  strokeWidth={2.8}
                  strokeLinecap="round"
                  strokeDasharray="0.01 11.99"
                  style={{ animation: "flow 1.2s linear infinite", transition: "d 600ms var(--ease-out-strong)" }}
                />
              )}
            </g>
          )
        })}
        {pulses.map((p) => (
          <g key={p.id}>
            <circle r={9} fill={p.color} opacity={0.5} filter="url(#pulse-glow)">
              <animateMotion dur={`${p.dur}ms`} path={p.path} fill="freeze" keyPoints="0;1" keyTimes="0;1" calcMode="spline" keySplines="0.4 0 0.2 1" />
            </circle>
            <circle r={3.8} fill={p.color} stroke="var(--surface)" strokeWidth={1.2}>
              <animateMotion dur={`${p.dur}ms`} path={p.path} fill="freeze" keyPoints="0;1" keyTimes="0;1" calcMode="spline" keySplines="0.4 0 0.2 1" />
            </circle>
          </g>
        ))}
      </svg>

      {state.agents.map((a) => {
        const p = pos.get(a.key)
        if (!p) return null
        const self = a.key === state.self
        const small = size.w < 520
        const sz = self ? (small ? 48 : 58) : small ? 36 : 44
        const st = statusLine(a)
        const { who, host } = splitName(agentName(a))
        const isSel = selected === a.key
        const dim = a.status === "stale" || a.status === "offline"
        return (
          <button
            key={a.key}
            type="button"
            onClick={() => onSelect(a.key)}
            onMouseEnter={() => setHover(a.key)}
            onMouseLeave={() => setHover(undefined)}
            aria-label={`${agentName(a)}, ${st.text}`}
            className="group absolute flex w-28 -translate-x-1/2 sm:w-36 flex-col items-center gap-1.5 rounded-2xl px-1 pt-1 pb-1.5 outline-none focus-visible:ring-2 focus-visible:ring-accent"
            style={{
              left: p.x,
              top: p.y - sz / 2 - 4,
              transition: "left 600ms var(--ease-out-strong), top 600ms var(--ease-out-strong)",
              animation: "rise-in 500ms var(--ease-out-strong) both",
            }}
          >
            <span className="relative" style={{ width: sz, height: sz }}>
              {pings[a.key] && (
                <span
                  key={pings[a.key]}
                  className="absolute inset-0 rounded-full"
                  style={{
                    boxShadow: `0 0 0 2px ${agentColor(a.key)}`,
                    animation: "orb-ping 900ms var(--ease-out-strong) both",
                  }}
                />
              )}
              <span
                className={`absolute -inset-[5px] rounded-full transition-all duration-300 ${
                  isSel ? "shadow-[0_0_0_2px_var(--ink)]" : "shadow-[0_0_0_0px_transparent] group-hover:shadow-[0_0_0_1px_var(--line-strong)]"
                }`}
              />
              <Orb agentKey={a.key} size={sz} working={a.working} dim={dim} harness={a.harness} className="transition-transform duration-300 group-hover:scale-[1.04]" />
              {self && (
                <span className="absolute -top-2.5 left-1/2 -translate-x-1/2 rounded-full bg-ink px-1.5 py-px text-[9.5px] font-semibold tracking-wide text-page uppercase shadow-btn">
                  host
                </span>
              )}
            </span>
            <span className="flex max-w-full flex-col items-center leading-tight">
              <span className="max-w-full truncate text-[12.5px] font-medium text-ink">{who}</span>
              {host && <span className="max-w-full truncate text-[11px] text-ink-3">@{host}</span>}
              {a.model && <ModelTag model={a.model} className="mt-0.5 max-w-[140px]" />}
              <span className={`mt-0.5 text-[11px] font-medium ${st.tone}`}>
                {st.shimmer ? <Shimmer>{st.text}</Shimmer> : st.text}
              </span>
            </span>
          </button>
        )
      })}
    </div>
  )
}
