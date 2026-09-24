package shape

import "fmt"

// ArraySchema validates a list of elements each matching one schema, built
// with [Array].
type ArraySchema struct {
	mod     modifiers
	element Schema
}

// Array declares a schema whose elements each match element.
func Array(element Schema) *ArraySchema { return &ArraySchema{element: element} }

// Optional lets the field be absent, with no default.
func (a *ArraySchema) Optional() *ArraySchema { a.mod.optional = true; return a }

// Default supplies a value used when the field is absent.
func (a *ArraySchema) Default(value []any) *ArraySchema {
	a.mod.hasDefault = true
	a.mod.defaultValue = value
	return a
}

func (a *ArraySchema) parse(ctx *context, raw any) (any, bool) {
	if raw == Undefined {
		return parseAbsent(ctx, a.mod, "array")
	}
	items, ok := raw.([]any)
	if !ok {
		ctx.issue(CodeInvalidType, fmt.Sprintf("Invalid input: expected array, received %s", typeName(raw)))
		return nil, false
	}
	out := make([]any, 0, len(items))
	sound := true
	for i, item := range items {
		value, itemSound := a.element.parse(ctx.at(i), item)
		if !itemSound {
			sound = false
		}
		out = append(out, value)
	}
	return out, sound
}
