// Agents' avatars: Boring Avatars' "marble", drawn from the agent's key, so
// the same agent looks the same on every host and visit.
//
// Every agent gets its own palette: awp's palette turned round the colour
// wheel by an amount taken from its key. The turn keeps each colour's
// lightness and chroma, so all palettes feel related while agents differ at
// a glance (one agent in twelve keeps its hues), and the colour that leads
// the palette, which dominates the marble, varies too.

export const PALETTE = ["#00686c", "#32c2b9", "#edecb3", "#fad928", "#ff9915"]

// The palette's strong colours: the cream and the yellow vanish as a thin
// ring or a moving dot on a light page.
const STRONG = [0, 1, 4]

function hash(s: string): number {
  let h = 2166136261
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i)
    h = Math.imul(h, 16777619)
  }
  return h >>> 0
}

// --- sRGB ⇄ OKLCH (Björn Ottosson's OKLab) ---

type Lch = [number, number, number]

function toLinear(c: number): number {
  return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
}

function fromLinear(c: number): number {
  const v = c <= 0.0031308 ? c * 12.92 : 1.055 * c ** (1 / 2.4) - 0.055
  return Math.min(1, Math.max(0, v))
}

function hexToOklch(hex: string): Lch {
  const n = parseInt(hex.slice(1), 16)
  const [r, g, b] = [(n >> 16) & 255, (n >> 8) & 255, n & 255].map((c) => toLinear(c / 255))
  const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b)
  const m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b)
  const s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b)
  const L = 0.2104542553 * l + 0.793617785 * m - 0.0040720468 * s
  const A = 1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s
  const B = 0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s
  return [L, Math.hypot(A, B), (Math.atan2(B, A) * 180) / Math.PI]
}

function oklchToHex([L, C, H]: Lch): string {
  const a = C * Math.cos((H * Math.PI) / 180)
  const b = C * Math.sin((H * Math.PI) / 180)
  const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3
  const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3
  const s = (L - 0.0894841775 * a - 1.291485548 * b) ** 3
  const rgb = [
    4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s,
    -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s,
    -0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s,
  ].map((c) => Math.round(fromLinear(c) * 255))
  return "#" + rgb.map((c) => c.toString(16).padStart(2, "0")).join("")
}

const BASE = PALETTE.map(hexToOklch)
const cache = new Map<string, string[]>()

/** An agent's palette: awp's, turned by an amount from its key. */
export function agentPalette(key: string): string[] {
  const hit = cache.get(key)
  if (hit) return hit
  const h = hash(key || "?")
  const turn = (h % 12) * 30 // twelve related palettes, 30° apart
  const turned = BASE.map(([L, C, H]) => oklchToHex([L, C, H + turn]))
  // Which colour leads decides which dominates the marble: five more
  // variations of each palette.
  const lead = (h >>> 4) % turned.length
  const palette = [...turned.slice(lead), ...turned.slice(0, lead)]
  cache.set(key, palette)
  return palette
}

/** One colour for an agent, for its pings and the pulses it sends. */
export function agentColor(key: string): string {
  return agentPalette(key)[STRONG[(hash(key || "?") >>> 8) % STRONG.length]]
}
