package main

import (
	"bytes"
	"os"
	"testing"
)

// TestGenerated fails when the checked-in schema files are not what the
// wire package now describes: run go generate ./wire.
func TestGenerated(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	out, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{"schema/v1/awp.schema.json": out.JSON, "schema/v1/schema.mdx": out.MDX} {
		have, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(have, want) {
			t.Errorf("%s is stale: run go generate ./wire", path)
		}
	}
}
