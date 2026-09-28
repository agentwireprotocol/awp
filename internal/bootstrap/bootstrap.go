// Package bootstrap wires awp into the agent harnesses installed on a
// machine, so the next session of each already knows how to message other
// agents. Each harness gets what it supports:
//
//	Claude Code   the whole plugin: skill, MCP server and hooks
//	Codex         skill and MCP server
//	Cursor        skill, MCP server and hooks
//	Gemini CLI    skill, MCP server and hooks
//	Copilot CLI   skill and MCP server
//	grok          skill and MCP server
//	pi            skill (pi has no MCP)
//
// Everything bootstrap writes is marked as awp's own (a plugin or skill
// named awp, hook commands running `awp hook`), so running it again
// replaces rather than duplicates, and uninstalling removes only that.
package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/agentwireprotocol/awp/plugin"
)

// Env is everything bootstrap touches, so that tests can fake it.
type Env struct {
	Home       string                                                    // the user's home directory
	ConfigHome string                                                    // $XDG_CONFIG_HOME; empty means Home/.config
	Bin        string                                                    // absolute path of the awp binary to wire in
	LookPath   func(file string) (string, error)                         // finds harness executables
	Run        func(ctx context.Context, argv ...string) (string, error) // runs harness CLIs
	DryRun     bool
}

// NewEnv returns the real environment for home, wiring in bin.
func NewEnv(home, bin string) *Env {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(configHome) {
		configHome = ""
	}
	return &Env{
		Home:       home,
		ConfigHome: configHome,
		Bin:        bin,
		LookPath:   exec.LookPath,
		Run: func(ctx context.Context, argv ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Env = append(os.Environ(), "HOME="+home)
			cmd.Stdin = nil
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), fmt.Errorf("%s: %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
			}
			return string(out), nil
		},
	}
}

func (e *Env) path(rel ...string) string {
	return filepath.Join(append([]string{e.Home}, rel...)...)
}

// configPath is a path under the XDG config directory.
func (e *Env) configPath(rel ...string) string {
	base := e.ConfigHome
	if base == "" {
		base = e.path(".config")
	}
	return filepath.Join(append([]string{base}, rel...)...)
}

// display shortens a path under Home to ~/...
func (e *Env) display(p string) string {
	if rel, err := filepath.Rel(e.Home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + rel
	}
	return p
}

// Harness is one agent harness bootstrap knows how to set up.
type Harness struct {
	ID   string   // short name, used with --harness
	Name string   // display name
	Bins []string // executables that indicate it is installed
	Dir  string   // config directory, relative to Home
	Gets string   // what bootstrap installs, for display

	MCP   bool // gets the MCP server
	Hooks bool // gets hooks (or a plugin doing their job)

	dirOf     func(*Env) string // config directory, when it is not Home/Dir
	install   func(context.Context, *Env) ([]string, error)
	uninstall func(context.Context, *Env) ([]string, error)
	status    func(*Env) Status
}

// Status is what awp has installed into a harness.
type Status struct {
	Skill bool
	MCP   bool
	Hooks bool
}

// Any reports whether anything is installed.
func (s Status) Any() bool { return s.Skill || s.MCP || s.Hooks }

// Found is a harness detected on this machine.
type Found struct {
	*Harness
	Exe     string // its executable, if on PATH
	Version string
	Status  Status
}

