package shape

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// JSONSchemaDraft07 is the $schema URI of the dialect [JSONSchema] writes.
const JSONSchemaDraft07 = "http://json-schema.org/draft-07/schema#"

// JSONSchema renders root as a draft-07 JSON Schema document describing the
// input shape it accepts: the keys and types a document may hold, not the
// normalized value [Parse] returns from it. It is generated from the same
// builders validation runs, so the two cannot drift apart.
//
// A field with a default or marked Optional is not "required". Checks that
// are code rather than data (a [StringSchema.Refine], a [Refiner]) cannot be
// stated and are left out; [StringSchema.SchemaPattern] documents one that
// can. Lengths are UTF-16 code units in validation and code points in JSON
// Schema, which only differ for characters outside the Basic Multilingual
// Plane. [CodeOnlyChecks] lists what is left out. id is the $id: where the
// published copy lives.
//
// The output is stable: keys are written in a fixed order, indented by two
// spaces, and end with a newline.
func JSONSchema(root Schema, id, title string) []byte {
	doc := NewObj()
	doc.Set("$schema", JSONSchemaDraft07)
	doc.Set("$id", id)
	doc.Set("title", title)
	node := jsonNode(root)
	for _, key := range node.Keys() {
		v, _ := node.Get(key)
		doc.Set(key, v)
	}
	var b bytes.Buffer
	writeJSON(&b, doc, 0)
	b.WriteByte('\n')
	return b.Bytes()
}

// modOf is s's modifiers.
func modOf(s Schema) modifiers {
	switch v := s.(type) {
	case *StringSchema:
		return v.mod
	case *NumberSchema:
		return v.mod
	case *BoolSchema:
		return v.mod
	case *ObjectSchema:
		return v.mod
	case *ArraySchema:
		return v.mod
	case *RecordSchema:
		return v.mod
	case *UnionSchema:
		return v.mod
	default:
		panic(fmt.Sprintf("shape: JSONSchema does not know %T", s))
	}
}

// re2Only are the spellings of RE2 (Go's regexp) that ECMA-262 (what editors
// run a "pattern" with) reads differently or rejects. The guard in setPattern
// is a substring check: conservative, so it may reject a legitimate escaped
// "(?" such as \(?, and it does not catch every difference, \pL and \Q...\E
// among them. It catches the likely slips; the pattern is still on its author.
var re2Only = []string{`\A`, `\z`, `[[:`}

// setPattern adds a pattern to node; a second one goes under allOf, since
// JSON Schema allows a node only one. It panics on a pattern written in
// RE2-only syntax (see [StringSchema.Regex]): that is a mistake in the
// schema definition, to catch when the schema is generated.
func setPattern(node *Obj, pattern string) {
	if rest := strings.ReplaceAll(pattern, "(?:", ""); strings.Contains(rest, "(?") {
		panic(fmt.Sprintf("shape: pattern %q uses RE2-only syntax; only (?: groups are shared with ECMA-262", pattern))
	}
	for _, spelling := range re2Only {
		if strings.Contains(pattern, spelling) {
			panic(fmt.Sprintf("shape: pattern %q uses %s, which ECMA-262 does not have", pattern, spelling))
		}
	}
	if _, taken := node.Get("pattern"); !taken {
		node.Set("pattern", pattern)
		return
	}
	extra := NewObj().Set("pattern", pattern)
	all, _ := node.Get("allOf")
	list, _ := all.([]any)
	node.Set("allOf", append(list, extra))
}

