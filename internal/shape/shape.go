// Package shape is a small validator that reproduces zod 4's parsing
// behaviour and English messages for the subset treerig's own config, registry
// records, index cards and Compose JSON need: typed objects with defaults,
// `.strict()` unrecognized-key detection, and a superRefine-style hook for
// checks that span more than one field.
//
// A schema is built by composing [String], [Number], [Object] and [Array],
// then handed to [Parse] along with a value — typically a [*Obj] a caller
// built from its own decoder (TOML, JSON, …), since Go's own map type does
// not remember the order its keys were set and zod's output does. [Parse]
// returns the normalized value (defaults applied, unknown keys rejected) and
// every [Issue] found, in the order zod reports them: a value's own checks
// before its object's "Unrecognized key(s)" issue, before a sibling later in
// the schema. A schema's own superRefine only runs once the value it is
// attached to parsed with no such issue anywhere under it, matching zod: a
// wrong type or an unrecognized key is fatal to the value it is found in,
// everything else (a regex, a length limit, a custom refinement) is not.
//
// The package has no dependencies beyond the standard library, so it can be
// reused wherever treerig's Go build wants zod-shaped validation without zod.
package shape

import "strconv"

// Issue mirrors one problem zod would report: where it happened, what kind
// it is, and the message a person reads. Path is empty for the document root.
type Issue struct {
	Path    []string
	Code    string
	Message string
}

// Codes this package produces, matching zod's own issue codes.
const (
	CodeInvalidType      = "invalid_type"
	CodeTooSmall         = "too_small"
	CodeTooBig           = "too_big"
	CodeInvalidFormat    = "invalid_format"
	CodeUnrecognizedKeys = "unrecognized_keys"
	CodeCustom           = "custom"
)

// PathString joins path segments the way a printed issue does: "a.b.c", or
// "(root)" when path is empty. Array indices are already decimal strings, as
// [*context.at] and a superRefine's own path segments both produce.
func PathString(path []string) string {
	if len(path) == 0 {
		return "(root)"
	}
	s := path[0]
	for _, p := range path[1:] {
		s += "." + p
	}
	return s
}

// undefinedSentinel marks an absent value, distinct from JSON null or an
// empty string: only a schema's Optional or Default reacts to it. A caller
// building a [*Obj] simply never Sets a key that was not present; Object's
// own parse substitutes [Undefined] for any declared field it does not find.
type undefinedSentinel struct{}

// Undefined is the value [Object] and [Array] pass to an element schema for
// a key or index that was not present in the source.
var Undefined = undefinedSentinel{}

// Schema is one validator: a value in, the normalized value and whether it
// parsed free of a fatal (structural) issue. It is implemented only by this
// package's own builders — [String], [Number], [Object], [Array] — which
// compose the way zod's schemas do; a consumer builds a document's shape out
// of those rather than implementing Schema itself.
type Schema interface {
	parse(ctx *context, raw any) (value any, sound bool)
}

// modifiers is what a schema does with an absent value: whether that is
// acceptable, and what it becomes. Every schema builder holds one in a field
// named mod, and its parse hands it to parseAbsent.
type modifiers struct {
	optional     bool
	hasDefault   bool
	defaultValue any
	// description is documentation only: JSONSchema publishes it as the
	// schema's "description", and parsing never reads it.
	description string
}

// context carries the path issues are reported against, and the shared slice
// they accumulate into.
type context struct {
	path   []string
	issues *[]Issue
}

// newContext starts a parse at the document root, with an empty issues
// slice every nested context shares.
func newContext() *context {
	return &context{issues: &[]Issue{}}
}

// field returns a context one level deeper, at the named key.
func (c *context) field(key string) *context {
	next := make([]string, len(c.path)+1)
	copy(next, c.path)
	next[len(c.path)] = key
	return &context{path: next, issues: c.issues}
}

// at is field for an array index.
func (c *context) at(index int) *context {
	return c.field(strconv.Itoa(index))
}

// issue records a problem at c's current path, copying the path so a later
// mutation of c.path (as parsing descends further) can't retroactively
// change an already-recorded issue.
func (c *context) issue(code, message string) {
	path := make([]string, len(c.path))
	copy(path, c.path)
	*c.issues = append(*c.issues, Issue{Path: path, Code: code, Message: message})
}

// Parse validates raw against schema and returns the normalized value
// alongside every issue found. Success is len(issues) == 0 — a value can
// come back non-nil with issues still present, when a later field is what
// failed; a caller should not use the value unless issues is empty, the same
// way zod's own safeParse().success gates using .data.
func Parse(schema Schema, raw any) (value any, issues []Issue) {
	ctx := newContext()
	value, _ = schema.parse(ctx, raw)
	return value, *ctx.issues
}

// typeName is what zod calls raw's type in an "expected X, received Y"
// message.
func typeName(raw any) string {
	switch v := raw.(type) {
	case undefinedSentinel:
		return "undefined"
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case int, int64:
		return "number"
	case float64:
		if isNaN(v) {
			return "NaN"
		}
		return "number"
	case []any:
		return "array"
	case *Obj:
		return "object"
	default:
		return "unknown"
	}
}

// isNaN reports whether f is NaN, using the fact that NaN is the only float
// value that does not equal itself.
func isNaN(f float64) bool { return f != f }

// parseAbsent is what every schema does when handed [Undefined]: apply a
// default, allow it through as still-absent when Optional, or report the
// same "expected X, received undefined" invalid_type issue zod does for a
// required field that is missing.
func parseAbsent(ctx *context, m modifiers, expected string) (any, bool) {
	if m.hasDefault {
		return m.defaultValue, true
	}
	if m.optional {
		return Undefined, true
	}
	ctx.issue(CodeInvalidType, "Invalid input: expected "+expected+", received undefined")
	return nil, false
}

// formatNum prints a float the way a builtin zod message does: whole numbers
// with no trailing ".0". Every limit this package's callers pass in is
// whole, so this never needs to print a fraction in practice.
func formatNum(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
