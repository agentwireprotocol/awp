import { Dialog } from "@base-ui/react/dialog"
import { useMemo, useState } from "react"

import GlideMenu from "@/components/primitives/GlideMenu"
import { agentName } from "@/lib/format"
import type { Agent, State, Thread } from "@/lib/types"

import { StatePill } from "./chips"
import { Orb } from "./Orb"
import { harnessName } from "@/lib/harness"

type Item = { kind: "agent"; agent: Agent } | { kind: "thread"; thread: Thread }

/** Search every agent and thread on the network ("/" or ⌘K). */
export function Palette({
  open,
  onClose,
  state,
  agents,
  onAgent,
  onThread,
}: {
  open: boolean
  onClose: () => void
  state?: State
  agents: Map<string, Agent>
  onAgent: (k: string) => void
  onThread: (t: Thread) => void
}) {
  const [q, setQ] = useState("")
  const [active, setActive] = useState(0)
  const items = useMemo<Item[]>(() => {
    if (!state) return []
    const s = q.trim().toLowerCase()
    const hit = (...xs: (string | undefined)[]) => !s || xs.some((x) => x?.toLowerCase().includes(s))
    return [
      ...state.agents.filter((a) => hit(a.name, a.about, a.short, a.key, a.harness, harnessName(a.harness), a.model, a.host)).map((agent) => ({ kind: "agent" as const, agent })),
      ...state.threads
        .filter((t) => hit(t.subject, t.th, agents.get(t.a)?.name, agents.get(t.b)?.name, t.a_state, t.b_state))
        .map((thread) => ({ kind: "thread" as const, thread })),
    ].slice(0, 12)
  }, [q, state, agents])

  const choose = (it: Item | undefined) => {
    if (!it) return
    onClose()
    setQ("")
    if (it.kind === "agent") onAgent(it.agent.key)
    else onThread(it.thread)
  }

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(o) => {
        if (!o) {
          onClose()
          setQ("")
        }
      }}
    >
      <Dialog.Portal>
        <Dialog.Backdrop className="fixed inset-0 z-40 bg-[oklch(0.2_0.01_260/0.16)] backdrop-blur-[2px] transition-opacity duration-200 data-[ending-style]:opacity-0 data-[starting-style]:opacity-0 dark:bg-[oklch(0_0_0/0.45)]" />
        <Dialog.Popup className="fixed top-[12svh] left-1/2 z-50 w-[min(560px,calc(100vw-32px))] -translate-x-1/2 overflow-hidden rounded-[16px] bg-surface shadow-overlay outline-none transition-[opacity,transform] duration-200 data-[ending-style]:scale-[0.98] data-[ending-style]:opacity-0 data-[starting-style]:scale-[0.98] data-[starting-style]:opacity-0">
          <Dialog.Title className="sr-only">Search</Dialog.Title>
          <div className="flex h-12 items-center gap-2.5 border-b border-line px-4">
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="var(--ink-3)" strokeWidth="2" strokeLinecap="round" className="shrink-0"><circle cx="11" cy="11" r="7" /><path d="M21 21l-4.3-4.3" /></svg>
            <input
              autoFocus
              value={q}
              onChange={(e) => {
                setQ(e.target.value)
                setActive(0)
              }}
              onKeyDown={(e) => {
                if (e.key === "ArrowDown") {
                  e.preventDefault()
                  setActive((i) => Math.min(items.length - 1, i + 1))
                } else if (e.key === "ArrowUp") {
                  e.preventDefault()
                  setActive((i) => Math.max(0, i - 1))
                } else if (e.key === "Enter") {
                  choose(items[active])
                }
              }}
              placeholder="Search agents, threads, states…"
              aria-label="Search agents and threads"
              className="min-w-0 flex-1 bg-transparent text-[14px] text-ink outline-none placeholder:text-ink-3"
            />
            <kbd className="rounded-[6px] bg-field px-1.5 font-mono text-[11px] text-ink-3 shadow-hairline">esc</kbd>
          </div>
          <div className="max-h-[50svh] overflow-y-auto p-1.5">
            {items.length === 0 ? (
              <div className="flex flex-col items-center gap-1 px-4 py-10">
                <span className="text-[13px] font-medium text-ink">No results found</span>
                <span className="text-[12px] text-ink-3">Adjust your search to try again</span>
              </div>
            ) : (
              <GlideMenu className="flex flex-col gap-px" highlightClassName="inset-x-0 rounded-[8px] bg-hover">
                {items.map((it, i) => (
                  <button
                    key={it.kind === "agent" ? it.agent.key : it.thread.id}
                    data-menu-row
                    type="button"
                    onMouseEnter={() => setActive(i)}
                    onClick={() => choose(it)}
                    className={`relative z-10 flex min-h-10 w-full items-center gap-3 rounded-[8px] px-2.5 py-2 text-left ${i === active ? "bg-hover" : ""}`}
                  >
                    {it.kind === "agent" ? (
                      <>
                        <Orb agentKey={it.agent.key} size={22} working={it.agent.working} harness={it.agent.harness} />
                        <span className="min-w-0 flex-1">
                          <span className="block truncate text-[13.5px] font-medium text-ink">{agentName(it.agent)}</span>
                          {it.agent.about && <span className="block truncate text-[12px] text-ink-3">{it.agent.about}</span>}
                        </span>
                        <span className="text-[11px] text-ink-3">Agent</span>
                      </>
                    ) : (
                      <>
                        <span className="flex size-[22px] items-center justify-center rounded-full bg-field text-ink-3 shadow-hairline">
                          <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round"><path d="M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.6A8 8 0 1 1 21 12z" /></svg>
                        </span>
                        <span className="min-w-0 flex-1">
                          <span className="block truncate text-[13.5px] font-medium text-ink">{it.thread.subject || it.thread.th}</span>
                          <span className="block truncate text-[12px] text-ink-3">
                            {agentName(agents.get(it.thread.a))} ⇄ {agentName(agents.get(it.thread.b))}
                          </span>
                        </span>
                        <StatePill state={it.thread.b_state === "working" ? it.thread.b_state : it.thread.a_state} />
                      </>
                    )}
                  </button>
                ))}
              </GlideMenu>
            )}
          </div>
        </Dialog.Popup>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
