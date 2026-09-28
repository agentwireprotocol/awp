package plugin

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedPlugin(t *testing.T) {
	var files []string
	fs.WalkDir(FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	have := strings.Join(files, "\n")
	for _, want := range []string{
		".claude-plugin/plugin.json",
		".mcp.json",
		"com.anthropic.claude-code/hooks.json",
		"skills/awp/SKILL.md",
		"bin/awp",
		"plugin.json",
		"mcp.json",
	} {
		if !strings.Contains(have, want) {
			t.Errorf("embedded plugin is missing %s", want)
		}
	}
	if strings.Contains(have, "libexec/") {
		t.Errorf("built binaries must not be embedded:\n%s", have)
	}
}