// Harnesses lists every harness bootstrap supports, in display order.
var Harnesses = []*Harness{
	{
		ID: "claude", Name: "Claude Code", Bins: []string{"claude"}, Dir: ".claude",
		MCP: true, Hooks: true,
		Gets:      "plugin: skill, MCP server, hooks",
		install:   installClaude,
		uninstall: uninstallClaude,
		status:    statusClaude,
	},
	{
		ID: "opencode", Name: "opencode", Bins: []string{"opencode"}, Dir: ".config/opencode",
		MCP: true, Hooks: true,
		Gets:      "skill, MCP server, plugin",
		dirOf:     opencodeDir,
		install:   installOpencode,
		uninstall: uninstallOpencode,
		status:    statusOpencode,
	},
	{
		ID: "codex", Name: "Codex", Bins: []string{"codex"}, Dir: ".codex",
		MCP: true, Hooks: false,
		Gets: "skill, MCP server",
		install: cliMCP(".codex/skills", []string{"codex", "mcp", "remove", "awp"}, func(bin string) []string {
			return []string{"codex", "mcp", "add", "awp", "--", bin, "mcp", "--harness", "codex"}
		}),
		uninstall: cliUninstall(".codex/skills", []string{"codex", "mcp", "remove", "awp"}),
		status:    tomlStatus(".codex/skills", ".codex/config.toml"),
	},
	{
		ID: "cursor", Name: "Cursor", Bins: []string{"cursor-agent", "cursor"}, Dir: ".cursor",
		MCP: true, Hooks: true,
		Gets:      "skill, MCP server, hooks",
		install:   installCursor,
		uninstall: uninstallCursor,
		status:    statusCursor,
	},
	{
		ID: "gemini", Name: "Gemini CLI", Bins: []string{"gemini"}, Dir: ".gemini",
		MCP: true, Hooks: true,
		Gets:      "skill, MCP server, hooks",
		install:   installGemini,
		uninstall: uninstallGemini,
		status:    statusGemini,
	},
	{
		ID: "copilot", Name: "GitHub Copilot CLI", Bins: []string{"copilot"}, Dir: ".copilot",
		MCP: true, Hooks: false,
		Gets: "skill, MCP server",
		install: cliMCP(".copilot/skills", []string{"copilot", "mcp", "remove", "awp"}, func(bin string) []string {
			return []string{"copilot", "mcp", "add", "awp", "--", bin, "mcp", "--harness", "copilot"}
		}),
		uninstall: cliUninstall(".copilot/skills", []string{"copilot", "mcp", "remove", "awp"}),
		status:    jsonMCPStatus(".copilot/skills", ".copilot/mcp-config.json"),
	},
	{
		ID: "grok", Name: "grok", Bins: []string{"grok"}, Dir: ".grok",
		MCP: true, Hooks: false,
		Gets: "skill, MCP server",
		install: cliMCP(".grok/skills", []string{"grok", "mcp", "remove", "awp"}, func(bin string) []string {
			return []string{"grok", "mcp", "add", "-s", "user", "awp", bin, "--", "mcp", "--harness", "grok"}
		}),
		uninstall: cliUninstall(".grok/skills", []string{"grok", "mcp", "remove", "awp"}),
		status:    tomlStatus(".grok/skills", ".grok/config.toml"),
	},
	{
		ID: "pi", Name: "pi", Bins: []string{"pi"}, Dir: ".pi",
		MCP: false, Hooks: false,
		Gets: "skill",
		install: func(ctx context.Context, e *Env) ([]string, error) {
			return installSkill(e, ".pi/agent/skills")
		},
		uninstall: func(ctx context.Context, e *Env) ([]string, error) {
			return uninstallSkill(e, ".pi/agent/skills")
		},
		status: func(e *Env) Status { return Status{Skill: hasSkill(e, ".pi/agent/skills")} },
	},
}

// Lookup returns the harness with the given ID, or nil.
func Lookup(id string) *Harness {
	for _, h := range Harnesses {
		if h.ID == id {
			return h
		}
	}
	return nil
}

// IDs lists the supported harness IDs.
func IDs() []string {
	var ids []string
	for _, h := range Harnesses {
		ids = append(ids, h.ID)
	}
	return ids
}

