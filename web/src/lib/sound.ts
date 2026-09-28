// Cues for live activity and for the interface itself, synthesized with
// @web-kits/audio. Off until the viewer turns them on, and throttled so a
// burst of events is one sound, not a clatter.
//
// Interface sounds: every button, link, tab and row taps when pressed. An
// element (or an ancestor) can ask for another cue with data-sound="select",
// "open", "close", "on", "off" or "copy", or for silence with
// data-sound="none". Opening and closing panels play from the code that does
// it (usePanelSound).
//
// Browsers only let audio start during a user gesture (Safari strictly so),
// so the synth library is fetched as soon as the page loads, and the audio
// context is started synchronously inside the press that needs it.
import { useEffect, useRef, useSyncExternalStore } from "react"
import type { SoundDefinition } from "@web-kits/audio"

import type { Activity } from "./types"

type Cue = "message" | "working" | "waiting" | "done" | "failed" | "joined" | "left"
export type UiCue = "tap" | "select" | "open" | "close" | "on" | "off" | "copy"

/** Overall level, applied on the master bus. Each sound's gains are set so
 *  it peaks near its target (interface -10 to -12 dBFS, events -7 to -9),
 *  measured by rendering it offline. */
export const MASTER = 1

const soft = { attack: 0.004, decay: 0.16, sustain: 0, release: 0.08 }
const click = { attack: 0.001, decay: 0.03, sustain: 0, release: 0.01 }

/** The live-activity cues. */
export const EVENT_SOUNDS: Record<Cue, SoundDefinition> = {
  // a soft droplet
  message: {
    source: { type: "sine", frequency: { start: 1180, end: 760 } },
    envelope: { attack: 0.002, decay: 0.09, sustain: 0, release: 0.05 },
    gain: 0.451,
  },
  // two notes rising
  working: {
    layers: [
      { source: { type: "triangle", frequency: 523.25 }, envelope: soft, gain: 0.404 },
      { source: { type: "triangle", frequency: 659.25 }, envelope: soft, gain: 0.367, delay: 0.07 },
    ],
  },
  // a held, questioning fifth
  waiting: {
    layers: [
      { source: { type: "sine", frequency: 587.33 }, envelope: soft, gain: 0.453 },
      { source: { type: "sine", frequency: 440 }, envelope: soft, gain: 0.371, delay: 0.09 },
    ],
  },
  // a bright major chord, arpeggiated
  done: {
    layers: [
      { source: { type: "sine", frequency: 659.25 }, envelope: { ...soft, decay: 0.3 }, gain: 0.316 },
      { source: { type: "sine", frequency: 830.61 }, envelope: { ...soft, decay: 0.3 }, gain: 0.285, delay: 0.06 },
      { source: { type: "sine", frequency: 987.77 }, envelope: { ...soft, decay: 0.4 }, gain: 0.253, delay: 0.12 },
    ],
  },
  // low and falling
  failed: {
    source: { type: "triangle", frequency: { start: 330, end: 196 } },
    filter: { type: "lowpass", frequency: 1400 },
    envelope: { attack: 0.004, decay: 0.28, sustain: 0, release: 0.08 },
    gain: 0.458,
  },
  // an airy swell
  joined: {
    layers: [
      { source: { type: "noise", color: "pink" }, filter: { type: "bandpass", frequency: 1800, resonance: 3 }, envelope: { attack: 0.05, decay: 0.25 }, gain: 0.281 },
      { source: { type: "sine", frequency: { start: 392, end: 784 } }, envelope: { attack: 0.02, decay: 0.25 }, gain: 0.394 },
    ],
  },
  left: {
    source: { type: "sine", frequency: { start: 660, end: 330 } },
    envelope: { attack: 0.02, decay: 0.3 },
    gain: 0.357,
  },
}

