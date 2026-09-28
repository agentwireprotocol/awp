import { Download, Share2 } from "lucide-react"
import { useEffect, useRef, useState } from "react"

import { StreamText } from "@/components/atoms/StreamText"
import { Loading } from "./Loading"
import { agentName, agoText, clock } from "@/lib/format"
import { Code, Markdown } from "@/lib/markdown"
import { getStore, useNow } from "@/lib/store"
import type { Agent, Conversation, Message, Part, State, Thread } from "@/lib/types"

import { AgentChip, StatePill } from "./chips"
import { CopyButton } from "./Copy"
import { Orb } from "./Orb"

function size(n?: number): string {
  if (n === undefined) return ""
  if (n < 1024) return `${n} B`
  if (n < 1 << 20) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / (1 << 20)).toFixed(1)} MB`
}

function PartView({ p, fresh }: { p: Part; fresh: boolean }) {
  switch (p.type) {
    case "text":
      return fresh && !p.text.includes("```") && p.text.length < 600 ? (
        <p className="whitespace-pre-wrap">
          <StreamText text={p.text} caret={false} charsPerTick={4} />
        </p>
      ) : (
        <Markdown text={p.text} />
      )
    case "code":
      return <Code text={p.text} lang={p.lang} />
    case "data":
      return <Code text={JSON.stringify(p.data, null, 2)} lang="json" name="data.json" />
    case "blob": {
      const image = !!p.url && IMAGE_TYPES.has((p.mime ?? "").split(";")[0].trim().toLowerCase())
      return (
        <div className="my-2 overflow-hidden rounded-card bg-surface shadow-card">
          {image && (
            <a href={p.url} target="_blank" rel="noreferrer" className="block border-b border-line bg-field" title={`Open ${p.name ?? "image"}`}>
              <img src={p.url} alt={p.name ?? "image"} loading="lazy" className="mx-auto block max-h-80 w-auto max-w-full object-contain" />
            </a>
          )}
          <div className="flex items-center gap-3 px-3 py-2.5">
          <span className="flex size-8 items-center justify-center rounded-[8px] bg-field text-ink-2 shadow-hairline">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z" /><path d="M14 3v6h6" /></svg>
          </span>
          <span className="min-w-0 flex-1">
            <span className="block truncate font-mono text-[12.5px] text-ink">{p.name || "file"}</span>
            <span className="text-[11.5px] text-ink-3">{[p.mime, size(p.size)].filter(Boolean).join(" · ")}</span>
          </span>
          {p.url ? (
            <a
              href={p.url}
              download={p.name || true}
              data-sound="select"
              className="inline-flex shrink-0 items-center gap-1 rounded-full bg-field px-2.5 py-1 text-[11.5px] font-medium text-ink-2 shadow-hairline transition-colors hover:text-ink"
            >
              <Download size={12} /> Download
            </a>
          ) : (
            p.status && <span className="rounded-full bg-field px-2 py-0.5 text-[11px] font-medium text-ink-2 shadow-hairline">{p.status}</span>
          )}
          </div>
        </div>
      )
    }
  }
}

// Images the server shows inline (internal/web: inlineImages).
const IMAGE_TYPES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp", "image/avif"])

function Bubble({ m, mine, agent, fresh }: { m: Message; mine: boolean; agent?: Agent; fresh: boolean }) {
  const who = agentName(agent, m.from.slice(8, 18))
  return (
    <div className={`flex gap-2.5 ${mine ? "flex-row-reverse" : ""}`} style={fresh ? { animation: "rise-in 420ms var(--ease-out-strong) both" } : undefined}>
      <Orb agentKey={m.from} size={26} harness={agent?.harness} className="mt-5" />
      <div className={`flex min-w-0 max-w-[88%] flex-col ${mine ? "items-end" : "items-start"}`}>
        <div className="mb-1 flex items-center gap-1.5 px-1 text-[11.5px] text-ink-3">
          <span className="font-medium text-ink-2">{who}</span>
          <time dateTime={m.at}>{clock(m.at)}</time>
          {mine && m.acked && (
            <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="var(--green)" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" aria-label="delivered"><path d="M20 6L9 17l-5-5" /></svg>
          )}
        </div>
        <div
          className={`min-w-0 max-w-full rounded-[16px] px-3.5 py-2.5 text-[14px] leading-[1.55] text-ink ${
            mine ? "rounded-tr-[6px] bg-field shadow-hairline" : "rounded-tl-[6px] bg-surface shadow-card"
          }`}
        >
          {(m.parts ?? []).map((p, i) => (
            <PartView key={i} p={p} fresh={fresh} />
          ))}
        </div>
      </div>
    </div>
  )
}

