// The dashboard's navigation, after Beautiful UI's SidebarNav: a 224px
// sidebar that collapses to a 52px rail with the icons in place, a gliding
// hover highlight, and a searchable list, here of every agent on the network.
import { useEffect, useRef, useState, type CSSProperties, type ReactNode } from "react"
import { Comet, Ripple } from "loading-dev"
import {
  Activity as ActivityIcon,
  ChevronDown,
  LayoutGrid,
  MessagesSquare,
  Moon,
  PanelLeftClose,
  PanelLeftOpen,
  Search,
  Share2,
  Sun,
  Users,
  Volume2,
  VolumeX,
  X,
} from "lucide-react"

import { Shimmer } from "@/components/atoms/Shimmer"
import GlideMenu from "@/components/primitives/GlideMenu"
import { useTheme } from "@/components/theme-provider"
import { agentName, quietMinutes, splitName } from "@/lib/format"
import { harnessName } from "@/lib/harness"
import type { View } from "@/lib/route"
import { setSound, useSound } from "@/lib/sound"
import { getStore, type Conn } from "@/lib/store"
import type { Agent, State } from "@/lib/types"

import { Wordmark } from "./Header"
import { Orb } from "./Orb"

const MOTION = {
  expandedWidth: 224,
  collapsedWidth: 52,
  duration: 280,
  copyDuration: 180,
  copyOffset: 8,
  easing: "cubic-bezier(0.16, 1, 0.3, 1)",
}

const SEARCH_MOTION = { duration: 180, closedWidth: 28, easing: "cubic-bezier(0.16, 1, 0.3, 1)" }

const DOT: Record<Agent["status"], string> = {
  self: "var(--ink)",
  connected: "var(--green)",
  online: "var(--accent)",
  stale: "var(--ink-3)",
  offline: "var(--line-strong)",
}

function GlideGroup({ children }: { children: ReactNode }) {
  return (
    <GlideMenu rowSelector="[data-row]" highlightClassName="sidebar-glide-highlight rounded-[7px] bg-hover-2" className="group/glide flex flex-col gap-px">
      {children}
    </GlideMenu>
  )
}

function RailButton({
  icon,
  label,
  active = false,
  count,
  hint,
  onClick,
  title,
  sound,
}: {
  icon: ReactNode
  label: ReactNode
  active?: boolean
  count?: ReactNode
  hint?: string
  onClick?: () => void
  title?: string
  /** the interface sound for pressing it (lib/sound) */
  sound?: string
}) {
  return (
    <button
      data-row
      data-sound={sound}
      type="button"
      onClick={onClick}
      title={title ?? (typeof label === "string" ? label : undefined)}
      aria-current={active ? "page" : undefined}
      className={`sidebar-row relative z-10 mx-2 flex h-8 items-center rounded-[8px] px-2 text-left
        transition-[width,background-color,color,transform] duration-150 active:scale-[0.98]
        ${active ? "bg-hover-2 group-hover/glide:bg-transparent" : ""}`}
    >
      <span className={`flex size-5 shrink-0 items-center justify-center ${active ? "text-ink" : "text-ink-2"}`}>{icon}</span>
      <span className={`sidebar-copy ml-1.5 min-w-0 flex-1 truncate text-[14px] font-medium ${active ? "text-ink" : "text-ink-2"}`}>{label}</span>
      {count !== undefined && count !== 0 && <span className="sidebar-copy mr-1 shrink-0 text-[12px] font-medium tabular-nums text-ink-3">{count}</span>}
      {hint && (
        <kbd className="sidebar-copy mr-0.5 shrink-0 rounded-[5px] bg-field px-1.5 font-mono text-[11px] text-ink-3 shadow-hairline">{hint}</kbd>
      )}
    </button>
  )
}

function connLabel(conn: Conn): { icon: ReactNode; label: string; tone: string } {
  if (conn === "live") return { icon: <Ripple size={16} color="var(--green)" />, label: "Live", tone: "text-ink-2" }
  if (conn === "connecting") return { icon: <Comet size={14} color="var(--ink-3)" />, label: "Connecting", tone: "text-ink-3" }
  return { icon: <Comet size={14} color="var(--orange)" />, label: "Reconnecting", tone: "text-orange" }
}