/** The interface cues. */
export const UI_SOUNDS: Record<UiCue, SoundDefinition> = {
  // a dry, short click
  tap: {
    layers: [
      { source: { type: "noise", color: "white" }, filter: { type: "bandpass", frequency: 3200, resonance: 1.4 }, envelope: click, gain: 0.275 },
      { source: { type: "sine", frequency: { start: 1900, end: 1300 } }, envelope: click, gain: 0.192 },
    ],
  },
  // a rounder tick, for choosing a view or a filter
  select: {
    source: { type: "triangle", frequency: { start: 880, end: 990 } },
    envelope: { attack: 0.002, decay: 0.06, sustain: 0, release: 0.03 },
    gain: 0.326,
  },
  // a soft upward swish as a panel slides in
  open: {
    layers: [
      { source: { type: "noise", color: "pink" }, filter: { type: "bandpass", frequency: 900, resonance: 2, envelope: { attack: 0.1, peak: 2600, decay: 0.05 } }, envelope: { attack: 0.03, decay: 0.12, sustain: 0, release: 0.05 }, gain: 0.315 },
      { source: { type: "sine", frequency: { start: 520, end: 780 } }, envelope: { attack: 0.01, decay: 0.1, sustain: 0, release: 0.04 }, gain: 0.315 },
    ],
  },
  // and back down as it leaves
  close: {
    layers: [
      { source: { type: "noise", color: "pink" }, filter: { type: "bandpass", frequency: 800, resonance: 2, envelope: { attack: 0, peak: 2400, decay: 0.1 } }, envelope: { attack: 0.02, decay: 0.1, sustain: 0, release: 0.04 }, gain: 0.252 },
      { source: { type: "sine", frequency: { start: 700, end: 460 } }, envelope: { attack: 0.005, decay: 0.08, sustain: 0, release: 0.03 }, gain: 0.252 },
    ],
  },
  // a switch flipping up, and down
  on: {
    layers: [
      { source: { type: "sine", frequency: 660 }, envelope: click, gain: 0.32 },
      { source: { type: "sine", frequency: 990 }, envelope: { ...click, decay: 0.05 }, gain: 0.274, delay: 0.045 },
    ],
  },
  off: {
    layers: [
      { source: { type: "sine", frequency: 990 }, envelope: click, gain: 0.326 },
      { source: { type: "sine", frequency: 660 }, envelope: { ...click, decay: 0.05 }, gain: 0.326, delay: 0.045 },
    ],
  },
  // a bright little confirmation
  copy: {
    layers: [
      { source: { type: "sine", frequency: 1318.5 }, envelope: { attack: 0.002, decay: 0.07, sustain: 0, release: 0.04 }, gain: 0.316 },
      { source: { type: "sine", frequency: 1760 }, envelope: { attack: 0.002, decay: 0.12, sustain: 0, release: 0.05 }, gain: 0.271, delay: 0.06 },
    ],
  },
}

const KEY = "awp.sound"

function readPref(): boolean {
  try {
    return localStorage.getItem(KEY) === "on"
  } catch {
    return false
  }
}

let enabled = readPref()
const listeners = new Set<() => void>()
const notify = () => listeners.forEach((fn) => fn())

type Lib = typeof import("@web-kits/audio")
type Player = () => unknown
let lib: Lib | undefined
let events: Record<Cue, Player> | undefined
let uis: Record<UiCue, Player> | undefined

// Fetch the synth now, so a press can start audio without waiting.
const libLoaded: Promise<Lib | undefined> =
  typeof window === "undefined"
    ? Promise.resolve(undefined)
    : import("@web-kits/audio").then(
        (m) => (lib = m),
        () => undefined,
      )

function players<K extends string>(defs: Record<K, SoundDefinition>, l: Lib): Record<K, Player> {
  const out = {} as Record<K, Player>
  for (const k of Object.keys(defs) as K[]) out[k] = l.defineSound(defs[k])
  return out
}

/** Starts the audio context. Call it during a user gesture; false if the
 *  library has not arrived yet. */
