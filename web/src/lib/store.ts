import { useEffect, useState, useSyncExternalStore } from "react"

import type { Transport } from "./api"
import type { Activity, Message, State } from "./types"

export type Conn = "connecting" | "live" | "reconnecting"

export interface Snapshot {
  state?: State
  activity: Activity[] // oldest first
  conn: Conn
  error?: string
  /** when the stream last broke, for "reconnecting in…" */
  retryAt?: number
}

const KEEP = 1000

type ActivityListener = (a: Activity) => void
type MessageListener = (m: { peer: string; th: string; message: Message }) => void

/** Holds the dashboard's data and keeps it live: one state snapshot, the
 *  network-wide activity log, and a stream that reconnects with backoff. */
export class AWPStore {
  private snap: Snapshot = { activity: [], conn: "connecting" }
  private listeners = new Set<() => void>()
  private activityListeners = new Set<ActivityListener>()
  private messageListeners = new Set<MessageListener>()
  private stop?: () => void
  private timer?: ReturnType<typeof setTimeout>
  private attempt = 0
  private started = false
  readonly transport: Transport

  constructor(transport: Transport) {
    this.transport = transport
  }

  subscribe = (fn: () => void) => {
    this.listeners.add(fn)
    return () => {
      this.listeners.delete(fn)
    }
  }

  getSnapshot = () => this.snap

  private set(patch: Partial<Snapshot>) {
    this.snap = { ...this.snap, ...patch }
    for (const fn of this.listeners) fn()
  }

  /** Called for every activity item that arrives live (not the backlog). */
  onActivity(fn: ActivityListener) {
    this.activityListeners.add(fn)
    return () => {
      this.activityListeners.delete(fn)
    }
  }

  onMessage(fn: MessageListener) {
    this.messageListeners.add(fn)
    return () => {
      this.messageListeners.delete(fn)
    }
  }

  start() {
    if (this.started) return
    this.started = true
    this.connect()
  }

  private merge(items: Activity[]) {
    if (!items.length) return
    const bySeq = new Map(this.snap.activity.map((a) => [a.seq, a]))
    for (const a of items) bySeq.set(a.seq, a)
    const all = [...bySeq.values()].sort((a, b) => a.seq - b.seq)
    this.set({ activity: all.slice(-KEEP) })
  }

  private connect() {
    this.stop?.()
    const t = this.transport
    // Load the backlog alongside the stream; items that arrive on both are
    // merged by seq.
    t.state()
      .then((state) => this.set({ state, error: undefined }))
      .catch((e: Error) => this.set({ error: e.message }))
    t.activity(KEEP)
      .then((items) => this.merge(items))
      .catch(() => {})
    this.stop = t.events({
      onOpen: () => {
        this.attempt = 0
        this.set({ conn: "live", error: undefined, retryAt: undefined })
      },
      onState: (state) => this.set({ state }),
      onActivity: (a) => {
        const last = this.snap.activity.at(-1)
        if (last && a.seq <= last.seq && this.snap.activity.some((x) => x.seq === a.seq)) return
        this.merge([a])
        for (const fn of this.activityListeners) fn(a)
      },
      onMessage: (m) => {
        for (const fn of this.messageListeners) fn(m)
      },
      onError: () => this.retry(),
    })
  }

  private retry() {
    this.stop?.()
    this.stop = undefined
    const delay = Math.min(15000, 1000 * 2 ** this.attempt) * (0.8 + Math.random() * 0.4)
    this.attempt++
    this.set({ conn: "reconnecting", retryAt: Date.now() + delay })
    clearTimeout(this.timer)
    this.timer = setTimeout(() => this.connect(), delay)
  }

  /** Try again right away (the "retry now" button). */
  reconnect() {
    clearTimeout(this.timer)
    this.attempt = 0
    this.connect()
  }
}

let store: AWPStore | undefined

export function setStore(s: AWPStore) {
  store = s
}

export function getStore(): AWPStore {
  if (!store) throw new Error("awp store not initialized")
  return store
}

export function useAWP(): Snapshot {
  const s = getStore()
  return useSyncExternalStore(s.subscribe, s.getSnapshot)
}

/** A clock that ticks every `ms`, for relative times. */
export function useNow(ms = 5000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms)
    return () => clearInterval(t)
  }, [ms])
  return now
}
