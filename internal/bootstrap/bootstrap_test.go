package bootstrap

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const bin = "/opt/awp/bin/awp"

// testEnv is a fake machine: a temporary home, the given executables "on
// PATH", and a runner that records harness CLI calls.
func testEnv(t *testing.T, exes ...string) (*Env, *[][]string) {
	t.Helper()
	calls := &[][]string{}
	e := &Env{
		Home: t.TempDir(),
		Bin:  bin,
		LookPath: func(f string) (string, error) {
			for _, x := range exes {
				if x == f {
					return "/nonexistent/bin/" + f, nil
				}
			}
			return "", exec.ErrNotFound
		},
		Run: func(ctx context.Context, argv ...string) (string, error) {
			*calls = append(*calls, argv)
			return "", nil
		},
	}
	return e, calls
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDetect(t *testing.T) {
	e, _ := testEnv(t, "codex")
	for _, d := range []string{".claude", ".cursor", ".pi/agent", ".config/opencode"} {
		os.MkdirAll(filepath.Join(e.Home, d), 0o755)
	}
	var ids []string
	for _, f := range Detect(context.Background(), e) {
		ids = append(ids, f.ID)
	}
	if want := []string{"claude", "opencode", "codex", "cursor", "pi"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("detected %v, want %v", ids, want)
	}
}

func TestClaudePlugin(t *testing.T) {
	e, _ := testEnv(t, "claude")
	ctx := context.Background()
	h := Lookup("claude")
	if _, err := h.Install(ctx, e); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Home, ".claude/skills/awp")
	for _, f := range []string{".claude-plugin/plugin.json", ".mcp.json", "com.anthropic.claude-code/hooks.json", "skills/awp/SKILL.md", "plugin.json", "mcp.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("plugin is missing %s", f)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "bin/awp")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("launcher not executable: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "libexec/awp-*"))
	if len(matches) != 1 {
		t.Fatalf("libexec: %v", matches)
	}
	if target, _ := os.Readlink(matches[0]); target != bin {
		t.Fatalf("libexec link points at %q", target)
	}
	if s := h.Status(e); !s.Skill || !s.MCP || !s.Hooks {
		t.Fatalf("status %+v", s)
	}
	// Installing again replaces the plugin cleanly.
	if _, err := h.Install(ctx, e); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if _, err := h.Uninstall(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("plugin still there after uninstall")
	}
	// Someone else's skill directory named awp is left alone.
	write(t, filepath.Join(dir, "SKILL.md"), "---\nname: not-ours\n---\n")
	if _, err := h.Install(ctx, e); err == nil {
		t.Fatal("overwrote a foreign directory")
	}
	if did, _ := h.Uninstall(ctx, e); len(did) != 0 {
		t.Fatalf("uninstall touched a foreign directory: %v", did)
	}
}