function Marker({ m, agent }: { m: Message; agent?: Agent }) {
  const who = agentName(agent, m.from.slice(8, 18))
  return (
    <div className="flex items-center gap-3 py-1 text-[12px] text-ink-3">
      <span className="h-px flex-1 bg-line" />
      <span className="flex min-w-0 flex-wrap items-center justify-center gap-1.5 text-center">
        <span className="font-medium text-ink-2">{who}</span>
        {m.kind === "state" ? (
          <>
            is <StatePill state={m.state} />
          </>
        ) : m.kind === "error" ? (
          <span className="text-red">error</span>
        ) : m.kind === "blob" ? (
          <span>sent a file</span>
        ) : (
          <span>{m.kind}</span>
        )}
        {m.note && <span className="text-ink-3">· {m.note}</span>}
      </span>
      <span className="h-px flex-1 bg-line" />
    </div>
  )
}

function Header({ thread, agents }: { thread: Thread; agents: Map<string, Agent> }) {
  const now = useNow(10000)
  const sharer = thread.shared_by ? agentName(agents.get(thread.shared_by), thread.shared_by.slice(8, 18)) : ""
  return (
    <header className="border-b border-line px-6 pt-8 pb-4 sm:pt-6">
      <p className="flex items-center gap-1.5 text-[12px] font-medium text-ink-3">
        {thread.local ? (
          "Conversation"
        ) : sharer ? (
          <>
            <Share2 size={12} /> Shared by {sharer}
          </>
        ) : (
          "Thread between other agents"
        )}
      </p>
      <h2 className="mt-1 pr-10 text-[21px] leading-tight font-semibold tracking-[-0.02em] text-balance text-ink">{thread.subject || thread.th}</h2>
      {/* The id every command takes (awp send --thread, state, private). */}
      <CopyButton text={thread.th} label={thread.th} className="mt-2 h-6 px-2 font-mono text-[11px]" />
      <div className="mt-3 flex flex-wrap items-center gap-x-2 gap-y-1.5">
        <span className="inline-flex items-center gap-1">
          <AgentChip agentKey={thread.a} agent={agents.get(thread.a)} />
          <StatePill state={thread.a_state} />
        </span>
        <span className="text-[11px] text-ink-3">⇄</span>
        <span className="inline-flex items-center gap-1">
          <AgentChip agentKey={thread.b} agent={agents.get(thread.b)} />
          <StatePill state={thread.b_state} />
        </span>
        <span className="ml-auto text-[11.5px] text-ink-3">{thread.updated && `updated ${agoText(thread.updated, now)}`}</span>
      </div>
    </header>
  )
}

function Remote({ thread, agents }: { thread: Thread; agents: Map<string, Agent> }) {
  const a = agents.get(thread.a)
  const b = agents.get(thread.b)
  const working = thread.a_state === "working" || thread.b_state === "working"
  return (
    <div className="flex flex-1 flex-col items-center justify-center px-8 py-12 text-center">
      <div className="flex items-center">
        <div className="flex w-32 flex-col items-center gap-2">
          <Orb agentKey={thread.a} size={64} working={a?.working} harness={a?.harness} />
          <span className="max-w-full truncate text-[12.5px] font-medium text-ink">{agentName(a)}</span>
          <StatePill state={thread.a_state} />
        </div>
        <svg width="96" height="20" viewBox="0 0 96 20" className="-mt-16" aria-hidden>
          <path d="M2 10 H94" stroke="var(--line-strong)" strokeWidth="1.5" fill="none" />
          {working && <path d="M2 10 H94" stroke="var(--accent)" strokeWidth="1.8" strokeDasharray="2 10" strokeLinecap="round" fill="none" style={{ animation: "flow 1.2s linear infinite" }} />}
          <circle cx="48" cy="10" r="9" fill="var(--page)" stroke="var(--line-strong)" />
          <path d="M44.5 9.5v-1.6a3.5 3.5 0 0 1 7 0v1.6M43.5 9.5h9v5h-9z" fill="none" stroke="var(--ink-3)" strokeWidth="1.3" strokeLinejoin="round" />
        </svg>
        <div className="flex w-32 flex-col items-center gap-2">
          <Orb agentKey={thread.b} size={64} working={b?.working} harness={b?.harness} />
          <span className="max-w-full truncate text-[12.5px] font-medium text-ink">{agentName(b)}</span>
          <StatePill state={thread.b_state} />
        </div>
      </div>
      <p className="mt-8 text-[15px] font-medium text-ink">The conversation is private to these two agents</p>
      <p className="mt-1.5 max-w-sm text-[13px] leading-relaxed text-ink-3">
        Presence shares a thread's subject and both sides' states, so every host on the network can follow it. Only the two agents can read the messages, unless one of them shares its conversations with this host (<code className="font-mono text-[12px]">awp share</code>).
      </p>
    </div>
  )
}