export function Sidebar({
  state,
  conn,
  view,
  onView,
  onAgent,
  onSearch,
  selectedAgent,
  collapsed,
  onCollapse,
  mobile = false,
  onClose,
}: {
  state: State
  conn: Conn
  view: View
  onView: (v: View) => void
  onAgent: (key: string) => void
  onSearch: () => void
  selectedAgent?: string
  collapsed: boolean
  onCollapse: (c: boolean) => void
  /** in the phone drawer: never collapsed, with a close button */
  mobile?: boolean
  onClose?: () => void
}) {
  const sound = useSound()
  const { setTheme } = useTheme()
  const dark = typeof document !== "undefined" && document.documentElement.classList.contains("dark")
  const [agentsOpen, setAgentsOpen] = useState(true)
  const [searchOpen, setSearchOpen] = useState(false)
  const [query, setQuery] = useState("")
  const searchRef = useRef<HTMLInputElement>(null)
  const isCollapsed = collapsed && !mobile

  useEffect(() => {
    if (searchOpen) searchRef.current?.focus()
  }, [searchOpen])

  const q = query.trim().toLowerCase()
  const agents = state.agents.filter(
    (a) => !q || [a.name, a.short, a.about, a.harness, harnessName(a.harness), a.model, a.host].some((x) => x?.toLowerCase().includes(q)),
  )
  const self = state.agents.find((a) => a.key === state.self)
  const c = connLabel(conn)
  const nav: { key: View; label: string; icon: ReactNode; count?: ReactNode }[] = [
    { key: "overview", label: "Overview", icon: <LayoutGrid size={17} /> },
    { key: "network", label: "Network", icon: <Share2 size={17} /> },
    { key: "threads", label: "Threads", icon: <MessagesSquare size={17} />, count: state.stats.active || undefined },
    { key: "agents", label: "Agents", icon: <Users size={17} />, count: state.stats.agents },
    { key: "activity", label: "Activity", icon: <ActivityIcon size={17} /> },
  ]
  const go = (v: View) => {
    onView(v)
    onClose?.()
  }

  return (
    <aside
      data-sidebar-collapsed={isCollapsed}
      aria-label="Dashboard navigation"
      className="relative flex h-full shrink-0 overflow-hidden transition-[width]"
      style={
        {
          width: isCollapsed ? MOTION.collapsedWidth : MOTION.expandedWidth,
          transitionDuration: `${MOTION.duration}ms`,
          transitionTimingFunction: MOTION.easing,
          "--sidebar-copy-duration": `${MOTION.copyDuration}ms`,
          "--sidebar-copy-offset": `${MOTION.copyOffset}px`,
          "--sidebar-easing": MOTION.easing,
        } as CSSProperties
      }
    >
      <div className="flex min-h-0 w-[224px] shrink-0 flex-col py-2.5">
        {/* Brand, and the collapse control that swaps places with it. */}
        <div className="relative mb-2 h-10 shrink-0">
          <span className="sidebar-logo absolute top-1 left-3.5 flex h-8 items-center" aria-hidden={isCollapsed}>
            <Wordmark />
          </span>
          {mobile ? (
            <button
              type="button"
              aria-label="Close navigation"
              data-sound="none"
              onClick={onClose}
              className="absolute top-1 right-2 flex size-8 items-center justify-center rounded-[8px] text-ink-3 transition-colors hover:bg-hover-2 hover:text-ink"
            >
              <X size={18} />
            </button>
          ) : (
            <>
              <button
                type="button"
                aria-label="Collapse sidebar"
                data-sound="close"
                aria-hidden={isCollapsed}
                tabIndex={isCollapsed ? -1 : 0}
                onClick={() => {
                  onCollapse(true)
                  setSearchOpen(false)
                  setQuery("")
                }}
                className="sidebar-collapse-control absolute top-1 right-2 flex size-8 items-center justify-center rounded-[8px] text-ink-3 transition-[opacity,background-color,color] duration-150 hover:bg-hover-2 hover:text-ink"
              >
                <PanelLeftClose size={18} />
              </button>
              <button
                type="button"
                aria-label="Expand sidebar"
                data-sound="open"
                aria-hidden={!isCollapsed}
                tabIndex={isCollapsed ? 0 : -1}
                onClick={() => onCollapse(false)}
                className="sidebar-expand-control absolute top-0.5 left-2 flex size-9 items-center justify-center rounded-[8px] text-ink-3 transition-[opacity,background-color,color] duration-150 hover:bg-hover-2 hover:text-ink"
              >
                <PanelLeftOpen size={18} />
              </button>
            </>
          )}
        </div>

        {/* The host this page watches from. */}
        <GlideGroup>
          <button
            data-row
            type="button"
            onClick={() => {
              onAgent(state.self)
              onClose?.()
            }}
            title={`Watching from ${state.host_name}`}
            className="sidebar-row relative z-10 mx-2 flex h-11 items-center rounded-[8px] px-1.5 text-left transition-[width,background-color] duration-150 active:scale-[0.98]"
          >
            <Orb agentKey={state.self} size={24} harness={self?.harness} />
            <span className="sidebar-copy ml-2 min-w-0 flex-1">
              <span className="block text-[11px] leading-tight text-ink-3">Watching from</span>
              <span className="block truncate text-[13.5px] leading-tight font-medium text-ink">{state.host_name || "this host"}</span>
            </span>
          </button>
        </GlideGroup>

        <div className="mt-2">
          <GlideGroup>
            <RailButton icon={<Search size={17} />} label="Search" hint="/" onClick={() => { onSearch(); onClose?.() }} />
            {nav.map((n) => (
              <RailButton key={n.key} icon={n.icon} label={n.label} count={n.count} active={view === n.key} sound="select" onClick={() => go(n.key)} />
            ))}
          </GlideGroup>
        </div>

        {/* Every agent: the network's "recents". */}
        <div className="mt-4 min-h-0 flex-1 overflow-x-hidden overflow-y-auto">
          <div className="sidebar-copy relative mx-2 mb-1 h-8">
            <button
              type="button"
              aria-expanded={agentsOpen}
              aria-hidden={searchOpen}
              onClick={() => setAgentsOpen((o) => !o)}
              className={`absolute inset-y-0 left-0 flex items-center gap-1 rounded-[8px] px-2 text-[12.5px] font-medium text-ink-3 transition-[opacity,transform,color] hover:text-ink-2 ${searchOpen ? "pointer-events-none -translate-x-1 opacity-0" : "translate-x-0 opacity-100"}`}
              style={{ transitionDuration: `${SEARCH_MOTION.duration}ms`, transitionTimingFunction: SEARCH_MOTION.easing }}
            >
              <ChevronDown size={15} className={`transition-transform duration-200 ${agentsOpen ? "" : "-rotate-90"}`} />
              <span>On the network</span>
              <span className="text-ink-3 tabular-nums">{state.agents.length}</span>
            </button>
            <button
              type="button"
              aria-label="Filter agents"
              aria-expanded={searchOpen}
              onClick={() => {
                setSearchOpen(true)
                setAgentsOpen(true)
              }}
              className={`absolute top-0 right-0 z-10 flex size-8 items-center justify-center rounded-[8px] text-ink-3 transition-[opacity,background-color,color,transform] hover:bg-hover-2 hover:text-ink active:scale-[0.96] ${searchOpen ? "pointer-events-none opacity-0" : "opacity-100"}`}
              style={{ transitionDuration: `${SEARCH_MOTION.duration}ms` }}
            >
              <Search size={15} />
            </button>
            <div
              className={`absolute top-0 right-0 z-20 flex h-8 items-center overflow-hidden rounded-[8px] bg-field text-ink-3 shadow-hairline transition-[width,opacity] focus-within:text-ink-2 ${searchOpen ? "pointer-events-auto opacity-100" : "pointer-events-none opacity-0"}`}
              style={{ width: searchOpen ? "100%" : SEARCH_MOTION.closedWidth, transitionDuration: `${SEARCH_MOTION.duration}ms`, transitionTimingFunction: SEARCH_MOTION.easing }}
            >
              <span className="ml-2 flex shrink-0 items-center">
                <Search size={14} />
              </span>
              <input
                ref={searchRef}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Escape") {
                    e.stopPropagation()
                    setSearchOpen(false)
                    setQuery("")
                  }
                }}
                placeholder="Filter agents"
                aria-label="Filter agents"
                tabIndex={searchOpen ? 0 : -1}
                className="ml-1.5 min-w-0 flex-1 bg-transparent text-[13px] font-medium text-ink outline-none placeholder:text-ink-3"
              />
              <button
                type="button"
                aria-label="Clear filter"
                tabIndex={searchOpen ? 0 : -1}
                onClick={() => {
                  setSearchOpen(false)
                  setQuery("")
                }}
                className="flex size-8 shrink-0 items-center justify-center rounded-[8px] text-ink-3 transition-colors hover:bg-hover-2 hover:text-ink"
              >
                <X size={15} />
              </button>
            </div>
          </div>

          {(agentsOpen || isCollapsed) && (
            <GlideGroup>
              {agents.map((a) => {
                const { who, host } = splitName(agentName(a))
                const dim = a.status === "stale" || a.status === "offline"
                return (
                  <button
                    key={a.key}
                    data-row
                    type="button"
                    title={[agentName(a), harnessName(a.harness), a.model, a.host && `on ${a.host}`].filter(Boolean).join(" · ")}
                    onClick={() => {
                      onAgent(a.key)
                      onClose?.()
                    }}
                    className={`sidebar-row relative z-10 mx-2 flex h-9 items-center rounded-[8px] px-1.5 text-left transition-[width,background-color,transform] duration-150 active:scale-[0.98] ${
                      selectedAgent === a.key ? "bg-hover-2 group-hover/glide:bg-transparent" : ""
                    }`}
                  >
                    <span className="relative flex shrink-0">
                      <Orb agentKey={a.key} size={22} working={a.working} dim={dim} harness={a.harness} />
                      <span className="absolute -top-px -right-px size-2 rounded-full ring-[1.5px] ring-page" style={{ background: DOT[a.status] }} />
                    </span>
                    <span className="sidebar-copy ml-2 flex min-w-0 flex-1 items-baseline gap-1">
                      <span className={`truncate text-[13.5px] font-medium ${dim ? "text-ink-3" : "text-ink-2"}`}>{who}</span>
                      {host && <span className="truncate text-[12px] text-ink-3">@{host}</span>}
                    </span>
                    <span className="sidebar-copy ml-1 shrink-0 text-[11px] font-medium">
                      {quietMinutes(a) !== undefined ? (
                        <span className="text-orange">quiet {quietMinutes(a)}m</span>
                      ) : a.working ? (
                        <Shimmer>working</Shimmer>
                      ) : a.waiting ? (
                        <span className="text-orange">waiting</span>
                      ) : null}
                    </span>
                  </button>
                )
              })}
              {q && agents.length === 0 && <div className="sidebar-copy mx-2 px-2 py-2 text-[12.5px] text-ink-3">No agents match</div>}
            </GlideGroup>
          )}
        </div>

        {/* Connection, sound and theme. */}
        <div className="mt-2 shrink-0 border-t border-line pt-2">
          <GlideGroup>
            <RailButton
              icon={c.icon}
              label={<span className={c.tone}>{c.label}</span>}
              title={conn === "live" ? "Streaming live from the host" : "The stream dropped; click to retry now"}
              onClick={() => getStore().reconnect()}
            />
            <RailButton
              icon={sound ? <Volume2 size={17} /> : <VolumeX size={17} />}
              label={sound ? "Sounds on" : "Sounds off"}
              sound="none"
              onClick={() => setSound(!sound)}
            />
            <RailButton icon={dark ? <Sun size={17} /> : <Moon size={17} />} label={dark ? "Light mode" : "Dark mode"} sound={dark ? "on" : "off"} onClick={() => setTheme(dark ? "light" : "dark")} />
          </GlideGroup>
        </div>
      </div>
    </aside>
  )
}
