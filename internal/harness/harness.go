// Package harness names the agent harness an awp agent runs in (Claude
// Code, Codex, ...), so dashboards can show which is which. The IDs are the
// ones awp bootstrap uses.
package harness

import "strings"

// Known harness IDs.
const (
	Claude   = "claude"
	Opencode = "opencode"
	Codex    = "codex"
	Cursor   = "cursor"
	Gemini   = "gemini"
	Copilot  = "copilot"
	Grok     = "grok"
	Pi       = "pi"
)

// IDs lists the known harness IDs.
func IDs() []string {
	return []string{Claude, Opencode, Codex, Cursor, Gemini, Copilot, Grok, Pi}
}

// aliases maps the spellings people use (in agent names like
// "claude-code@host", or in --harness) to an ID.
var aliases = map[string]string{
	"claude": Claude, "claude-code": Claude, "claudecode": Claude, "claude_code": Claude,
	"opencode": Opencode, "open-code": Opencode,
	"codex": Codex, "codex-cli": Codex, "openai-codex": Codex,
	"cursor": Cursor, "cursor-agent": Cursor, "cursor-cli": Cursor,
	"gemini": Gemini, "gemini-cli": Gemini, "geminicli": Gemini,
	"copilot": Copilot, "copilot-cli": Copilot, "github-copilot": Copilot, "gh-copilot": Copilot,
	"grok": Grok, "grok-cli": Grok,
	"pi": Pi, "pi-agent": Pi, "pi-coding-agent": Pi,
}

// Normalize returns the ID for a harness name, or "" if it is not one
// awp knows.
func Normalize(s string) string {
	return aliases[strings.ToLower(strings.TrimSpace(s))]
}

// FromName guesses the harness from an agent name such as
// "claude-code@worker": the part before the @.
func FromName(name string) string {
	prefix, _, _ := strings.Cut(name, "@")
	return Normalize(prefix)
}

// markers are the environment variables each harness sets for the commands
// its agent runs. Harnesses launched from inside another inherit its
// variables too, so the ones most often found wrapping others come last.
var markers = []struct{ id, key string }{
	{Codex, "CODEX_THREAD_ID"},
	{Codex, "CODEX_SANDBOX"},
	{Cursor, "CURSOR_AGENT"},
	{Gemini, "GEMINI_CLI"},
	{Copilot, "COPILOT_CLI"},
	{Opencode, "OPENCODE"},
	{Pi, "PI_CODING_AGENT"},
	{Claude, "CLAUDECODE"},
}

// Detect finds the harness this process runs under from its environment,
// or "".
func Detect(getenv func(string) string) string {
	if id := Normalize(getenv("AI_AGENT")); id != "" {
		return id
	}
	for _, m := range markers {
		if v := getenv(m.key); v != "" && v != "0" && v != "false" {
			return m.id
		}
	}
	return ""
}