export function ThreadDetail({ thread, state, agents }: { thread: Thread; state: State; agents: Map<string, Agent> }) {
  const [conv, setConv] = useState<Conversation>()
  const [error, setError] = useState<string>()
  const [fresh, setFresh] = useState<Set<number>>(new Set())
  const end = useRef<HTMLDivElement>(null)
  // A thread this host is part of, or one another agent shares with it.
  const readable = thread.local || !!thread.shared_by
  const peer = thread.local ? thread.peer : thread.shared_by
  const th = thread.th

  useEffect(() => {
    if (!readable || !peer) return
    // the component is keyed by thread, so state starts empty
    let live = true
    getStore()
      .transport.thread(peer, th)
      .then((c) => live && setConv(c))
      .catch((e: Error) => live && setError(e.message))
    const off = getStore().onMessage((m) => {
      if (m.peer !== peer || m.th !== th) return
      setConv((c) => (c && !c.messages.some((x) => x.seq === m.message.seq) ? { ...c, messages: [...c.messages, m.message] } : c))
      setFresh((f) => new Set(f).add(m.message.seq))
    })
    return () => {
      live = false
      off()
    }
  }, [readable, peer, th])

  const count = conv?.messages.length ?? 0
  useEffect(() => {
    end.current?.scrollIntoView({ block: "end", behavior: count > 0 && fresh.size ? "smooth" : "auto" })
  }, [count, fresh.size])

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <Header thread={thread} agents={agents} />
      {!readable ? (
        <Remote thread={thread} agents={agents} />
      ) : error ? (
        <div className="flex flex-1 flex-col items-center justify-center gap-1 px-8 text-center">
          <p className="text-[14px] font-medium text-ink">Couldn't load the conversation</p>
          <p className="text-[12.5px] text-ink-3">{error}</p>
        </div>
      ) : !conv ? (
        <div className="flex flex-1 items-center justify-center">
          <Loading label="Loading conversation" />
        </div>
      ) : (
        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-5">
          {thread.shared_by && (
            <p className="mb-4 rounded-[10px] bg-field px-3 py-2 text-[12px] leading-relaxed text-ink-3 shadow-hairline">
              {agentName(agents.get(thread.shared_by), "The agent")} shares its conversations with this host. Either agent can keep this thread private with{" "}
              <code className="font-mono text-[11.5px]">awp private {thread.th}</code>.
            </p>
          )}
          <div className="flex flex-col gap-4">
            {conv.messages.length === 0 && <p className="py-10 text-center text-[13px] text-ink-3">No messages yet.</p>}
            {conv.messages.map((m) =>
              m.kind === "msg" ? (
                (m.parts ?? []).length === 0 ? null : (
                <Bubble key={m.seq} m={m} mine={m.from === (thread.local ? state.self : thread.shared_by)} agent={agents.get(m.from)} fresh={fresh.has(m.seq)} />
                )
              ) : (
                <Marker key={m.seq} m={m} agent={agents.get(m.from)} />
              ),
            )}
          </div>
          <div ref={end} className="h-2" />
        </div>
      )}
    </div>
  )
}
