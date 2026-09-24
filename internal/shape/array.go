package shape

import "fmt"

// arrayCheck is one chained validation on the array's own length, run once
// every element has parsed — stringCheck's counterpart for a size check
// rather than a per-character one.
type arrayCheck struct {
	code    string
	message string
	test    func(length int) bool
}

// ArraySchema validates a list of elements each matching one schema, built
// with [Array].
type ArraySchema struct {
	mod     modifiers
	element Schema
	checks  []arrayCheck
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

// Max requires at most n items. message overrides zod's builtin wording —
// every caller in this codebase supplies its own, the way StringSchema.Max's
// callers do, so the limit is always explained in terms of whatever it is
// bounding, not just "an array".
func (a *ArraySchema) Max(n int, message ...string) *ArraySchema {
	msg := fmt.Sprintf("Too big: expected array to have <=%d items", n)
	if len(message) > 0 {
		msg = message[0]
	}
	a.checks = append(a.checks, arrayCheck{code: CodeTooBig, message: msg, test: func(length int) bool { return length <= n }})
	return a
}

// parse implements Schema: every element in turn, then a.checks against the
// array's own length, so a too-long array with an also-invalid element still
// reports both issues, not just the first one found.
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
	for _, c := range a.checks {
		if !c.test(len(items)) {
			ctx.issue(c.code, c.message)
		}
	}
	return out, sound
}