// Detect finds the harnesses present on this machine: those with an
// executable on PATH or a config directory in Home. Versions are read in
// parallel, since some harness CLIs take a second to start.
func Detect(ctx context.Context, e *Env) []Found {
	var found []Found
	for _, h := range Harnesses {
		f := Found{Harness: h}
		for _, b := range h.Bins {
			if p, err := e.LookPath(b); err == nil {
				f.Exe = p
				break
			}
		}
		dir := e.path(h.Dir)
		if h.dirOf != nil {
			dir = h.dirOf(e)
		}
		_, dirErr := os.Stat(dir)
		if f.Exe == "" && dirErr != nil {
			continue
		}
		f.Status = h.status(e)
		found = append(found, f)
	}
	var wg sync.WaitGroup
	for i := range found {
		if found[i].Exe == "" {
			continue
		}
		wg.Add(1)
		go func(f *Found) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, f.Exe, "--version").Output()
			if err == nil {
				f.Version = firstLine(string(out))
			}
		}(&found[i])
	}
	wg.Wait()
	return found
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(strings.TrimSpace(s), ".")
}

// Install sets awp up in the harness and describes what it did.
func (h *Harness) Install(ctx context.Context, e *Env) ([]string, error) {
	return h.install(ctx, e)
}

// Uninstall removes what Install added and describes what it did.
func (h *Harness) Uninstall(ctx context.Context, e *Env) ([]string, error) {
	return h.uninstall(ctx, e)
}

// Status reports what is installed.
func (h *Harness) Status(e *Env) Status { return h.status(e) }

// --- skills ---

const skillPath = "skills/awp/SKILL.md"

// skillText is the awp skill for harnesses that are not running it from
// the plugin: it points at the installed binary instead of the plugin's.
func skillText(bin string) ([]byte, error) {
	b, err := fs.ReadFile(plugin.FS(), skillPath)
	if err != nil {
		return nil, err
	}
	const pluginNote = "Installed as a plugin, it is on PATH. If not, it sits next to this skill at `${CLAUDE_SKILL_DIR}/../../bin/awp`."
	s := string(b)
	if !strings.Contains(s, pluginNote) {
		return nil, errors.New("SKILL.md no longer has the plugin path note; update bootstrap.skillText")
	}
	return []byte(strings.Replace(s, pluginNote, "If the command is not found, run it as `"+bin+"`.", 1)), nil
}

func installSkill(e *Env, dir string) ([]string, error) { return installSkillAt(e, e.path(dir)) }

func installSkillAt(e *Env, skills string) ([]string, error) {
	text, err := skillText(e.Bin)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(skills, "awp", "SKILL.md")
	if !e.DryRun {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, text, 0o644); err != nil {
			return nil, err
		}
	}
	return []string{"skill → " + e.display(path)}, nil
}

func hasSkill(e *Env, dir string) bool { return hasSkillAt(e.path(dir)) }

func hasSkillAt(skills string) bool {
	b, err := os.ReadFile(filepath.Join(skills, "awp", "SKILL.md"))
	return err == nil && bytes.Contains(b, []byte("name: awp"))
}

func uninstallSkill(e *Env, dir string) ([]string, error) { return uninstallSkillAt(e, e.path(dir)) }

func uninstallSkillAt(e *Env, skills string) ([]string, error) {
	if !hasSkillAt(skills) {
		return nil, nil
	}
	p := filepath.Join(skills, "awp")
	if !e.DryRun {
		if err := os.RemoveAll(p); err != nil {
			return nil, err
		}
	}
	return []string{"removed skill " + e.display(p)}, nil
}

// --- MCP through the harness's own CLI (Codex, Copilot, grok) ---

func cliMCP(skillDir string, remove []string, add func(bin string) []string) func(context.Context, *Env) ([]string, error) {
	return func(ctx context.Context, e *Env) ([]string, error) {
		did, err := installSkill(e, skillDir)
		if err != nil {
			return did, err
		}
		argv := add(e.Bin)
		if !e.DryRun {
			e.Run(ctx, remove...) // replace any earlier registration; absent is fine
			if _, err := e.Run(ctx, argv...); err != nil {
				return did, err
			}
		}
		return append(did, "MCP server → "+strings.Join(argv, " ")), nil
	}
}

