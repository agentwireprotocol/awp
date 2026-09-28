// From Beautiful UI's task-rows (beautifului.dev/r/task-rows): the pieces
// awp's thread list uses.
/** A thread's lead ring: static, with an optional mark inside. (Moving
 *  indicators use loading.dev's Comet.) */
export function SpinnerRing({ children }: { children?: React.ReactNode }) {
  const size = 24, stroke = 2;
  const r = (size - stroke) / 2;
  return (
    <span className="relative inline-flex shrink-0 items-center justify-center" style={{ width: size, height: size }}>
      <svg width={size} height={size} className="absolute inset-0">
        <circle cx={size / 2} cy={size / 2} r={r} fill="none" stroke="var(--line)" strokeWidth={stroke} />
      </svg>
      {children}
    </span>
  );
}

export function Badge({ tone, children }: { tone: "red" | "green"; children: React.ReactNode }) {
  return (
    <span
      className={`flex size-5.5 shrink-0 items-center justify-center rounded-full text-white
        ${tone === "red" ? "bg-red" : "bg-green"}`}
      style={{ animation: "pop-in 300ms cubic-bezier(0.23,1,0.32,1) both" }}
    >
      {children}
    </span>
  );
}

const XIcon = (
  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" strokeLinecap="round"><path d="M18 6L6 18M6 6l12 12" /></svg>
);
const CheckIcon = (
  <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" strokeLinecap="round" strokeLinejoin="round"><path d="M20 6L9 17l-5-5" /></svg>
);

/* The badge glyphs, for rows built elsewhere. */
export function XMark() {
  return XIcon;
}
export function CheckMark() {
  return CheckIcon;
}
