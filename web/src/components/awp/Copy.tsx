import { useState } from "react"

/** A copy-to-clipboard button that confirms. */
export function CopyButton({ text, label = "Copy", className = "" }: { text: string; label?: string; className?: string }) {
  const [done, setDone] = useState(false)
  return (
    <button
      data-sound="copy"
      type="button"
      onClick={() => {
        navigator.clipboard
          ?.writeText(text)
          .then(() => {
            setDone(true)
            setTimeout(() => setDone(false), 1400)
          })
          .catch(() => {})
      }}
      className={`inline-flex h-7 shrink-0 items-center gap-1.5 rounded-[8px] bg-surface px-2.5 text-[12px] font-medium shadow-btn transition-colors hover:bg-hover ${
        done ? "text-green" : "text-ink-2 hover:text-ink"
      } ${className}`}
    >
      {done ? (
        <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round"><path d="M20 6L9 17l-5-5" /></svg>
      ) : (
        <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><rect x="9" y="9" width="12" height="12" rx="2.5" /><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" /></svg>
      )}
      {done ? "Copied" : label}
    </button>
  )
}
