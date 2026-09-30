package shape

import "fmt"

// UnionSchema validates a value against alternatives tried in order, built
// with [Union], accepting the first whose own type matches raw — not
// necessarily the first that parses issue-free.
//
// Two failure modes. When raw's type matches none of the alternatives (say,
// a number where the alternatives are [String] and an [Object]), one
// invalid_type issue is reported at the union's own path naming every
// alternative's expected type, following this package's own "Invalid input:
// expected X, received Y" wording: "Invalid input: expected string or
// object, received number". When raw's type DOES match one alternative —
// it's a string and one alternative is [String], or an object and one is
// [Object] or [Record] — but that alternative's own validation still fails
// (a bad field, a failed regex, an unrecognized key, …), this reports THAT
// alternative's own issues, at their own paths, instead of the generic
// "expected X or Y": a wrong field inside a matching branch tells a caller
// more than being told the value could have been something else entirely.
// Only the first type-matching alternative is tried; a value whose type
// could satisfy more than one alternative (two [Object] schemas, say) is
// validated against whichever was listed first.
//
// An absent value is decided by the union's own [UnionSchema.Optional] or
// [UnionSchema.Default] alone — [UnionSchema.parse] never reaches an
// alternative's own parse for [Undefined], so an alternative's own Optional
// or Default is never consulted.
type UnionSchema struct {
	mod     modifiers
	schemas []Schema
}

// Union declares a schema accepting any one of schemas, tried in the order
// given. Panics if schemas is empty: a union with nothing to accept is a
// construction mistake to catch immediately, not something to discover
// mid-parse.
func Union(schemas ...Schema) *UnionSchema {
	if len(schemas) == 0 {
		panic("shape: Union requires at least one schema")
	}
	return &UnionSchema{schemas: schemas}
}

// Describe sets the text JSONSchema publishes as this schema's description.
func (u *UnionSchema) Describe(text string) *UnionSchema { u.mod.description = text; return u }

// Optional lets the field be absent, with no default.
func (u *UnionSchema) Optional() *UnionSchema { u.mod.optional = true; return u }

// Default supplies a value used when the field is absent.
func (u *UnionSchema) Default(value any) *UnionSchema {
	u.mod.hasDefault = true
	u.mod.defaultValue = value
	return u
}

// parse tries each alternative in turn against a scratch context that
// shares ctx's path but collects into its own issues slice, so a rejected
// alternative's issues never leak into the result. The first alternative
// whose own top-level type check passed — see [typeMatched] — has its
// issues (zero or more) copied into ctx verbatim and is used as the result;
// no further alternative is tried, even if this one reports issues of its
// own. Only when every alternative's own type check failed is the combined
// "expected A or B" issue reported instead.
func (u *UnionSchema) parse(ctx *context, raw any) (any, bool) {
	if raw == Undefined {
		return parseAbsent(ctx, u.mod, joinOr(expectedNames(u.schemas)))
	}
	for _, s := range u.schemas {
		scratch := &context{path: ctx.path, issues: &[]Issue{}}
		value, sound := s.parse(scratch, raw)
		if typeMatched(*scratch.issues, ctx.path) {
			*ctx.issues = append(*ctx.issues, *scratch.issues...)
			return value, sound
		}
	}
	ctx.issue(CodeInvalidType, fmt.Sprintf("Invalid input: expected %s, received %s", joinOr(expectedNames(u.schemas)), typeName(raw)))
	return nil, false
}

// typeMatched reports whether issues — gathered from one alternative's own
// parse at a scratch context sharing path as its top-level position — holds
// that alternative's own top-level invalid_type issue, the signal that raw
// was never even the right JS-ish type for it. Any other issue (a nested
// field, an unrecognized key, a failed check) still counts as a type match:
// the alternative accepted raw's shape and found a problem within it, which
// [UnionSchema.parse] then reports instead of the generic "expected" issue.
func typeMatched(issues []Issue, path []string) bool {
	for _, issue := range issues {
		if issue.Code == CodeInvalidType && pathEqual(issue.Path, path) {
			return false
		}
	}
	return true
}

// pathEqual reports whether a and b name the same path, element by element.
func pathEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// expectedName is the JS-style type word [UnionSchema] reports for one of
// its alternatives, drawn from the same vocabulary [typeName] uses for
// every other invalid_type message in this package, so a union's combined
// message reads the way a single schema's own would.
func expectedName(s Schema) string {
	switch v := s.(type) {
	case *StringSchema:
		return "string"
	case *BoolSchema:
		return "boolean"
	case *NumberSchema:
		if v.intOnly {
			return "int"
		}
		return "number"
	case *ObjectSchema, *RecordSchema:
		return "object"
	case *ArraySchema:
		return "array"
	default:
		return "value"
	}
}

// expectedNames maps [expectedName] over schemas, for [joinOr], splicing a
// nested [UnionSchema]'s own names in place so they read as one flat list.
func expectedNames(schemas []Schema) []string {
	var names []string
	for _, s := range schemas {
		if nested, ok := s.(*UnionSchema); ok {
			names = append(names, expectedNames(nested.schemas)...)
			continue
		}
		names = append(names, expectedName(s))
	}
	return names
}

// joinOr joins names the way this package's messages list alternatives:
// "a" for one, "a or b" for two, "a, b or c" for more.
func joinOr(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " or " + names[1]
	default:
		s := ""
		for _, n := range names[:len(names)-1] {
			if s != "" {
				s += ", "
			}
			s += n
		}
		return s + " or " + names[len(names)-1]
	}
}
