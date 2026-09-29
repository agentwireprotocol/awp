// Package schema holds the published JSON Schema of the Agent Wire Protocol
// (v1/awp.schema.json) and validates lines against it.
//
// The schema is generated from the wire package by go generate ./wire; it
// is the same document that https://agentwireprotocol.com/schema/v1/awp.schema.json
// serves. A test fails when the file is older than the package.
package schema

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/agentwireprotocol/awp/wire"
)

// ID is the schema's $id.
const ID = "https://agentwireprotocol.com/schema/v1/awp.schema.json"

// V0 is the schema document.
//
//go:embed v1/awp.schema.json
var V0 []byte

// Validator checks lines and objects against the schema. It is safe for
// concurrent use.
type Validator struct {
	c        *jsonschema.Compiler
	message  *jsonschema.Schema
	envelope *jsonschema.Schema
	byType   map[string]*jsonschema.Schema

	mu   sync.Mutex
	defs map[string]*jsonschema.Schema
}

var (
	shared    *Validator
	sharedErr error
	once      sync.Once
)

// Default returns a Validator over the embedded schema, compiled once.
func Default() (*Validator, error) {
	once.Do(func() { shared, sharedErr = New(V0) })
	return shared, sharedErr
}

// New compiles a schema document. Formats (date-time) are asserted.
func New(doc []byte) (*Validator, error) {
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(ID, parsed); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	v := &Validator{c: c, byType: map[string]*jsonschema.Schema{}, defs: map[string]*jsonschema.Schema{}}
	if v.message, err = c.Compile(ID); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	if v.envelope, err = v.Def("Envelope"); err != nil {
		return nil, err
	}
	for _, m := range wire.Messages() {
		s, err := v.Def(m.Type.Name())
		if err != nil {
			return nil, err
		}
		v.byType[m.T] = s
	}
	return v, nil
}

// Def returns the compiled schema of one definition: "Hello", "Part",
// "Grant", "Presence", ...
func (v *Validator) Def(name string) (*jsonschema.Schema, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.defs[name]; ok {
		return s, nil
	}
	s, err := v.c.Compile(ID + "#/$defs/" + name)
	if err != nil {
		return nil, fmt.Errorf("schema: %s: %w", name, err)
	}
	v.defs[name] = s
	return s, nil
}

// ErrUnknownType is wrapped by Line for a line whose type the schema does
// not know. Such a line is still checked against the envelope, since
// receivers must ignore unknown types (section 8) after reading it.
var ErrUnknownType = errors.New("unknown message type")

// Line validates one line as it travels: a JSON object whose type selects
// the message definition. A known type is checked against its definition;
// an unknown type against the envelope only, and the error then wraps
// ErrUnknownType so callers can treat it as a warning.
func (v *Validator) Line(line []byte) error {
	val, err := jsonschema.UnmarshalJSON(bytes.NewReader(line))
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return errors.New("not a JSON object")
	}
	t, _ := obj["t"].(string)
	s, known := v.byType[t]
	if !known {
		if err := v.envelope.Validate(val); err != nil {
			return fmt.Errorf("%w %q: %s", ErrUnknownType, t, explain(err))
		}
		return fmt.Errorf("%w %q", ErrUnknownType, t)
	}
	if err := s.Validate(val); err != nil {
		return fmt.Errorf("%s: %s", t, explain(err))
	}
	return nil
}

// Object validates a JSON document against a named definition: a grant
// against "Grant", a presence document against "Presence".
func (v *Validator) Object(def string, doc []byte) error {
	s, err := v.Def(def)
	if err != nil {
		return err
	}
	val, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	if err := s.Validate(val); err != nil {
		return fmt.Errorf("%s: %s", def, explain(err))
	}
	return nil
}

// explain flattens a validation error to its leaves, one per line, each
// with the location in the instance it is about.
func explain(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	var out []string
	seen := map[string]bool{}
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			line := "/" + strings.Join(e.InstanceLocation, "/") + ": " + e.ErrorKind.LocalizedString(printer)
			if !seen[line] {
				seen[line] = true
				out = append(out, line)
			}
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return strings.Join(out, "; ")
}

var printer = message.NewPrinter(language.English)
