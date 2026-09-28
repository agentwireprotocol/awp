package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelFromHook(t *testing.T) {
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-5","content":[]}}`,
		`{"type":"attachment"}`,
		`{"type":"assistant","message":{"model":"claude-opus-5-5","content":[]}}`,
		`{"type":"assistant","message":{"model":"<synthetic>","content":[]}}`,
		`{"type":"last-prompt"}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		input map[string]any
		want  string
	}{
		{map[string]any{}, ""},
		{map[string]any{"model": "gpt-5.5"}, "gpt-5.5"},
		{map[string]any{"model": map[string]any{"id": "gemini-3-pro", "display_name": "Gemini 3 Pro"}}, "gemini-3-pro"},
		// Claude Code: the latest real reply in the transcript (after /model, the new one).
		{map[string]any{"transcript_path": transcript}, "claude-opus-5-5"},
		{map[string]any{"transcript_path": filepath.Join(t.TempDir(), "missing.jsonl")}, ""},
	} {
		if got := modelFromHook(c.input); got != c.want {
			t.Errorf("modelFromHook(%v) = %q, want %q", c.input, got, c.want)
		}
	}
}
