import { lazy, Suspense, useEffect, useMemo, useState } from "react"
import { Dialog } from "@base-ui/react/dialog"
import { Menu, Search } from "lucide-react"

import { AgentList } from "@/components/awp/AgentList"
import { ActivityFeed } from "@/components/awp/ActivityFeed"
import { EmptyNetwork } from "@/components/awp/EmptyNetwork"
import { Connection, Wordmark } from "@/components/awp/Header"
import { Loading } from "@/components/awp/Loading"
import { Sidebar } from "@/components/awp/Sidebar"
import { NetworkGraph } from "@/components/awp/NetworkGraph"
import { Palette } from "@/components/awp/Palette"
import { Sheet } from "@/components/awp/Sheet"
import { Stats } from "@/components/awp/Stats"
import { ThreadList } from "@/components/awp/ThreadList"
import { agentName, plural } from "@/lib/format"
import { playFor, usePanelSound } from "@/lib/sound"
import { useView } from "@/lib/route"
import { getStore, useAWP } from "@/lib/store"
import type { Activity, State, Thread } from "@/lib/types"

const AgentDetail = lazy(() => import("@/components/awp/AgentDetail").then((m) => ({ default: m.AgentDetail })))
const ThreadDetail = lazy(() => import("@/components/awp/ThreadDetail").then((m) => ({ default: m.ThreadDetail })))

type Selection = { kind: "agent"; key: string } | { kind: "thread"; id: string; fallback?: Thread } | undefined

function Card({ title, sub, action, children, className = "" }: { title?: string; sub?: string; action?: React.ReactNode; children: React.ReactNode; className?: string }) {
  return (
    <section className={`min-w-0 rounded-[18px] bg-surface shadow-card ${className}`}>
      {title && (
        <div className="flex items-start justify-between gap-3 px-4 pt-4 pb-2">
          <div className="min-w-0">
            <h2 className="text-[15px] font-semibold tracking-[-0.01em] text-ink">{title}</h2>
            {sub && <p className="text-[12px] text-ink-3">{sub}</p>}
          </div>
          {action}
        </div>
      )}
      {children}
    </section>
  )
}

function PageTitle({ title, sub, big = false }: { title: string; sub: string; big?: boolean }) {
  return (
    <div className="mb-6">
      <h1 className={`leading-[1.05] font-semibold text-balance text-ink ${big ? "text-[34px] tracking-[-0.04em] sm:text-[44px]" : "text-[28px] tracking-[-0.035em] sm:text-[34px]"}`}>{title}</h1>
      <p className="mt-2 max-w-3xl text-[15px] text-ink-2">{sub}</p>
    </div>
  )
}

function SeeAll({ onClick }: { onClick: () => void }) {
  return (
    <button type="button" data-sound="select" onClick={onClick} className="shrink-0 rounded-full px-2 py-1 text-[12px] font-medium text-ink-3 transition-colors hover:bg-hover hover:text-ink">
      See all
    </button>
  )
}