// jsonNode is the JSON Schema of one schema, its description and default
// included.
func jsonNode(s Schema) *Obj {
	node := NewObj()
	m := modOf(s)
	switch v := s.(type) {
	case *StringSchema:
		node.Set("type", "string")
		describe(node, m)
		for _, c := range v.checks {
			if c.doc != nil {
				c.doc(node)
			}
		}
	case *NumberSchema:
		if v.intOnly {
			node.Set("type", "integer")
		} else {
			node.Set("type", "number")
		}
		describe(node, m)
		for _, c := range v.checks {
			if c.doc != nil {
				c.doc(node)
			}
		}
	case *BoolSchema:
		node.Set("type", "boolean")
		describe(node, m)
	case *ObjectSchema:
		node.Set("type", "object")
		describe(node, m)
		properties := NewObj()
		var required []any
		for _, f := range v.fields {
			properties.Set(f.Key, jsonNode(f.Schema))
			fm := modOf(f.Schema)
			if !fm.optional && !fm.hasDefault {
				required = append(required, f.Key)
			}
		}
		node.Set("properties", properties)
		if len(required) > 0 {
			node.Set("required", required)
		}
		if v.strict {
			node.Set("additionalProperties", false)
		}
	case *ArraySchema:
		node.Set("type", "array")
		describe(node, m)
		node.Set("items", jsonNode(v.element))
		for _, c := range v.checks {
			if c.doc != nil {
				c.doc(node)
			}
		}
	case *RecordSchema:
		node.Set("type", "object")
		describe(node, m)
		names := NewObj()
		names.Set("type", "string")
		describe(names, v.key.mod)
		for _, c := range v.key.checks {
			if c.doc != nil {
				c.doc(names)
			}
		}
		node.Set("propertyNames", names)
		node.Set("additionalProperties", jsonNode(v.value))
		if v.hasMax {
			node.Set("maxProperties", v.maxEntries)
		}
	case *UnionSchema:
		describe(node, m)
		alternatives := make([]any, len(v.schemas))
		for i, alt := range v.schemas {
			alternatives[i] = jsonNode(alt)
		}
		node.Set("anyOf", alternatives)
	}
	if m.hasDefault {
		node.Set("default", m.defaultValue)
	}
	return node
}

// describe adds m's description, when it has one.
func describe(node *Obj, m modifiers) {
	if m.description != "" {
		node.Set("description", m.description)
	}
}

// writeJSON writes v indented by two spaces per level, keeping an [*Obj]'s
// key order. It does not escape HTML, so a description can say <name>.
func writeJSON(b *bytes.Buffer, v any, depth int) {
	pad := strings.Repeat("  ", depth+1)
	end := strings.Repeat("  ", depth)
	switch v := v.(type) {
	case *Obj:
		if v.Len() == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, key := range v.Keys() {
			if i > 0 {
				b.WriteString(",\n")
			}
			value, _ := v.Get(key)
			b.WriteString(pad)
			writeJSONString(b, key)
			b.WriteString(": ")
			writeJSON(b, value, depth+1)
		}
		b.WriteString("\n" + end + "}")
	case []any:
		if len(v) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, item := range v {
			if i > 0 {
				b.WriteString(",\n")
			}
			b.WriteString(pad)
			writeJSON(b, item, depth+1)
		}
		b.WriteString("\n" + end + "]")
	case string:
		writeJSONString(b, v)
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case int:
		b.WriteString(strconv.Itoa(v))
	case float64:
		b.WriteString(formatNum(v))
	case nil:
		b.WriteString("null")
	default:
		panic(fmt.Sprintf("shape: JSONSchema cannot write %T", v))
	}
}

// writeJSONString writes s as a JSON string, escaping only what JSON
// requires.
func writeJSONString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// CodeOnlyChecks lists where root validates something [JSONSchema] cannot
// state, one path per place: a [StringSchema.Refine] that no
// [StringSchema.SchemaPattern] restates, and an [ObjectSchema.SuperRefine].
// A path is dotted keys from the root, "*" for a record's value or an array's
// element, "|N" for the Nth alternative of a union, and "(root)" for the
// root itself. A test pins the list, so adding such a check is a visible
// choice; the published schema is looser than validation exactly there.
func CodeOnlyChecks(root Schema) []string {
	var found []string
	var walk func(path string, s Schema)
	walk = func(path string, s Schema) {
		switch v := s.(type) {
		case *StringSchema:
			if v.schemaPattern {
				return
			}
			for _, c := range v.checks {
				if c.custom {
					found = append(found, path)
					return
				}
			}
		case *ObjectSchema:
			if len(v.refiners) > 0 {
				found = append(found, path)
			}
			for _, f := range v.fields {
				walk(join(path, f.Key), f.Schema)
			}
		case *ArraySchema:
			walk(join(path, "*"), v.element)
		case *RecordSchema:
			walk(join(path, "<key>"), v.key)
			walk(join(path, "*"), v.value)
		case *UnionSchema:
			for i, alt := range v.schemas {
				walk(fmt.Sprintf("%s|%d", path, i), alt)
			}
		}
	}
	walk("(root)", root)
	return found
}

// join extends a [CodeOnlyChecks] path.
func join(path, key string) string {
	if path == "(root)" {
		return key
	}
	return path + "." + key
}
