package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// opencode reads skills from ~/.config/opencode/skills, MCP servers from
// the "mcp" section of its global config, and JS plugins from
// ~/.config/opencode/plugins. It merges opencode.json and opencode.jsonc,
// so awp never has to rewrite a config file that has comments in it.

func opencodeDir(e *Env) string { return e.configPath("opencode") }

// opencodePluginMarker identifies the plugin file bootstrap writes.
const opencodePluginMarker = "Written by `awp bootstrap`"

// opencodePlugin appends new awp messages to the output of each tool
// call, the way post-tool hooks do in other harnesses, and tells awp which
// model each chat turn runs on, so a model switch reaches the network.
func opencodePlugin(bin string) []byte {
	quoted, _ := json.Marshal(bin)
	return []byte(`// awp: messages from other agents show up after tool calls, and the
// network sees which model this agent runs on.
// ` + opencodePluginMarker + `; ` + "`awp bootstrap --uninstall`" + ` removes it.
const BIN = ` + string(quoted) + `;

export const AWP = async ({ $ }) => {
  let reported = "";
  return {
    "chat.params": async (input) => {
      const model = input.model?.api?.id || input.model?.id || "";
      if (!model || model === reported) return;
      // Fails quietly while awp is not running; the next turn retries.
      const r = await $` + "`${BIN} model ${model}`" + `.nothrow().quiet();
      if (r.exitCode === 0) reported = model;
    },
    "tool.execute.after": async (input, output) => {
      const text = await $` + "`echo '{}' | ${BIN} hook inbox --format text`" + `.nothrow().quiet().text();
      if (text.trim()) output.output = ` + "`${output.output ?? \"\"}\\n\\n${text.trim()}`" + `;
    },
  };
};
`)
}

// opencodeConfig picks the config file to edit: opencode.json, unless that
// has comments (JSONC) and there is no opencode.jsonc yet, in which case a
// fresh opencode.jsonc holds awp's entry and opencode merges the two.
func opencodeConfig(e *Env) (string, error) {
	dir := opencodeDir(e)
	main := filepath.Join(dir, "opencode.json")
	b, err := os.ReadFile(main)
	if errors.Is(err, os.ErrNotExist) || err == nil && (len(bytes.TrimSpace(b)) == 0 || json.Valid(b)) {
		return main, nil
	}
	if err != nil {
		return "", err
	}
	alt := filepath.Join(dir, "opencode.jsonc")
	if _, err := os.Stat(alt); errors.Is(err, os.ErrNotExist) {
		return alt, nil
	}
	return "", fmt.Errorf("%s has comments and %s exists; add awp to its \"mcp\" section yourself: \"awp\": {\"type\": \"local\", \"command\": [%q, \"mcp\"], \"enabled\": true}",
		e.display(main), e.display(alt), e.Bin)
}

func installOpencode(ctx context.Context, e *Env) ([]string, error) {
	dir := opencodeDir(e)
	did, err := installSkillAt(e, filepath.Join(dir, "skills"))
	if err != nil {
		return did, err
	}
	cfg, err := opencodeConfig(e)
	if err != nil {
		return did, err
	}
	if _, err := editJSON(cfg, e.DryRun, func(o *object) error {
		if o.empty() {
			o.set("$schema", "https://opencode.ai/config.json")
		}
		mcp, err := o.object("mcp")
		if err != nil {
			return err
		}
		mcp.set("awp", map[string]any{"type": "local", "command": []string{e.Bin, "mcp", "--harness", "opencode"}, "enabled": true})
		return o.set("mcp", mcp)
	}); err != nil {
		return did, err
	}
	did = append(did, "MCP server → "+e.display(cfg))
	plugin := filepath.Join(dir, "plugins", "awp.js")
	if !e.DryRun {
		if err := os.MkdirAll(filepath.Dir(plugin), 0o755); err != nil {
			return did, err
		}
		if err := os.WriteFile(plugin, opencodePlugin(e.Bin), 0o644); err != nil {
			return did, err
		}
	}
	return append(did, "plugin (messages after tool calls) → "+e.display(plugin)), nil
}

func uninstallOpencode(ctx context.Context, e *Env) ([]string, error) {
	dir := opencodeDir(e)
	did, err := uninstallSkillAt(e, filepath.Join(dir, "skills"))
	if err != nil {
		return did, err
	}
	for _, name := range []string{"opencode.json", "opencode.jsonc"} {
		cfg := filepath.Join(dir, name)
		if !opencodeHasServer(cfg) {
			continue
		}
		changed, err := editJSON(cfg, e.DryRun, func(o *object) error {
			mcp, err := o.object("mcp")
			if err != nil {
				return err
			}
			mcp.del("awp")
			if mcp.empty() {
				o.del("mcp")
				return nil
			}
			return o.set("mcp", mcp)
		})
		if err != nil {
			return did, err
		}
		if changed {
			did = append(did, "removed MCP server from "+e.display(cfg))
		}
	}
	plugin := filepath.Join(dir, "plugins", "awp.js")
	if b, err := os.ReadFile(plugin); err == nil && bytes.Contains(b, []byte(opencodePluginMarker)) {
		if !e.DryRun {
			if err := os.Remove(plugin); err != nil {
				return did, err
			}
		}
		did = append(did, "removed plugin "+e.display(plugin))
	}
	return did, nil
}

// opencodeHasServer reports whether a (plain JSON) opencode config has an
// mcp.awp entry. Files with comments are the user's and are skipped.
func opencodeHasServer(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil || !json.Valid(b) {
		return false
	}
	var cfg struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	json.Unmarshal(b, &cfg)
	_, ok := cfg.MCP["awp"]
	return ok
}

func statusOpencode(e *Env) Status {
	dir := opencodeDir(e)
	b, _ := os.ReadFile(filepath.Join(dir, "plugins", "awp.js"))
	return Status{
		Skill: hasSkillAt(filepath.Join(dir, "skills")),
		MCP:   opencodeHasServer(filepath.Join(dir, "opencode.json")) || opencodeHasServer(filepath.Join(dir, "opencode.jsonc")),
		Hooks: bytes.Contains(b, []byte(opencodePluginMarker)),
	}
}
