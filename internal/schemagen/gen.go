package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"

	"github.com/agentwireprotocol/awp/wire"
)

// Module is the Go module the wire package lives in; ID is the schema's $id.
const (
	Module = "github.com/agentwireprotocol/awp"
	ID     = "https://agentwireprotocol.com/schema/v0/awp.schema.json"
)

// required adds, per message type, the envelope fields the spec requires
// that the Go types leave optional because other messages do without them.
var required = map[string][]string{
	wire.TMsg:     {"th"},
	wire.TState:   {"th"},
	wire.TAck:     {"th", "re"},
	wire.TPong:    {"re"},
	wire.TMirror:  {"th"},
	wire.TPrivate: {"th"},
}

// generate builds the schema. The working directory must be the repository
// root, so that the wire package's comments can be read.
func generate() (*Output, error) {
	r := &jsonschema.Reflector{
		// Unknown fields must be ignored (section 5), so no object is closed.
		AllowAdditionalProperties: true,
		ExpandedStruct:            true,
		Anonymous:                 true,
	}
	if err := r.AddGoComments(Module, "./wire", jsonschema.WithFullComment()); err != nil {
		return nil, fmt.Errorf("reading wire comments: %w", err)
	}

	defs := jsonschema.Definitions{}
	reflectType := func(t reflect.Type) *jsonschema.Schema {
		s := r.ReflectFromType(t)
		for k, v := range s.Definitions {
			if _, done := defs[k]; !done {
				defs[k] = v
			}
		}
		s.Definitions, s.Version = nil, ""
		describeFields(t, s)
		return s
	}

	// The envelope on its own, for lines whose type is unknown.
	defs["Envelope"] = reflectType(reflect.TypeFor[wire.Envelope]())

	// One definition per message type, discriminated by t.
	var variants []*jsonschema.Schema
	extensions := wire.Extensions()
	for _, m := range wire.Messages() {
		s := reflectType(m.Type)
		s.Title = m.T
		t, ok := s.Properties.Get("t")
		if !ok {
			return nil, fmt.Errorf("%s has no t", m.Type.Name())
		}
		t.Const = m.T
		for _, f := range required[m.T] {
			if !slices.Contains(s.Required, f) {
				s.Required = append(s.Required, f)
			}
		}
		if slices.Contains(extensions, m.T) {
			s.Extras = map[string]any{"x-extension": true}
		}
		defs[m.Type.Name()] = s
		variants = append(variants, ref(m.Type.Name()))
	}
	defs["Message"] = &jsonschema.Schema{
		Description: "One line of a connection: a message of SPEC.md sections 7 to 10, or one of the extensions marked x-extension. " +
			"A receiver ignores lines of a type it does not know, after checking the envelope.",
		OneOf: variants,
	}

	// Parts: one variant per kind, with the fields the kind carries.
	part := reflectType(reflect.TypeFor[wire.Part]())
	var kinds []*jsonschema.Schema
	for _, k := range wire.PartKinds() {
		name := strings.ToUpper(k.K[:1]) + k.K[1:] + "Part"
		v := &jsonschema.Schema{
			Type:        "object",
			Title:       k.K + " part",
			Description: k.Doc,
			Properties:  orderedmap.New[string, *jsonschema.Schema](),
			Required:    append([]string{"k"}, k.Required...),
		}
		kp, _ := part.Properties.Get("k")
		kc := *kp
		kc.Const = k.K
		v.Properties.Set("k", &kc)
		for _, f := range slices.Concat(k.Required, k.Optional) {
			p, ok := part.Properties.Get(f)
			if !ok {
				return nil, fmt.Errorf("part kind %s names unknown field %s", k.K, f)
			}
			v.Properties.Set(f, p)
		}
		defs[name] = v
		kinds = append(kinds, ref(name))
	}
	defs["Part"] = &jsonschema.Schema{Description: part.Description, OneOf: kinds}

	// Objects carried as raw JSON inside messages.
	defs["Grant"] = reflectType(reflect.TypeFor[wire.GrantObject]())
	presence := reflectType(reflect.TypeFor[wire.Presence]())
	presence.Required = append(presence.Required, "sig") // omitempty in Go only for signing
	defs["Presence"] = presence

	if err := point(defs, "Auth", "grants", "Grant", true); err != nil {
		return nil, err
	}
	if err := point(defs, "GrantMsg", "grant", "Grant", false); err != nil {
		return nil, err
	}
	if err := point(defs, "Introduce", "grant", "Grant", false); err != nil {
		return nil, err
	}
	if err := point(defs, "PresenceMsg", "doc", "Presence", false); err != nil {
		return nil, err
	}
	line, ok := defs["Mirror"].Properties.Get("line")
	if !ok {
		return nil, fmt.Errorf("Mirror has no line")
	}
	line.OneOf = []*jsonschema.Schema{ref("Msg"), ref("State")}

	// What the tags cannot express: examples on arrays, an enum from the
	// error code constants, and a pattern with "=" in it.
	if caps, ok := defs["Hello"].Properties.Get("caps"); ok {
		caps.Examples = []any{[]string{"chat", "blob", "grant", "introduce"}}
	}
	if code, ok := defs["Err"].Properties.Get("code"); ok {
		for _, c := range wire.ErrCodes() {
			code.Enum = append(code.Enum, c)
		}
	}
	if data, ok := defs["Chunk"].Properties.Get("data"); ok {
		data.Pattern = wire.B64Pattern
	}
	for _, d := range defs {
		joinLines(d)
	}

	root := &jsonschema.Schema{
		Version:     jsonschema.Version,
		ID:          ID,
		Title:       "Agent Wire Protocol v0",
		Description: "The messages of the Agent Wire Protocol, one JSON object per line (SPEC.md). Every message is an envelope (t, id, ts, and th or re where they apply) with the fields of its type beside it.",
		Comments:    "Generated from the wire package of " + Module + " by go generate ./wire. Do not edit by hand.",
		Ref:         "#/$defs/Message",
		Definitions: defs,
	}
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return &Output{JSON: append(b, '\n'), MDX: []byte(renderMDX(defs))}, nil
}

