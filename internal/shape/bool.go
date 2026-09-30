package shape

import "fmt"

// BoolSchema validates a boolean, built with [Bool].
type BoolSchema struct {
	mod modifiers
}

// Bool starts a boolean schema.
func Bool() *BoolSchema { return &BoolSchema{} }

// Describe sets the text JSONSchema publishes as this schema's description.
func (b *BoolSchema) Describe(text string) *BoolSchema { b.mod.description = text; return b }

// Optional lets the field be absent, with no default.
func (b *BoolSchema) Optional() *BoolSchema { b.mod.optional = true; return b }

// Default supplies a value used when the field is absent.
func (b *BoolSchema) Default(value bool) *BoolSchema {
	b.mod.hasDefault = true
	b.mod.defaultValue = value
	return b
}

// parse validates raw as a boolean; anything else is a fatal invalid_type
// issue, worded the way zod words it.
func (b *BoolSchema) parse(ctx *context, raw any) (any, bool) {
	if raw == Undefined {
		return parseAbsent(ctx, b.mod, "boolean")
	}
	value, ok := raw.(bool)
	if !ok {
		ctx.issue(CodeInvalidType, fmt.Sprintf("Invalid input: expected boolean, received %s", typeName(raw)))
		return nil, false
	}
	return value, true
}