function NetworkCard({
  state,
  alone,
  selected,
  onSelect,
  className,
  titled,
}: {
  state: State
  alone: boolean
  titled: boolean
  selected?: string
  onSelect: (key: string) => void
  className: string
}) {
  return (
    <section className={`relative min-w-0 overflow-hidden rounded-[18px] bg-surface shadow-card ${className}`}>
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0"
        style={{
          background: "radial-gradient(ellipse 60% 55% at 50% 50%, var(--glow-a), transparent 70%), radial-gradient(ellipse 40% 40% at 80% 20%, var(--glow-b), transparent 70%)",
        }}
      />
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0"
        style={{
          backgroundImage: "radial-gradient(color-mix(in oklch, var(--ink-3) 45%, transparent) 1.2px, transparent 1.4px)",
          backgroundSize: "22px 22px",
          maskImage: "radial-gradient(ellipse at center, #000 45%, transparent 92%)",
        }}
      />
      <div className={`absolute top-4 left-4 z-10 ${titled ? "" : "hidden"}`}>
        <h2 className="text-[15px] font-semibold tracking-[-0.01em] text-ink">Network</h2>
        <p className="text-[12px] text-ink-3">
          {plural(state.agents.length, "agent")} · {plural(state.links.length, "link")}
        </p>
      </div>
      <div className="absolute bottom-3 left-4 z-10 hidden items-center gap-3 text-[11.5px] text-ink-3 sm:flex">
        <span className="flex items-center gap-1.5"><span className="size-1.5 rounded-full bg-green" />connected</span>
        <span className="flex items-center gap-1.5"><span className="size-1.5 rounded-full bg-accent" />via gossip</span>
        <span className="flex items-center gap-1.5"><span className="size-1.5 rounded-full bg-ink-3" />quiet</span>
      </div>
      <div className="absolute inset-0 pt-8">{alone ? <EmptyNetwork state={state} /> : <NetworkGraph state={state} selected={selected} onSelect={onSelect} />}</div>
    </section>
  )
}

function headline(state: State): { title: string; sub: string } {
  const others = state.agents.length - 1
  const working = state.agents.filter((a) => a.working).length
  if (others <= 0) return { title: "Waiting for the network", sub: `Watching from ${state.host_name || "this host"}` }
  const title = working ? `${plural(state.agents.length, "agent")}, ${working} at work` : `${plural(state.agents.length, "agent")}, all quiet`
  return { title, sub: `Live from ${state.host_name || "this host"} and every agent its presence gossip reaches` }
}

function threadFor(state: State | undefined, a: Activity): Thread | undefined {
  if (!state || !a.th) return undefined
  const pair = [a.from, a.to ?? ""].sort()
  return (
    state.threads.find((t) => t.th === a.th && t.a === pair[0] && t.b === pair[1]) ??
    state.threads.find((t) => t.th === a.th && (t.a === a.from || t.b === a.from))
  )
}