// Output is what the generator produces: the schema, and its reference
// page for the docs site.
type Output struct {
	JSON []byte
	MDX  []byte
}

// joinLines makes one line of each description, which the comments wrap.
func joinLines(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	s.Description = strings.Join(strings.Fields(s.Description), " ")
	if s.Properties != nil {
		for p := s.Properties.Oldest(); p != nil; p = p.Next() {
			joinLines(p.Value)
		}
	}
	joinLines(s.Items)
	for _, v := range s.OneOf {
		joinLines(v)
	}
}

func ref(name string) *jsonschema.Schema {
	return &jsonschema.Schema{Ref: "#/$defs/" + name}
}

// point replaces the schema of a raw JSON field (or of its items) with a
// reference to the definition of what it carries, keeping the description.
func point(defs jsonschema.Definitions, def, field, target string, items bool) error {
	d, ok := defs[def]
	if !ok {
		return fmt.Errorf("no definition %s", def)
	}
	p, ok := d.Properties.Get(field)
	if !ok {
		return fmt.Errorf("%s has no field %s", def, field)
	}
	if items {
		p.Items = ref(target)
	} else {
		p.Ref = "#/$defs/" + target
		p.Type = ""
	}
	return nil
}

// describeFields rewrites field descriptions that begin with the Go field
// name ("Th is the thread id.") to name the JSON field instead ("`th` is
// the thread id."), walking embedded structs like encoding/json does.
func describeFields(t reflect.Type, s *jsonschema.Schema) {
	if s.Properties == nil {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			describeFields(f.Type, s)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		p, ok := s.Properties.Get(name)
		if !ok {
			continue
		}
		if rest, ok := strings.CutPrefix(p.Description, f.Name+" "); ok {
			p.Description = "`" + name + "` " + rest
		}
	}
}
