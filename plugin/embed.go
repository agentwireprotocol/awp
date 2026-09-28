// Package plugin embeds the awp agent plugin (plugin/awp) so that
// `awp bootstrap` can install it without downloading anything. Built
// binaries under libexec/ are deliberately left out.
package plugin

import (
	"embed"
	"io/fs"
)

//go:embed all:awp/.claude-plugin all:awp/com.anthropic.claude-code awp/.mcp.json awp/skills awp/bin awp/plugin.json awp/mcp.json
var files embed.FS

// FS returns the plugin tree, rooted at the plugin directory.
func FS() fs.FS {
	sub, err := fs.Sub(files, "awp")
	if err != nil {
		panic(err)
	}
	return sub
}