export function App() {
  const { state, activity, conn, error } = useAWP()
  const [sel, setSel] = useState<Selection>()
  const [palette, setPalette] = useState(false)
  const [view, go] = useView()
  const [drawer, setDrawer] = useState(false)
  const [collapsed, setCollapsedState] = useState(() => {
    try {
      return localStorage.getItem("awp.sidebar") === "collapsed"
    } catch {
      return false
    }
  })
  const setCollapsed = (c: boolean) => {
    setCollapsedState(c)
    try {
      localStorage.setItem("awp.sidebar", c ? "collapsed" : "open")
    } catch {
      // private mode: the choice lasts this visit
    }
  }

  useEffect(() => getStore().onActivity(playFor), [])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null
      if (t?.closest("input, textarea, [contenteditable='true']")) return
      if ((e.key === "/" && !e.metaKey && !e.ctrlKey) || (e.key.toLowerCase() === "k" && (e.metaKey || e.ctrlKey))) {
        e.preventDefault()
        setPalette(true)
      }
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [])

  const agents = useMemo(() => new Map((state?.agents ?? []).map((a) => [a.key, a])), [state?.agents])

  useEffect(() => {
    if (!state) return
    const w = state.stats.working
    document.title = w ? `(${w}) awp · ${state.host_name}` : `awp · ${state.host_name}`
  }, [state])

  const openAgent = (key: string) => setSel({ kind: "agent", key })
  const openThread = (t: Thread) => setSel({ kind: "thread", id: t.id, fallback: t })
  const openActivity = (a: Activity) => {
    const t = threadFor(state, a)
    if (t) openThread(t)
    else openAgent(a.from)
  }

  const selAgent = sel?.kind === "agent" ? agents.get(sel.key) : undefined
  const selThread = sel?.kind === "thread" ? (state?.threads.find((t) => t.id === sel.id) ?? sel.fallback) : undefined
  usePanelSound(!!(selAgent || selThread))
  usePanelSound(palette)
  usePanelSound(drawer)

  if (!state) {
    return (
      <div className="flex min-h-svh flex-col items-center justify-center gap-4 px-4">
        <Wordmark />
        {error ? (
          <div className="max-w-sm text-center">
            <p className="text-[14px] font-medium text-ink">Can't reach the awp daemon</p>
            <p className="mt-1 text-[12.5px] text-ink-3">{error}</p>
            <p className="mt-3 text-[12.5px] text-ink-2">
              Is it running? Start it with <code className="font-mono">awp up</code>. Retrying…
            </p>
          </div>
        ) : (
          <Loading kind="ripple" label="Listening for the network" />
        )}
      </div>
    )
  }

  const h = headline(state)
  const alone = state.agents.length <= 1
  const selectedKey = sel?.kind === "agent" ? sel.key : undefined
  const selectedThread = sel?.kind === "thread" ? sel.id : undefined
  const sidebar = (mobile: boolean) => (
    <Sidebar
      state={state}
      conn={conn}
      view={view}
      onView={go}
      onAgent={openAgent}
      onSearch={() => setPalette(true)}
      selectedAgent={selectedKey}
      collapsed={collapsed}
      onCollapse={setCollapsed}
      mobile={mobile}
      onClose={mobile ? () => setDrawer(false) : undefined}
    />
  )
  const network = (className: string, titled = true) => (
    <NetworkCard state={state} alone={alone} selected={selectedKey} onSelect={openAgent} className={className} titled={titled} />
  )
  const threads = (
    <ThreadList threads={state.threads} agents={agents} selected={selectedThread} onOpen={openThread} onAgent={openAgent} />
  )
  const agentList = <AgentList agents={state.agents} selected={selectedKey} onSelect={openAgent} />
  const feed = (className: string, titled = true) => (
    <Card className={`flex flex-col ${className}`}>
      <ActivityFeed activity={activity} agents={agents} query="" onAgent={openAgent} onThread={openActivity} className="flex-1" titled={titled} />
    </Card>
  )

  return (
    <div className="flex min-h-svh">
      <div className="sticky top-0 hidden h-svh shrink-0 border-r border-line/70 bg-page lg:block">{sidebar(false)}</div>

      <Dialog.Root open={drawer} onOpenChange={setDrawer}>
        <Dialog.Portal>
          <Dialog.Backdrop className="fixed inset-0 z-40 bg-[oklch(0.2_0.01_260/0.18)] backdrop-blur-[2px] transition-opacity duration-300 data-[ending-style]:opacity-0 data-[starting-style]:opacity-0 lg:hidden dark:bg-[oklch(0_0_0/0.45)]" />
          <Dialog.Popup className="fixed inset-y-0 left-0 z-50 bg-page shadow-overlay outline-none transition-transform duration-300 ease-[var(--ease-out-strong)] data-[ending-style]:-translate-x-full data-[starting-style]:-translate-x-full lg:hidden">
            <Dialog.Title className="sr-only">Navigation</Dialog.Title>
            {sidebar(true)}
          </Dialog.Popup>
        </Dialog.Portal>
      </Dialog.Root>

      <div className="min-w-0 flex-1">
        <header className="sticky top-0 z-30 border-b border-line/70 bg-page/80 backdrop-blur-xl backdrop-saturate-150 lg:hidden">
          <div className="flex h-14 items-center gap-2 px-4 sm:px-6">
            <button
              type="button"
              aria-label="Open navigation"
              onClick={() => setDrawer(true)}
              className="-ml-1.5 flex size-9 items-center justify-center rounded-[10px] text-ink-2 transition-colors hover:bg-hover hover:text-ink"
            >
              <Menu size={20} />
            </button>
            <Wordmark />
            <div className="ml-auto flex items-center gap-2">
              <button
                type="button"
                aria-label="Search"
                onClick={() => setPalette(true)}
                className="flex size-8 items-center justify-center rounded-full bg-surface text-ink-2 shadow-hairline"
              >
                <Search size={14} />
              </button>
              <Connection conn={conn} />
            </div>
          </div>
        </header>

        <main key={view} className="mx-auto max-w-[1440px] px-4 pt-7 pb-10 sm:px-6 lg:px-8 lg:pt-9" style={{ animation: "rise-in 420ms var(--ease-out-strong) both" }}>
          {view === "overview" && (
            <>
              <PageTitle title={h.title} sub={h.sub} big />
              <Stats state={state} activity={activity} />
              <div className="mt-4 grid gap-4 xl:grid-cols-[minmax(0,1fr)_minmax(340px,400px)]">
                <div className="flex min-w-0 flex-col gap-4">
                  {network("h-[440px] sm:h-[500px]")}
                  <div className="grid min-w-0 gap-4 2xl:grid-cols-[minmax(0,1.5fr)_minmax(0,1fr)]">
                    <Card title="Threads" sub="Every conversation on the network, with both sides' states" action={<SeeAll onClick={() => go("threads")} />}>
                      <div className="px-1.5 pb-2">{threads}</div>
                    </Card>
                    <Card title="Agents" sub={`${state.stats.up} of ${state.stats.agents} up`} action={<SeeAll onClick={() => go("agents")} />}>
                      <div className="px-1.5 pb-2">{agentList}</div>
                    </Card>
                  </div>
                </div>
                <aside className="min-w-0 xl:sticky xl:top-6 xl:self-start">{feed("h-[640px] xl:h-[calc(100svh-48px)]")}</aside>
              </div>
            </>
          )}
          {view === "network" && (
            <>
              <PageTitle title="Network" sub={`${plural(state.agents.length, "agent")} and ${plural(state.links.length, "link")}, as far as presence gossip reaches from ${state.host_name}`} />
              {network("h-[calc(100svh-190px)] min-h-[440px]", false)}
            </>
          )}
          {view === "threads" && (
            <>
              <PageTitle title="Threads" sub={`${plural(state.stats.threads, "conversation")}, ${state.stats.active} active. Threads this host is part of open as a live conversation; the rest are private to their two agents.`} />
              <Card>
                <div className="p-1.5">{threads}</div>
              </Card>
            </>
          )}
          {view === "agents" && (
            <>
              <PageTitle title="Agents" sub={`${state.stats.up} of ${plural(state.stats.agents, "agent")} up. ${state.stats.working} working, ${state.stats.waiting} waiting.`} />
              <Card>
                <div className="p-1.5">{agentList}</div>
              </Card>
            </>
          )}
          {view === "activity" && (
            <>
              <PageTitle title="Activity" sub="Everything happening on the network, live, from this host and every agent's presence" />
              {feed("h-[calc(100svh-190px)] min-h-[480px]", false)}
            </>
          )}

          <footer className="mt-10 flex flex-wrap items-center justify-between gap-2 text-[12px] text-ink-3">
            <span>awp {state.version}</span>
            <span>
              Press <kbd className="rounded-[5px] bg-field px-1 font-mono shadow-hairline">/</kbd> to search ·{" "}
              <kbd className="rounded-[5px] bg-field px-1 font-mono shadow-hairline">d</kbd> for dark mode
            </span>
          </footer>
        </main>
      </div>

      <Sheet
        open={!!(selAgent || selThread)}
        onClose={() => setSel(undefined)}
        title={selAgent ? agentName(selAgent) : (selThread?.subject ?? "Thread")}
      >
        <Suspense
          fallback={
            <div className="flex flex-1 items-center justify-center">
              <Loading label="Loading" />
            </div>
          }
        >
          {selAgent && <AgentDetail agent={selAgent} state={state} agents={agents} onThread={openThread} onAgent={openAgent} />}
          {selThread && <ThreadDetail key={selThread.id} thread={selThread} state={state} agents={agents} />}
        </Suspense>
      </Sheet>

      <Palette open={palette} onClose={() => setPalette(false)} state={state} agents={agents} onAgent={openAgent} onThread={openThread} />
    </div>
  )
}

export default App