func TestCLIHarnesses(t *testing.T) {
	e, calls := testEnv(t, "codex", "grok", "copilot")
	ctx := context.Background()
	for _, id := range []string{"codex", "grok", "copilot"} {
		if _, err := Lookup(id).Install(ctx, e); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	skill := read(t, filepath.Join(e.Home, ".codex/skills/awp/SKILL.md"))
	if strings.Contains(skill, "CLAUDE_SKILL_DIR") || !strings.Contains(skill, "run it as `"+bin+"`") {
		t.Fatalf("skill not rendered for a standalone install:\n%.400s", skill)
	}
	want := [][]string{
		{"codex", "mcp", "remove", "awp"},
		{"codex", "mcp", "add", "awp", "--", bin, "mcp", "--harness", "codex"},
		{"grok", "mcp", "remove", "awp"},
		{"grok", "mcp", "add", "-s", "user", "awp", bin, "--", "mcp", "--harness", "grok"},
		{"copilot", "mcp", "remove", "awp"},
		{"copilot", "mcp", "add", "awp", "--", bin, "mcp", "--harness", "copilot"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", *calls, want)
	}
	if !Lookup("codex").Status(e).Skill {
		t.Fatal("codex skill not detected")
	}
}

const cursorHooksBefore = `{
  "version": 1,
  "hooks": {
    "postToolUse": [
      {
        "command": ".sprite-shared/hooks/sprite-env-check.sh --format cursor postToolUse",
        "matcher": "Shell|Write|Edit"
      }
    ]
  }
}
`

func TestCursorKeepsUserConfig(t *testing.T) {
	e, _ := testEnv(t, "cursor-agent")
	ctx := context.Background()
	hooks := filepath.Join(e.Home, ".cursor/hooks.json")
	mcp := filepath.Join(e.Home, ".cursor/mcp.json")
	write(t, hooks, cursorHooksBefore)
	write(t, mcp, `{"zeta": true, "mcpServers": {"other": {"command": "x"}}, "alpha": 1}`)
	h := Lookup("cursor")
	for i := 0; i < 2; i++ { // twice: idempotent
		if _, err := h.Install(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var cfg struct {
		Hooks map[string][]struct{ Command, Matcher string } `json:"hooks"`
	}
	json.Unmarshal([]byte(read(t, hooks)), &cfg)
	post := cfg.Hooks["postToolUse"]
	if len(post) != 2 || post[0].Matcher != "Shell|Write|Edit" || post[1].Command != bin+" hook inbox --format cursor" {
		t.Fatalf("postToolUse %+v", post)
	}
	if len(cfg.Hooks["sessionStart"]) != 1 || len(cfg.Hooks["stop"]) != 1 {
		t.Fatalf("hooks %+v", cfg.Hooks)
	}
	m := read(t, mcp)
	if !(strings.Index(m, "zeta") < strings.Index(m, "mcpServers") && strings.Index(m, "mcpServers") < strings.Index(m, "alpha")) {
		t.Fatalf("key order not kept:\n%s", m)
	}
	if !strings.Contains(m, `"other"`) || !strings.Contains(m, `"awp"`) {
		t.Fatalf("mcp.json:\n%s", m)
	}
	if read(t, hooks+".awp-backup") != cursorHooksBefore {
		t.Fatal("backup is not the original file")
	}
	if s := h.Status(e); !s.Skill || !s.MCP || !s.Hooks {
		t.Fatalf("status %+v", s)
	}
	if _, err := h.Uninstall(ctx, e); err != nil {
		t.Fatal(err)
	}
	if !sameJSON([]byte(read(t, hooks)), []byte(cursorHooksBefore)) {
		t.Fatalf("hooks after uninstall:\n%s", read(t, hooks))
	}
	if strings.Contains(read(t, mcp), "awp") || !strings.Contains(read(t, mcp), "other") {
		t.Fatalf("mcp.json after uninstall:\n%s", read(t, mcp))
	}
}

func TestGeminiKeepsUserConfig(t *testing.T) {
	e, _ := testEnv(t, "gemini")
	ctx := context.Background()
	settings := filepath.Join(e.Home, ".gemini/settings.json")
	before := `{"general": {"checkpointing": {"enabled": true}}, "hooks": {"AfterTool": [{"matcher": "^(write_file)$", "hooks": [{"type": "command", "command": "check.sh"}]}]}}`
	write(t, settings, before)
	h := Lookup("gemini")
	for i := 0; i < 2; i++ {
		if _, err := h.Install(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Command string }
		} `json:"hooks"`
	}
	json.Unmarshal([]byte(read(t, settings)), &cfg)
	if s := cfg.MCPServers["awp"]; s.Command != bin || !reflect.DeepEqual(s.Args, []string{"mcp", "--harness", "gemini"}) {
		t.Fatalf("mcpServers %+v", cfg.MCPServers)
	}
	after := cfg.Hooks["AfterTool"]
	if len(after) != 2 || after[0].Hooks[0].Command != "check.sh" || after[1].Matcher != ".*" {
		t.Fatalf("AfterTool %+v", after)
	}
	if len(cfg.Hooks["SessionStart"]) != 1 || len(cfg.Hooks["BeforeAgent"]) != 1 {
		t.Fatalf("hooks %+v", cfg.Hooks)
	}
	if _, err := h.Uninstall(ctx, e); err != nil {
		t.Fatal(err)
	}
	if !sameJSON([]byte(read(t, settings)), []byte(before)) {
		t.Fatalf("settings after uninstall:\n%s", read(t, settings))
	}
}

func TestDryRun(t *testing.T) {
	e, calls := testEnv(t, "codex", "cursor-agent")
	e.DryRun = true
	for _, id := range []string{"claude", "opencode", "codex", "cursor", "gemini", "pi"} {
		did, err := Lookup(id).Install(context.Background(), e)
		if err != nil || len(did) == 0 {
			t.Fatalf("%s: %v %v", id, did, err)
		}
	}
	entries, _ := os.ReadDir(e.Home)
	if len(entries) != 0 || len(*calls) != 0 {
		t.Fatalf("dry run wrote %v and ran %v", entries, *calls)
	}
}

func TestOpencode(t *testing.T) {
	e, _ := testEnv(t, "opencode")
	ctx := context.Background()
	dir := filepath.Join(e.Home, ".config/opencode")
	cfg := filepath.Join(dir, "opencode.json")
	write(t, cfg, `{"model": "anthropic/claude-sonnet-5", "mcp": {"other": {"type": "local", "command": ["x"]}}}`)
	h := Lookup("opencode")
	for i := 0; i < 2; i++ {
		if _, err := h.Install(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var c struct {
		Model string
		MCP   map[string]struct {
			Type    string
			Command []string
			Enabled bool
		} `json:"mcp"`
	}
	json.Unmarshal([]byte(read(t, cfg)), &c)
	if s := c.MCP["awp"]; c.Model == "" || c.MCP["other"].Type != "local" || s.Type != "local" || !reflect.DeepEqual(s.Command, []string{bin, "mcp", "--harness", "opencode"}) || !s.Enabled {
		t.Fatalf("opencode.json:\n%s", read(t, cfg))
	}
	plugin := read(t, filepath.Join(dir, "plugins/awp.js"))
	if !strings.Contains(plugin, `const BIN = "`+bin+`"`) || !strings.Contains(plugin, `"tool.execute.after"`) {
		t.Fatalf("plugin:\n%s", plugin)
	}
	if !strings.Contains(read(t, filepath.Join(dir, "skills/awp/SKILL.md")), "name: awp") {
		t.Fatal("skill missing")
	}
	if st := h.Status(e); !st.Skill || !st.MCP || !st.Hooks {
		t.Fatalf("status %+v", st)
	}
	if _, err := h.Uninstall(ctx, e); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, cfg), "awp") || !strings.Contains(read(t, cfg), "other") {
		t.Fatalf("opencode.json after uninstall:\n%s", read(t, cfg))
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins/awp.js")); !os.IsNotExist(err) {
		t.Fatal("plugin left behind")
	}
}

// A user's opencode.json with comments is never rewritten: awp goes into
// opencode.jsonc, which opencode merges with it.
func TestOpencodeJSONC(t *testing.T) {
	e, _ := testEnv(t, "opencode")
	e.ConfigHome = filepath.Join(e.Home, "xdg")
	dir := filepath.Join(e.ConfigHome, "opencode")
	withComments := "{\n  // my settings\n  \"theme\": \"tokyonight\",\n}\n"
	write(t, filepath.Join(dir, "opencode.json"), withComments)
	if _, err := Lookup("opencode").Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "opencode.json")) != withComments {
		t.Fatal("rewrote a config file with comments")
	}
	if !opencodeHasServer(filepath.Join(dir, "opencode.jsonc")) {
		t.Fatalf("opencode.jsonc:\n%s", read(t, filepath.Join(dir, "opencode.jsonc")))
	}
	// With both files taken there is nowhere safe to write: say what to add.
	os.Remove(filepath.Join(dir, "opencode.jsonc"))
	write(t, filepath.Join(dir, "opencode.jsonc"), withComments)
	if _, err := Lookup("opencode").Install(context.Background(), e); err == nil || !strings.Contains(err.Error(), "add awp") {
		t.Fatalf("expected manual instructions, got %v", err)
	}
}

// The plugin must be valid JavaScript (checked with node when available).
func TestOpencodePluginSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	f := filepath.Join(t.TempDir(), "awp.mjs")
	os.WriteFile(f, opencodePlugin(`/opt/my "tools"/awp`), 0o644)
	if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}

func TestHookOwnership(t *testing.T) {
	for cmd, ours := range map[string]bool{
		"/usr/local/bin/awp hook inbox --format cursor":            true,
		"'/opt/my tools/hb' hook session-start --format gemini":    true,
		"/x/renamed hook stop --format cursor":                     true,
		".sprite-shared/hooks/sprite-env-check.sh --format cursor": false,
		"awp hook":        false,
		"echo hook inbox": false,
	} {
		if isAWPHook(cmd) != ours {
			t.Errorf("isAWPHook(%q) = %v", cmd, !ours)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/bin/awp": "/usr/local/bin/awp",
		"/Users/a b/bin/awp": "'/Users/a b/bin/awp'",
		"/tmp/it's/awp":      `'/tmp/it'\''s/awp'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
