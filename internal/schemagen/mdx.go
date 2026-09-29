package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/invopop/jsonschema"

	"github.com/agentwireprotocol/awp/wire"
)

// renderMDX writes the reference page of the schema for the docs site:
// the envelope, every message in spec order, the part kinds and the
// objects messages carry, each with a table of its fields.
func renderMDX(defs jsonschema.Definitions) string {
	var b strings.Builder
	b.WriteString(`---
title: Schema reference
description: Every message of the protocol and its fields, generated from the reference implementation.
---

{/* Generated from the wire package of ` + Module + ` by go generate ./wire. Do not edit by hand. */}

Every line of a connection is one JSON object: the [envelope](#envelope) plus the fields of its message type, selected by ` + "`t`" + `. This page is generated from the same source as the [JSON Schema](` + ID + `) (draft 2020-12), the ` + "`wire`" + ` package of the reference implementation, and lists the same constraints. Where it disagrees with [the specification](https://agentwireprotocol.com/spec), the specification wins and the schema has a bug.

Receivers ignore fields they do not know, and lines whose ` + "`t`" + ` they do not know, so no object here is closed. Messages marked *extension* are spoken by the reference implementation and not in the specification.

## Envelope

`)
	writeDescription(&b, defs["Envelope"])
	writeFields(&b, defs["Envelope"])
	b.WriteString("Unknown fields are ignored; unknown types are ignored too, after reading the envelope.\n\n")

	b.WriteString("## Messages\n\nEach message carries the envelope; the tables list the fields beside it.\n\n")
	extensions := wire.Extensions()
	for _, m := range wire.Messages() {
		d := defs[m.Type.Name()]
		fmt.Fprintf(&b, "### %s\n\n", m.T)
		if slices.Contains(extensions, m.T) {
			b.WriteString("*Extension.* ")
		}
		writeDescription(&b, d)
		var need []string
		for _, f := range []string{"th", "re"} {
			if slices.Contains(d.Required, f) {
				need = append(need, "`"+f+"`")
			}
		}
		switch len(need) {
		case 0:
			fmt.Fprintf(&b, "Envelope: `t` is `%q`.\n\n", m.T)
		case 1:
			fmt.Fprintf(&b, "Envelope: `t` is `%q`; %s is required.\n\n", m.T, need[0])
		default:
			fmt.Fprintf(&b, "Envelope: `t` is `%q`; %s are required.\n\n", m.T, strings.Join(need, " and "))
		}
		writeFields(&b, d, envelopeFields...)
	}

	b.WriteString("## Parts\n\n")
	writeDescription(&b, defs["Part"])
	b.WriteString("A part is one of the kinds below, selected by `k`.\n\n")
	for _, k := range wire.PartKinds() {
		name := strings.ToUpper(k.K[:1]) + k.K[1:] + "Part"
		fmt.Fprintf(&b, "### %s part\n\n", k.K)
		writeDescription(&b, defs[name])
		writeFields(&b, defs[name], "k")
	}

	b.WriteString("## Objects\n\nObjects carried inside messages.\n\n")
	for _, name := range []string{"Grant", "IntroPeer", "Presence", "PresencePeer", "PresenceThread"} {
		fmt.Fprintf(&b, "### %s\n\n", objectHeadings[name])
		writeDescription(&b, defs[name])
		writeFields(&b, defs[name])
	}
	return b.String()
}

// envelopeFields are listed once, under Envelope.
var envelopeFields = []string{"t", "id", "ts", "th", "re"}

// objectHeadings head the object sections; they must not collide with the
// message headings (the grant message and the grant object).
var objectHeadings = map[string]string{
	"Grant":          "Grant object",
	"IntroPeer":      "Introduced peer",
	"Presence":       "Presence document",
	"PresencePeer":   "Presence peer",
	"PresenceThread": "Presence thread",
}

func writeDescription(b *strings.Builder, s *jsonschema.Schema) {
	if s.Description != "" {
		b.WriteString(cell(s.Description))
		b.WriteString("\n\n")
	}
}

// writeFields writes the table of an object's properties, leaving out the
// skipped ones (the envelope's, listed once).
func writeFields(b *strings.Builder, s *jsonschema.Schema, skip ...string) {
	var rows []string
	if s.Properties != nil {
		for p := s.Properties.Oldest(); p != nil; p = p.Next() {
			if slices.Contains(skip, p.Key) {
				continue
			}
			req := ""
			if slices.Contains(s.Required, p.Key) {
				req = "yes"
			}
			rows = append(rows, fmt.Sprintf("| `%s` | %s | %s | %s |", p.Key, typeOf(p.Value), req, cell(p.Value.Description)))
		}
	}
	if len(rows) == 0 {
		return
	}
	b.WriteString("| Field | Type | Required | Description |\n|---|---|---|---|\n")
	b.WriteString(strings.Join(rows, "\n"))
	b.WriteString("\n\n")
}

// typeOf renders a property's type and constraints.
func typeOf(s *jsonschema.Schema) string {
	var t string
	switch {
	case s.Ref != "":
		t = link(s.Ref)
	case len(s.OneOf) > 0:
		var alts []string
		for _, a := range s.OneOf {
			alts = append(alts, link(a.Ref))
		}
		t = strings.Join(alts, " or ")
	case s.Type == "array" && s.Items != nil:
		t = "array of " + typeOf(s.Items)
	case s.Type == "object" && s.AdditionalProperties != nil && s.AdditionalProperties.Type != "":
		t = "object of " + s.AdditionalProperties.Type
	case s.Type == "":
		t = "any JSON"
	default:
		t = s.Type
	}
	var cons []string
	if s.Const != nil {
		cons = append(cons, fmt.Sprintf("`%q`", s.Const))
	}
	if len(s.Enum) > 0 {
		var vals []string
		for _, e := range s.Enum {
			vals = append(vals, fmt.Sprintf("`%v`", e))
		}
		cons = append(cons, "one of "+strings.Join(vals, ", "))
	}
	if s.Format != "" {
		cons = append(cons, s.Format)
	}
	if s.Pattern != "" {
		cons = append(cons, "`"+s.Pattern+"`")
	}
	if s.Minimum != "" {
		cons = append(cons, "≥ "+string(s.Minimum))
	}
	if s.MinLength != nil {
		cons = append(cons, "non-empty")
	}
	if len(cons) > 0 {
		t += ", " + strings.Join(cons, ", ")
	}
	return t
}

// link renders a $ref as a link to the definition's section. Messages are
// headed by their t, the rest by their definition name.
func link(ref string) string {
	name := strings.TrimPrefix(ref, "#/$defs/")
	for _, m := range wire.Messages() {
		if m.Type.Name() == name {
			return fmt.Sprintf("[%s](#%s)", m.T, m.T)
		}
	}
	heading := objectHeadings[name]
	if heading == "" {
		heading = name
	}
	return fmt.Sprintf("[%s](#%s)", name, strings.ReplaceAll(strings.ToLower(heading), " ", "-"))
}

// cell escapes text for a table cell and for MDX.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "{", "\\{")
	s = strings.ReplaceAll(s, "}", "\\}")
	s = strings.ReplaceAll(s, "<", "\\<")
	return s
}
