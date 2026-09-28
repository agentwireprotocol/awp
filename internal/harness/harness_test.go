package harness

import "testing"

func TestDetect(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{nil, ""},
		{map[string]string{"CLAUDECODE": "1"}, Claude},
		{map[string]string{"OPENCODE": "1", "AGENT": "1"}, Opencode},
		{map[string]string{"GEMINI_CLI": "1"}, Gemini},
		{map[string]string{"CURSOR_AGENT": "1"}, Cursor},
		{map[string]string{"COPILOT_CLI": "1"}, Copilot},
		{map[string]string{"PI_CODING_AGENT": "true", "AI_AGENT": "pi"}, Pi},
		{map[string]string{"CODEX_THREAD_ID": "019a"}, Codex},
		// Codex started from inside Claude Code: the inner one wins.
		{map[string]string{"CLAUDECODE": "1", "CODEX_THREAD_ID": "019a"}, Codex},
		{map[string]string{"CLAUDECODE": "0"}, ""},
	} {
		if got := Detect(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("Detect(%v) = %q, want %q", c.env, got, c.want)
		}
	}
}

func TestFromName(t *testing.T) {
	for name, want := range map[string]string{
		"claude-code@worker": Claude, "Codex@builder": Codex, "gemini-cli@x": Gemini,
		"awp@host": "", "cursor": Cursor, "": "",
	} {
		if got := FromName(name); got != want {
			t.Errorf("FromName(%q) = %q, want %q", name, got, want)
		}
	}
}
