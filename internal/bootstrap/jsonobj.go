package bootstrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// object is a JSON object that keeps its key order, so editing a user's
// config file changes only what we touch.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObject() *object { return &object{vals: map[string]json.RawMessage{}} }

// parseObject decodes a JSON object. Empty input is an empty object.
func parseObject(b []byte) (*object, error) {
	o := newObject()
	if len(bytes.TrimSpace(b)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if _, dup := o.vals[k]; !dup {
			o.keys = append(o.keys, k)
		}
		o.vals[k] = v
	}
	return o, nil
}

func (o *object) get(k string) (json.RawMessage, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// object returns the member k as an object, or a new empty one.
func (o *object) object(k string) (*object, error) {
	v, ok := o.vals[k]
	if !ok || string(v) == "null" {
		return newObject(), nil
	}
	sub, err := parseObject(v)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", k, err)
	}
	return sub, nil
}

// set stores v under k, keeping k's position if it already exists.
func (o *object) set(k string, v any) error {
	var raw json.RawMessage
	switch x := v.(type) {
	case *object:
		b, err := x.marshal()
		if err != nil {
			return err
		}
		raw = b
	case json.RawMessage:
		raw = x
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		raw = b
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = raw
	return nil
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *object) empty() bool { return len(o.keys) == 0 }

// marshal encodes the object compactly, in key order.
func (o *object) marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		var c bytes.Buffer
		if err := json.Compact(&c, o.vals[k]); err != nil {
			return nil, fmt.Errorf("%q: %w", k, err)
		}
		buf.Write(c.Bytes())
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// pretty encodes the object with two-space indentation.
func (o *object) pretty() ([]byte, error) {
	b, err := o.marshal()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// editJSON loads the JSON object at path (a missing file is empty), lets
// edit change it, and writes it back if it changed. The first time awp
// edits a file it keeps a copy next to it (file.awp-backup).
func editJSON(path string, dryRun bool, edit func(*object) error) (changed bool, err error) {
	before, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	o, err := parseObject(before)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if err := edit(o); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	after, err := o.pretty()
	if err != nil {
		return false, err
	}
	if sameJSON(before, after) {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if len(before) > 0 {
		backup := path + ".awp-backup"
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(backup, before, 0o600); err != nil {
				return false, err
			}
		}
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + ".awp-tmp"
	if err := os.WriteFile(tmp, after, mode); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

func sameJSON(a, b []byte) bool {
	var x, y bytes.Buffer
	if json.Compact(&x, a) != nil || json.Compact(&y, b) != nil {
		return false
	}
	return bytes.Equal(x.Bytes(), y.Bytes())
}

// awpHook matches the hook commands bootstrap writes, whatever the
// binary is called: "<bin> hook inbox --format cursor" and the like.
var awpHook = regexp.MustCompile(`\bhook (session-start|inbox|stop) --format [a-z]+\b`)

// isAWPHook reports whether a hook command was written by bootstrap.
func isAWPHook(cmd string) bool {
	return awpHook.MatchString(cmd)
}