func cliUninstall(skillDir string, remove []string) func(context.Context, *Env) ([]string, error) {
	return func(ctx context.Context, e *Env) ([]string, error) {
		did, err := uninstallSkill(e, skillDir)
		if err != nil {
			return did, err
		}
		if !e.DryRun {
			if _, err := e.Run(ctx, remove...); err == nil {
				did = append(did, "removed MCP server ("+strings.Join(remove, " ")+")")
			}
		}
		return did, nil
	}
}

// tomlStatus checks a TOML config for an [mcp_servers.awp] table.
func tomlStatus(skillDir, config string) func(*Env) Status {
	return func(e *Env) Status {
		b, _ := os.ReadFile(e.path(config))
		return Status{Skill: hasSkill(e, skillDir), MCP: bytes.Contains(b, []byte("[mcp_servers.awp]"))}
	}
}

// jsonMCPStatus checks a JSON config for mcpServers.awp.
func jsonMCPStatus(skillDir, config string) func(*Env) Status {
	return func(e *Env) Status {
		return Status{Skill: hasSkill(e, skillDir), MCP: jsonHasServer(e.path(config))}
	}
}

func jsonHasServer(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	json.Unmarshal(b, &cfg)
	_, ok := cfg.MCPServers["awp"]
	return ok
}

// --- Claude Code: the whole plugin, as a skills-directory plugin ---

// Claude Code loads plugins found in ~/.claude/skills/<name>/ as
// <name>@skills-dir, hooks and .mcp.json included.
func claudeDir(e *Env) string { return e.path(".claude", "skills", "awp") }

func isAWPPlugin(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		return false
	}
	var m struct{ Name string }
	return json.Unmarshal(b, &m) == nil && m.Name == "awp"
}

func installClaude(ctx context.Context, e *Env) ([]string, error) {
	dir := claudeDir(e)
	if _, err := os.Stat(dir); err == nil && !isAWPPlugin(dir) {
		return nil, fmt.Errorf("%s exists and is not the awp plugin; move it aside first", e.display(dir))
	}
	did := []string{"plugin → " + e.display(dir) + " (loads as awp@skills-dir)"}
	if e.DryRun {
		return did, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	err := fs.WalkDir(plugin.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := fs.ReadFile(plugin.FS(), p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(p, "bin/") {
			mode = 0o755
		}
		return os.WriteFile(dst, b, mode)
	})
	if err != nil {
		return nil, err
	}
	// The plugin's launcher runs libexec/awp-<os>-<arch>: point it at
	// this binary.
	lib := filepath.Join(dir, "libexec")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		return nil, err
	}
	link := filepath.Join(lib, "awp-"+runtime.GOOS+"-"+runtime.GOARCH)
	if err := os.Symlink(e.Bin, link); err != nil {
		return nil, err
	}
	return append(did, "plugin runs "+e.Bin), nil
}

func uninstallClaude(ctx context.Context, e *Env) ([]string, error) {
	dir := claudeDir(e)
	if !isAWPPlugin(dir) {
		return nil, nil
	}
	if !e.DryRun {
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
	}
	return []string{"removed plugin " + e.display(dir)}, nil
}

func statusClaude(e *Env) Status {
	ok := isAWPPlugin(claudeDir(e))
	return Status{Skill: ok, MCP: ok, Hooks: ok}
}

// --- Cursor: ~/.cursor/mcp.json and ~/.cursor/hooks.json ---

// cursorHooks maps Cursor hook events to `awp hook` events. Cursor adds
// a hook's additional_context to the conversation, and a stop hook's
// followup_message keeps the agent going.
var cursorHooks = []struct{ event, hook string }{
	{"sessionStart", "session-start"},
	{"postToolUse", "inbox"},
	{"stop", "stop"},
}

