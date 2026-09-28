import type { Activity, Conversation, Message, State } from "./types"

export interface EventHandlers {
  onOpen(): void
  onState(s: State): void
  onActivity(a: Activity): void
  onMessage(m: { peer: string; th: string; message: Message }): void
  /** the stream broke; the caller reconnects */
  onError(): void
}

/** Where the dashboard's data comes from: the host's `awp web`, or the
 *  development mock. */
export interface Transport {
  state(): Promise<State>
  activity(limit: number): Promise<Activity[]>
  thread(peer: string, th: string): Promise<Conversation>
  events(h: EventHandlers): () => void
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Accept: "application/json" } })
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`
    try {
      const body = await res.json()
      if (body?.error) msg = body.error
    } catch {
      // not JSON: keep the status line
    }
    throw new Error(msg)
  }
  return res.json() as Promise<T>
}

function parse<T>(ev: MessageEvent, fn: (v: T) => void) {
  try {
    fn(JSON.parse(ev.data) as T)
  } catch {
    // a malformed event is dropped, not fatal
  }
}

export const http: Transport = {
  state: () => get<State>("/api/state"),
  activity: async (limit) => (await get<{ items: Activity[] }>(`/api/activity?limit=${limit}`)).items ?? [],
  thread: (peer, th) =>
    get<Conversation>(`/api/thread?peer=${encodeURIComponent(peer)}&th=${encodeURIComponent(th)}`),
  events(h) {
    const es = new EventSource("/api/events")
    es.onopen = () => h.onOpen()
    es.addEventListener("state", (e) => parse<State>(e as MessageEvent, h.onState))
    es.addEventListener("activity", (e) => parse<Activity>(e as MessageEvent, h.onActivity))
    es.addEventListener("message", (e) =>
      parse<{ peer: string; th: string; message: Message }>(e as MessageEvent, h.onMessage),
    )
    es.onerror = () => {
      es.close()
      h.onError()
    }
    return () => es.close()
  },
}