function start(): boolean {
  if (!lib) return false
  if (!events || !uis) {
    events = players(EVENT_SOUNDS, lib)
    uis = players(UI_SOUNDS, lib)
  }
  // Synchronous inside the gesture: creates or resumes the context.
  void lib.ensureReady().catch(() => {})
  lib.setMasterVolume(MASTER)
  return true
}

export function soundEnabled(): boolean {
  return enabled
}

export function setSound(on: boolean) {
  if (!on) ui("off")
  enabled = on
  try {
    localStorage.setItem(KEY, on ? "on" : "off")
  } catch {
    // not remembered; still works for this visit
  }
  notify()
  if (!on) return
  if (start()) {
    ui("on")
    return
  }
  // The library is still downloading: the next press will start audio.
  libLoaded.then((l) => {
    if (!l) {
      enabled = false
      notify()
    }
  })
}

export function useSound(): boolean {
  return useSyncExternalStore(
    (fn) => {
      listeners.add(fn)
      return () => {
        listeners.delete(fn)
      }
    },
    () => enabled,
  )
}

let lastAny = 0
const lastCue = new Map<Cue, number>()

function play(cue: Cue) {
  if (!enabled || !events) return
  const t = performance.now()
  if (t - lastAny < 140 || t - (lastCue.get(cue) ?? 0) < 450) return
  lastAny = t
  lastCue.set(cue, t)
  try {
    events[cue]()
  } catch {
    // audio can fail (device gone); a missing cue is fine
  }
}

let lastUi = 0

/** Plays an interface sound, when sounds are on. */
export function ui(cue: UiCue) {
  if (!enabled) return
  if (!uis && !start()) return
  const t = performance.now()
  // a tap right before a richer cue (a click that opens a panel) is enough
  if (cue === "tap" && t - lastUi < 60) return
  lastUi = t
  try {
    uis![cue]()
  } catch {
    // no audio device; silence is fine
  }
}

const PRESSABLE = 'button, a[href], [role="tab"], [role="option"], [role="menuitem"], [data-row], [data-menu-row], summary, label'

/** Taps (or data-sound cues) for every press in the page, and a gesture to
 *  start audio on when sound was left on from an earlier visit. */
export function installUiSounds() {
  if (typeof window === "undefined") return
  document.addEventListener(
    "pointerdown",
    (e) => {
      if (!enabled) return
      start()
      if (e.button !== 0) return
      const el = (e.target as Element | null)?.closest?.(PRESSABLE)
      if (!el || (el as HTMLButtonElement).disabled) return
      const cue = el.closest("[data-sound]")?.getAttribute("data-sound")
      if (cue === "none") return
      ui((cue as UiCue) || "tap")
    },
    { capture: true },
  )
  document.addEventListener("keydown", () => enabled && start(), { capture: true })
}

/** Plays "open" when a panel opens and "close" when it closes. */
export function usePanelSound(open: boolean) {
  // Compare with the last value rather than skipping the first run: React's
  // development mode runs effects twice on mount.
  const prev = useRef(open)
  useEffect(() => {
    if (prev.current === open) return
    prev.current = open
    ui(open ? "open" : "close")
  }, [open])
}

/** The cue for an activity item, if any. */
export function cueFor(a: Activity): Cue | undefined {
  switch (a.kind) {
    case "msg":
      return "message"
    case "state":
      if (a.state === "working") return "working"
      if (a.state === "waiting") return "waiting"
      if (a.state === "done") return "done"
      if (a.state === "failed") return "failed"
      return undefined
    case "error":
      return "failed"
    case "joined":
    case "connected":
      return "joined"
    case "left":
    case "disconnected":
      return "left"
  }
  return undefined
}

/** Plays the cue for live activity. Audio started by an earlier press keeps
 *  running, so these need no gesture of their own. */
export function playFor(a: Activity) {
  const cue = cueFor(a)
  if (cue) play(cue)
}
