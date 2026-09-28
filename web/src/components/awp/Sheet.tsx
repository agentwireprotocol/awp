import { Dialog } from "@base-ui/react/dialog"
import type { ReactNode } from "react"

/** A panel that slides in from the right (a bottom sheet on phones). */
export function Sheet({
  open,
  onClose,
  title,
  children,
}: {
  open: boolean
  onClose: () => void
  title: string
  children: ReactNode
}) {
  return (
    <Dialog.Root open={open} onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Backdrop className="fixed inset-0 z-40 bg-[oklch(0.2_0.01_260/0.18)] backdrop-blur-[2px] transition-opacity duration-300 data-[ending-style]:opacity-0 data-[starting-style]:opacity-0 dark:bg-[oklch(0_0_0/0.45)]" />
        <Dialog.Popup
          className="fixed z-50 flex flex-col overflow-hidden bg-page shadow-overlay outline-none
            inset-x-0 bottom-0 max-h-[92svh] rounded-t-[20px] transition-transform duration-300 ease-[var(--ease-out-strong)]
            data-[starting-style]:translate-y-full data-[ending-style]:translate-y-full
            sm:inset-x-auto sm:inset-y-3 sm:right-3 sm:max-h-none sm:w-[min(600px,calc(100vw-24px))] sm:rounded-[18px]
            sm:data-[starting-style]:translate-x-[calc(100%+24px)] sm:data-[starting-style]:translate-y-0
            sm:data-[ending-style]:translate-x-[calc(100%+24px)] sm:data-[ending-style]:translate-y-0"
        >
          <Dialog.Title className="sr-only">{title}</Dialog.Title>
          <span aria-hidden className="mx-auto mt-2 h-1 w-10 shrink-0 rounded-full bg-line-strong sm:hidden" />
          <Dialog.Close
            aria-label="Close"
            className="absolute top-3.5 right-3.5 z-10 flex size-8 items-center justify-center rounded-full bg-surface text-ink-2 shadow-btn transition-colors hover:bg-hover hover:text-ink"
          >
            <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round">
              <path d="M18 6L6 18M6 6l12 12" />
            </svg>
          </Dialog.Close>
          {children}
        </Dialog.Popup>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
