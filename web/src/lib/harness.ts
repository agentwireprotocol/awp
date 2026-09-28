// Agent harness IDs, as the Go side (internal/harness) and awp bootstrap
// name them, with their display names.
const NAMES: Record<string, string> = {
  claude: "Claude Code",
  codex: "Codex",
  cursor: "Cursor",
  gemini: "Gemini CLI",
  copilot: "GitHub Copilot",
  grok: "grok",
  opencode: "opencode",
  pi: "pi",
}

/** The display name of a harness ID ("claude" is "Claude Code"). */
export function harnessName(id?: string): string | undefined {
  return id ? NAMES[id] : undefined
}