func hookCommand(bin, event, format string) string {
	return shellQuote(bin) + " hook " + event + " --format " + format
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func installCursor(ctx context.Context, e *Env) ([]string, error) {
	did, err := installSkill(e, ".cursor/skills")
	if err != nil {
		return did, err
	}
	mcp := e.path(".cursor", "mcp.json")
	if _, err := editJSON(mcp, e.DryRun, func(o *object) error {
		servers, err := o.object("mcpServers")
		if err != nil {
			return err
		}
		servers.set("awp", map[string]any{"command": e.Bin, "args": []string{"mcp", "--harness", "cursor"}})
		return o.set("mcpServers", servers)
	}); err != nil {
		return did, err
	}
	did = append(did, "MCP server → "+e.display(mcp))
	hooks := e.path(".cursor", "hooks.json")
	if _, err := editJSON(hooks, e.DryRun, func(o *object) error {
		if _, ok := o.get("version"); !ok {
			o.set("version", 1)
		}
		h, err := o.object("hooks")
		if err != nil {
			return err
		}
		for _, ch := range cursorHooks {
			entries, err := withoutAWPHooks(h, ch.event, cursorCommand)
			if err != nil {
				return err
			}
			own, _ := json.Marshal(map[string]string{"command": hookCommand(e.Bin, ch.hook, "cursor")})
			h.set(ch.event, append(entries, own))
		}
		return o.set("hooks", h)
	}); err != nil {
		return did, err
	}
	did = append(did, "hooks (sessionStart, postToolUse, stop) → "+e.display(hooks))
	if !e.DryRun {
		// Pre-approve the server so the first session does not stop to ask.
		for _, bin := range []string{"cursor-agent", "cursor"} {
			if _, err := e.LookPath(bin); err == nil {
				if _, err := e.Run(ctx, bin, "mcp", "enable", "awp"); err == nil {
					did = append(did, "approved the MCP server ("+bin+" mcp enable awp)")
				}
				break
			}
		}
	}
	return did, nil
}

func cursorCommand(entry json.RawMessage) string {
	var v struct{ Command string }
	json.Unmarshal(entry, &v)
	return v.Command
}

// hookEntries returns the entries configured for event.
func hookEntries(hooks *object, event string) ([]json.RawMessage, error) {
	raw, ok := hooks.get(event)
	if !ok {
		return nil, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("hooks.%s: %w", event, err)
	}
	return entries, nil
}

// withoutAWPHooks returns the entries under event, minus awp's own.
func withoutAWPHooks(hooks *object, event string, command func(json.RawMessage) string) ([]json.RawMessage, error) {
	entries, err := hookEntries(hooks, event)
	if err != nil {
		return nil, err
	}
	var keep []json.RawMessage
	for _, en := range entries {
		if !isAWPHook(command(en)) {
			keep = append(keep, en)
		}
	}
	return keep, nil
}

// removeAWPHooks drops awp's entries from every event in hooks.
func removeAWPHooks(o *object, command func(json.RawMessage) string) error {
	h, err := o.object("hooks")
	if err != nil {
		return err
	}
	for _, event := range append([]string(nil), h.keys...) {
		keep, err := withoutAWPHooks(h, event, command)
		if err != nil {
			return err
		}
		if len(keep) == 0 {
			h.del(event)
		} else {
			h.set(event, keep)
		}
	}
	if h.empty() {
		o.del("hooks")
		return nil
	}
	return o.set("hooks", h)
}

func hasAWPHooks(path string, command func(json.RawMessage) string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	o, err := parseObject(b)
	if err != nil {
		return false
	}
	h, err := o.object("hooks")
	if err != nil {
		return false
	}
	for _, event := range h.keys {
		entries, _ := hookEntries(h, event)
		for _, en := range entries {
			if isAWPHook(command(en)) {
				return true
			}
		}
	}
	return false
}

func uninstallCursor(ctx context.Context, e *Env) ([]string, error) {
	did, err := uninstallSkill(e, ".cursor/skills")
	if err != nil {
		return did, err
	}
	mcp := e.path(".cursor", "mcp.json")
	if changed, err := editJSON(mcp, e.DryRun, func(o *object) error {
		servers, err := o.object("mcpServers")
		if err != nil {
			return err
		}
		servers.del("awp")
		return o.set("mcpServers", servers)
	}); err != nil {
		return did, err
	} else if changed {
		did = append(did, "removed MCP server from "+e.display(mcp))
	}
	hooks := e.path(".cursor", "hooks.json")
	if changed, err := editJSON(hooks, e.DryRun, func(o *object) error { return removeAWPHooks(o, cursorCommand) }); err != nil {
		return did, err
	} else if changed {
		did = append(did, "removed hooks from "+e.display(hooks))
	}
	return did, nil
}

func statusCursor(e *Env) Status {
	return Status{
		Skill: hasSkill(e, ".cursor/skills"),
		MCP:   jsonHasServer(e.path(".cursor", "mcp.json")),
		Hooks: hasAWPHooks(e.path(".cursor", "hooks.json"), cursorCommand),
	}
}

// --- Gemini CLI: ~/.gemini/settings.json ---

// geminiHooks maps Gemini CLI hook events to `awp hook` events. Gemini
// reads additionalContext from hookSpecificOutput, like Claude Code.
var geminiHooks = []struct{ event, hook, matcher string }{
	{"SessionStart", "session-start", ""},
	{"BeforeAgent", "inbox", ""},
	{"AfterTool", "inbox", ".*"},
}

func geminiCommand(group json.RawMessage) string {
	var g struct {
		Hooks []struct{ Command string } `json:"hooks"`
	}
	json.Unmarshal(group, &g)
	for _, h := range g.Hooks {
		if isAWPHook(h.Command) {
			return h.Command
		}
	}
	return ""
}

func installGemini(ctx context.Context, e *Env) ([]string, error) {
	did, err := installSkill(e, ".gemini/skills")
	if err != nil {
		return did, err
	}
	settings := e.path(".gemini", "settings.json")
	_, err = editJSON(settings, e.DryRun, func(o *object) error {
		servers, err := o.object("mcpServers")
		if err != nil {
			return err
		}
		servers.set("awp", map[string]any{"command": e.Bin, "args": []string{"mcp", "--harness", "gemini"}})
		if err := o.set("mcpServers", servers); err != nil {
			return err
		}
		h, err := o.object("hooks")
		if err != nil {
			return err
		}
		for _, gh := range geminiHooks {
			groups, err := withoutAWPHooks(h, gh.event, geminiCommand)
			if err != nil {
				return err
			}
			group := map[string]any{"hooks": []map[string]any{{"type": "command", "command": hookCommand(e.Bin, gh.hook, "gemini"), "timeout": 10000}}}
			if gh.matcher != "" {
				group["matcher"] = gh.matcher
			}
			own, _ := json.Marshal(group)
			h.set(gh.event, append(groups, own))
		}
		return o.set("hooks", h)
	})
	if err != nil {
		return did, err
	}
	return append(did, "MCP server and hooks (SessionStart, BeforeAgent, AfterTool) → "+e.display(settings)), nil
}

func uninstallGemini(ctx context.Context, e *Env) ([]string, error) {
	did, err := uninstallSkill(e, ".gemini/skills")
	if err != nil {
		return did, err
	}
	settings := e.path(".gemini", "settings.json")
	changed, err := editJSON(settings, e.DryRun, func(o *object) error {
		servers, err := o.object("mcpServers")
		if err != nil {
			return err
		}
		servers.del("awp")
		if servers.empty() {
			o.del("mcpServers")
		} else if err := o.set("mcpServers", servers); err != nil {
			return err
		}
		return removeAWPHooks(o, geminiCommand)
	})
	if err != nil {
		return did, err
	}
	if changed {
		did = append(did, "removed MCP server and hooks from "+e.display(settings))
	}
	return did, nil
}

func statusGemini(e *Env) Status {
	settings := e.path(".gemini", "settings.json")
	return Status{Skill: hasSkill(e, ".gemini/skills"), MCP: jsonHasServer(settings), Hooks: hasAWPHooks(settings, geminiCommand)}
}
