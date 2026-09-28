import type { State } from "@/lib/types"

import { CopyButton } from "./Copy"
import { Orb } from "./Orb"

/** The host is alone: how to bring the network into view. */
export function EmptyNetwork({ state }: { state: State }) {
  const connect = state.address ? `awp connect ${state.address}` : ""
  return (
    <div className="flex h-full flex-col items-center justify-center px-6 py-10 text-center">
      <div className="relative">
        <span className="absolute inset-0 rounded-full" style={{ boxShadow: "0 0 0 1px var(--line-strong)", animation: "orb-ping 2.4s ease-out infinite" }} />
        <Orb agentKey={state.self} size={72} harness={state.agents.find((a) => a.key === state.self)?.harness} />
      </div>
      <h3 className="mt-6 text-[19px] font-semibold tracking-[-0.02em] text-ink">Just this host so far</h3>
      <p className="mt-1.5 max-w-md text-[13.5px] leading-relaxed text-ink-2">
        Agents show up here when they connect to this host, or to anyone connected to it, with presence turned on. Presence travels across the network, so one host sees everyone.
      </p>
      <div className="mt-5 w-full max-w-lg space-y-2 text-left">
        <div className="rounded-[12px] bg-surface p-3 shadow-card">
          <p className="mb-1.5 text-[11.5px] font-medium text-ink-3">On each agent's machine</p>
          <code className="block font-mono text-[12.5px] text-ink">awp up --presence</code>
        </div>
        {connect && (
          <div className="rounded-[12px] bg-surface p-3 shadow-card">
            <div className="mb-1.5 flex items-center justify-between gap-2">
              <p className="text-[11.5px] font-medium text-ink-3">Then connect to this host</p>
              <CopyButton text={connect} />
            </div>
            <code className="block truncate font-mono text-[12.5px] text-ink" title={connect}>
              {connect}
            </code>
          </div>
        )}
        {!state.presence && (
          <p className="px-1 text-[12px] text-orange">
            This host doesn't share its own presence. Restart it with <code className="font-mono">awp up --presence</code> so others can see it.
          </p>
        )}
      </div>
    </div>
  )
}
