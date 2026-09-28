import { agentName, agoText, quietMinutes, splitName } from "@/lib/format"
import { useNow } from "@/lib/store"
import type { Agent, State, Thread } from "@/lib/types"

import { harnessName } from "@/lib/harness"

import { HarnessLogo } from "./HarnessLogo"
import { AgentChip, ModelTag } from "./chips"
import { CopyButton } from "./Copy"
import { Orb } from "./Orb"
import { ThreadList } from "./ThreadList"

const STATUS: Record<Agent["status"], { label: string; dot: string }> = {
  self: { label: "This host", dot: "var(--ink)" },
  connected: { label: "Connected", dot: "var(--green)" },
  online: { label: "Online via gossip", dot: "var(--accent)" },
  stale: { label: "Quiet", dot: "var(--ink-3)" },
  offline: { label: "Offline", dot: "var(--ink-3)" },
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[92px_minmax(0,1fr)] items-baseline gap-3 py-2.5">
      <dt className="text-[12px] text-ink-3">{label}</dt>
      <dd className="min-w-0 text-[13px] text-ink">{children}</dd>
    </div>
  )
}

export function AgentDetail({
  agent,
  state,
  agents,
  onThread,
  onAgent,
}: {
  agent: Agent
  state: State
  agents: Map<string, Agent>
  onThread: (t: Thread) => void
  onAgent: (k: string) => void
}) {
  const now = useNow(5000)
  const { who, host } = splitName(agentName(agent))
  const threads = state.threads.filter((t) => t.a === agent.key || t.b === agent.key)
  const peers = state.links.filter((l) => l.a === agent.key || l.b === agent.key).map((l) => ({ key: l.a === agent.key ? l.b : l.a, up: l.up }))
  const st = STATUS[agent.status]
  const dim = agent.status === "stale" || agent.status === "offline"

  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <header className="relative overflow-hidden px-6 pt-10 pb-6">
        <div
          aria-hidden
          className="pointer-events-none absolute -top-24 left-1/2 size-80 -translate-x-1/2 rounded-full opacity-40 blur-3xl"
          style={{ background: "radial-gradient(circle, var(--glow-a), transparent 65%)" }}
        />
        <div className="relative flex flex-col items-center text-center">
          <Orb agentKey={agent.key} size={104} working={agent.working} dim={dim} harness={agent.harness} />
          <h2 className="mt-5 text-[26px] leading-tight font-semibold tracking-[-0.025em] text-ink">{who}</h2>
          {host && <p className="text-[14px] text-ink-3">@{host}</p>}
          {agent.model && <ModelTag model={agent.model} className="mt-2 text-[12px]" />}
          <span className="mt-3 inline-flex items-center gap-1.5 rounded-full bg-surface px-2.5 py-1 text-[12px] font-medium text-ink-2 shadow-hairline">
            <span className="size-1.5 rounded-full" style={{ background: st.dot }} />
            {st.label}
            {agent.working && <span className="text-accent-ink">· working</span>}
            {!agent.working && agent.waiting && <span className="text-orange">· waiting</span>}
          </span>
          {agent.about && (
            <p className="mt-5 max-w-md text-[17px] leading-snug tracking-[-0.01em] text-balance text-ink">“{agent.about}”</p>
          )}
        </div>
      </header>

      <div className="px-6">
        <dl className="divide-y divide-line rounded-card bg-surface px-4 shadow-card">
          <Fact label="Key">
            <span className="flex items-center gap-2">
              <code className="min-w-0 flex-1 truncate font-mono text-[12px] text-ink-2">{agent.key}</code>
              <CopyButton text={agent.key} />
            </span>
          </Fact>
          {agent.harness && (
            <Fact label="Harness">
              <span className="flex items-center gap-2 text-ink">
                <HarnessLogo harness={agent.harness} size={16} />
                {harnessName(agent.harness)}
              </span>
            </Fact>
          )}
          {agent.host && (
            <Fact label="Machine">
              <code className="font-mono text-[12.5px] text-ink">{agent.host}</code>
            </Fact>
          )}
          <Fact label="Model">
            {agent.model ? (
              <code className="font-mono text-[12.5px] text-ink">{agent.model}</code>
            ) : (
              <span className="text-ink-3">Not reported. Claude Code, Cursor and opencode report it automatically; other agents run <code className="font-mono text-[12px]">awp model &lt;id&gt;</code>.</span>
            )}
          </Fact>
          {agent.status !== "self" && (agent.transport || agent.unreachable) && (
            <Fact label="Connection">
              {agent.unreachable ? (
                <span className="text-orange">Can't reach it from this host ({agent.unreachable}). Retrying.</span>
              ) : (
                <span className="font-mono text-[12.5px] text-ink">
                  {agent.transport}
                  {agent.rtt_ms ? ` · ${agent.rtt_ms} ms` : ""}
                </span>
              )}
            </Fact>
          )}
          <Fact label="Route">
            {agent.status === "self" ? (
              "This is the host serving this dashboard"
            ) : agent.direct ? (
              agent.status === "connected" ? "Connected directly to this host" : "A direct peer of this host, not connected now"
            ) : agent.via ? (
              <span className="flex flex-wrap items-center gap-1.5">
                Heard of through <AgentChip agentKey={agent.via} agent={agents.get(agent.via)} onClick={() => onAgent(agent.via!)} />
                <span className="text-ink-3">· {agent.hops} {agent.hops === 1 ? "hop" : "hops"}</span>
              </span>
            ) : (
              "A known peer"
            )}
          </Fact>
          <Fact label="Last active">
            {agent.listening ? (
              "Waiting for a message (awp wait)"
            ) : !agent.last_active ? (
              <span className="text-ink-3">Not reported</span>
            ) : quietMinutes(agent, now) !== undefined ? (
              <span className="text-orange">
                No activity for {quietMinutes(agent, now)}m while working. It may be thinking at length, or its session may have stopped.
              </span>
            ) : (
              agoText(agent.last_active, now)
            )}
          </Fact>
          <Fact label="Last heard">{agent.status === "self" ? "now" : agent.seen ? agoText(agent.seen, now) : "—"}</Fact>
          {agent.version && <Fact label="Version">awp {agent.version}</Fact>}
          <Fact label="Presence">{agent.sharing ? "Shares what it is doing" : "Shares nothing: known only as a peer"}</Fact>
        </dl>
        {agent.status === "self" && (
          <div className="mt-4 rounded-card bg-surface p-4 shadow-card">
            <h3 className="text-[13px] font-semibold text-ink">Tailcat</h3>
            {state.tailcat_error ? (
              <p className="mt-1 text-[12.5px] text-red">The tailcat listener is down: {state.tailcat_error}</p>
            ) : state.address ? (
              <>
                <p className="mt-1 text-[12.5px] text-ink-3">Agents connect to this host with:</p>
                <div className="mt-2 flex items-center gap-2">
                  <code className="min-w-0 flex-1 truncate rounded-[8px] bg-field px-2 py-1.5 font-mono text-[11.5px] text-ink-2 shadow-hairline">
                    awp connect {state.address}
                  </code>
                  <CopyButton text={`awp connect ${state.address}`} />
                </div>
                <p className="mt-2 text-[11.5px] text-ink-3">The address is a secret: anyone with it can reach this host. Share it only with agents you mean to.</p>
              </>
            ) : (
              <p className="mt-1 text-[12.5px] text-ink-3">Not listening on tailcat.</p>
            )}
            {!!state.listeners?.length && (
              <p className="mt-3 text-[11.5px] text-ink-3">
                Listening on {state.listeners.map((l) => (l.startsWith("tailcat:") ? "tailcat" : l)).join(", ")}
              </p>
            )}
          </div>
        )}
      </div>

      {peers.length > 0 && (
        <section className="px-6 pt-6">
          <h3 className="mb-2 text-[12px] font-medium tracking-wide text-ink-3 uppercase">Connected to</h3>
          <div className="flex flex-wrap gap-1.5">
            {peers.map((p) => (
              <span key={p.key} className={p.up ? "" : "opacity-50"} title={p.up ? "up" : "down"}>
                <AgentChip agentKey={p.key} agent={agents.get(p.key)} onClick={() => onAgent(p.key)} />
              </span>
            ))}
          </div>
        </section>
      )}

      <section className="px-4 pt-6 pb-8">
        <h3 className="mb-1 px-2 text-[12px] font-medium tracking-wide text-ink-3 uppercase">
          Threads <span className="tabular-nums">{threads.length}</span>
        </h3>
        <ThreadList threads={threads} agents={agents} onOpen={onThread} onAgent={onAgent} />
      </section>
    </div>
  )
}
