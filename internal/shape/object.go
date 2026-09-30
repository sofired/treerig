package shape

import "fmt"

// Field is one declared key of an [Object] schema, in the order it should be
// parsed and, on success, appear in the result's [Obj.Keys].
type Field struct {
	Key    string
	Schema Schema
}

// F is shorthand for a [Field] literal.
func F(key string, schema Schema) Field { return Field{Key: key, Schema: schema} }

// Refiner is a cross-field check attached with [ObjectSchema.SuperRefine]. It
// receives the object's own parsed value and a function to report a problem
// at a path relative to this object — [ObjectSchema.parse] prepends the
// object's own position before recording it. It only runs once every field
// (recursively) parsed with no fatal issue, matching zod: a superRefine does
// not see a value that failed to take its declared shape.
type Refiner func(value *Obj, addIssue func(path []string, message string))

// ObjectSchema validates an object, built with [Object].
type ObjectSchema struct {
	mod      modifiers
	fields   []Field
	strict   bool
	refiners []Refiner
	keyHints map[string]func(path []string) string
}

// Object declares a schema with these fields, in this order. Add [ObjectSchema.Strict]
// to reject keys not listed here.
func Object(fields ...Field) *ObjectSchema {
	return &ObjectSchema{fields: fields}
}

// Strict rejects any key not among the declared fields, reporting one
// unrecognized_keys issue naming all of them (zod's `.strict()`).
func (o *ObjectSchema) Strict() *ObjectSchema { o.strict = true; return o }

// KeyHint adds a hint, in parentheses, to the unrecognized_keys issue when
// key is among the rejected keys: for a key that a likely mistake puts here
// and that has a better home. hint receives the path of this object, so it
// can name the table the key was found in.
func (o *ObjectSchema) KeyHint(key string, hint func(path []string) string) *ObjectSchema {
	if o.keyHints == nil {
		o.keyHints = map[string]func(path []string) string{}
	}
	o.keyHints[key] = hint
	return o
}

// SuperRefine adds a cross-field check. More than one may be added; they run
// in the order added.
func (o *ObjectSchema) SuperRefine(fn Refiner) *ObjectSchema {
	o.refiners = append(o.refiners, fn)
	return o
}

// Describe sets the text JSONSchema publishes as this schema's description.
func (o *ObjectSchema) Describe(text string) *ObjectSchema { o.mod.description = text; return o }

// Optional lets the field be absent, with no default.
func (o *ObjectSchema) Optional() *ObjectSchema { o.mod.optional = true; return o }

// Default supplies a value used when the field is absent.
func (o *ObjectSchema) Default(value *Obj) *ObjectSchema {
	o.mod.hasDefault = true
	o.mod.defaultValue = value
	return o
}

// parse validates raw as an object: each declared field against its own
// schema (an absent one seen as [Undefined]), an unrecognized_keys issue for
// any extra key when Strict, then — only if nothing above found a fatal
// issue — this schema's superRefine checks in the order they were added.
func (o *ObjectSchema) parse(ctx *context, raw any) (any, bool) {
	if raw == Undefined {
		return parseAbsent(ctx, o.mod, "object")
	}
	input, ok := raw.(*Obj)
	if !ok {
		ctx.issue(CodeInvalidType, fmt.Sprintf("Invalid input: expected object, received %s", typeName(raw)))
		return nil, false
	}

	out := NewObj()
	sound := true
	known := make(map[string]bool, len(o.fields))
	for _, f := range o.fields {
		known[f.Key] = true
		fieldRaw, present := input.Get(f.Key)
		if !present {
			fieldRaw = Undefined
		}
		value, fieldSound := f.Schema.parse(ctx.field(f.Key), fieldRaw)
		if !fieldSound {
			sound = false
		}
		if value != Undefined {
			out.Set(f.Key, value)
		}
	}

	if o.strict {
		var extra []string
		for _, k := range input.Keys() {
			if !known[k] {
				extra = append(extra, k)
			}
		}
		if len(extra) > 0 {
			sound = false
			message := unrecognizedKeysMessage(extra)
			for _, k := range extra {
				if hint, ok := o.keyHints[k]; ok {
					message += " (" + hint(ctx.path) + ")"
				}
			}
			ctx.issue(CodeUnrecognizedKeys, message)
		}
	}

	if sound {
		for _, refine := range o.refiners {
			refine(out, func(path []string, message string) {
				full := make([]string, 0, len(ctx.path)+len(path))
				full = append(full, ctx.path...)
				full = append(full, path...)
				*ctx.issues = append(*ctx.issues, Issue{Path: full, Code: CodeCustom, Message: message})
			})
		}
	}

	return out, sound
}

// unrecognizedKeysMessage formats a Strict object's rejected keys the way
// zod does: "Unrecognized key: "x"" for one, "Unrecognized keys: "x", "y""
// for more.
func unrecognizedKeysMessage(extra []string) string {
	if len(extra) == 1 {
		return fmt.Sprintf("Unrecognized key: %q", extra[0])
	}
	msg := "Unrecognized keys: "
	for i, k := range extra {
		if i > 0 {
			msg += ", "
		}
		msg += fmt.Sprintf("%q", k)
	}
	return msg
}
