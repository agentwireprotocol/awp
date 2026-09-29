// Command schemagen writes the JSON Schema of the Agent Wire Protocol from
// the wire package, which is the source of truth: every message type, its
// fields, their constraints and the doc comments come from there.
//
//	go run ./internal/schemagen            # writes schema/v0/awp.schema.json and schema.mdx
//	go run ./internal/schemagen -check     # fails if either file is stale
//
// It must run from the repository root (go generate ./wire and make schema
// do), since the comments are read from the wire package's source.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func main() {
	jsonOut := flag.String("json", "schema/v0/awp.schema.json", "schema file to write")
	mdxOut := flag.String("mdx", "schema/v0/schema.mdx", "reference page to write")
	check := flag.Bool("check", false, "write nothing; exit 1 if a file is not what would be generated")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		fatal(err)
	}
	out, err := generate()
	if err != nil {
		fatal(err)
	}
	stale := false
	for _, f := range []struct {
		path string
		data []byte
	}{{*jsonOut, out.JSON}, {*mdxOut, out.MDX}} {
		if *check {
			have, err := os.ReadFile(f.path)
			if err != nil || !bytes.Equal(have, f.data) {
				fmt.Fprintf(os.Stderr, "%s is stale: run go generate ./wire\n", f.path)
				stale = true
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(f.path, f.data, 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote %s (%d bytes)\n", f.path, len(f.data))
	}
	if stale {
		os.Exit(1)
	}
	if *check {
		fmt.Println("schema files are up to date")
	}
}

// repoRoot is the repository root, found from this file's location, so
// that the generator works from any working directory.
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate the repository root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "schemagen:", err)
	os.Exit(1)
}
