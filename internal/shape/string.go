package shape

import (
	"fmt"
	"regexp"
)

// stringCheck is one chained validation on an already-string value. Every
// check in a schema runs regardless of whether an earlier one failed — zod
// collects all of a field's own issues rather than stopping at the first —
// so a value can carry more than one Issue for a single field.
type stringCheck struct {
	code    string
	message string
	test    func(string) bool
	// doc, when set, adds this check's JSON Schema keywords to a node.
	doc func(node *Obj)
	// custom marks a [StringSchema.Refine]: code, which JSON Schema cannot state.
	custom bool
}

// StringSchema validates a string, built with [String].
type StringSchema struct {
	mod    modifiers
	checks []stringCheck
	// schemaPattern is set by SchemaPattern: the published schema states,
	// as a pattern, the rule a Refine enforces.
	schemaPattern bool
}

// String starts a string schema.
func String() *StringSchema { return &StringSchema{} }

// Regex requires value to match re, reporting message on failure. berth's own
// schemas always supply their own message — zod's builtin "invalid_string"
// wording is not reproduced here.
//
// re is also published as the JSON Schema "pattern", which editors run as an
// ECMA-262 regular expression, so it must mean the same in both dialects:
// write only syntax they share. [JSONSchema] panics on the RE2-only
// constructs it recognizes: flag groups such as (?i) and named groups, \A,
// \z and POSIX classes like [[:alpha:]].
func (s *StringSchema) Regex(re *regexp.Regexp, message string) *StringSchema {
	s.checks = append(s.checks, stringCheck{code: CodeInvalidFormat, message: message, test: re.MatchString,
		doc: func(node *Obj) { setPattern(node, re.String()) }})
	return s
}

// SchemaPattern documents a pattern in the published JSON Schema without
// checking it: for a rule that a [StringSchema.Refine] already enforces (a
// duration's spelling, say) and that JSON Schema can state as a pattern too.
// A [StringSchema.Refine] without one is code-only: see [CodeOnlyChecks].
// As for [StringSchema.Regex], re must be ECMA-262 compatible.
func (s *StringSchema) SchemaPattern(re *regexp.Regexp) *StringSchema {
	s.schemaPattern = true
	s.checks = append(s.checks, stringCheck{test: func(string) bool { return true },
		doc: func(node *Obj) { setPattern(node, re.String()) }})
	return s
}

// Describe sets the text JSONSchema publishes as this schema's description.
func (s *StringSchema) Describe(text string) *StringSchema { s.mod.description = text; return s }

// Min requires at least n UTF-16 code units — JavaScript's String.length,
// which is what zod measures. message overrides zod's builtin wording.
// The published minLength counts code points, so for n above 1 the schema
// would be stricter than this check on characters outside the Basic
// Multilingual Plane; every Min in the config is 1, where the two agree.
func (s *StringSchema) Min(n int, message ...string) *StringSchema {
	msg := fmt.Sprintf("Too small: expected string to have >=%d characters", n)
	if len(message) > 0 {
		msg = message[0]
	}
	s.checks = append(s.checks, stringCheck{code: CodeTooSmall, message: msg, test: func(v string) bool { return utf16Len(v) >= n },
		doc: func(node *Obj) { node.Set("minLength", n) }})
	return s
}

// Max requires at most n UTF-16 code units. message overrides zod's builtin wording.
// The published maxLength counts code points, so a string with characters
// outside the Basic Multilingual Plane can pass the schema and still fail
// here; docs/design/config.md records that difference.
func (s *StringSchema) Max(n int, message ...string) *StringSchema {
	msg := fmt.Sprintf("Too big: expected string to have <=%d characters", n)
	if len(message) > 0 {
		msg = message[0]
	}
	s.checks = append(s.checks, stringCheck{code: CodeTooBig, message: msg, test: func(v string) bool { return utf16Len(v) <= n },
		doc: func(node *Obj) { node.Set("maxLength", n) }})
	return s
}

// Refine adds a custom check: test reports whether value is acceptable, and
// message is reported (as zod's "custom" code) when it is not.
func (s *StringSchema) Refine(test func(string) bool, message string) *StringSchema {
	s.checks = append(s.checks, stringCheck{code: CodeCustom, message: message, test: test, custom: true})
	return s
}

// Optional lets the field be absent, with no default.
func (s *StringSchema) Optional() *StringSchema { s.mod.optional = true; return s }

// Default supplies a value used when the field is absent.
func (s *StringSchema) Default(value string) *StringSchema {
	s.mod.hasDefault = true
	s.mod.defaultValue = value
	return s
}

// parse validates raw as a string: a wrong type is fatal, but every failed
// check after that is reported and parsing still succeeds — matching zod,
// where a regex, length or refine failure is not fatal to the value.
func (s *StringSchema) parse(ctx *context, raw any) (any, bool) {
	if raw == Undefined {
		return parseAbsent(ctx, s.mod, "string")
	}
	value, ok := raw.(string)
	if !ok {
		ctx.issue(CodeInvalidType, fmt.Sprintf("Invalid input: expected string, received %s", typeName(raw)))
		return nil, false
	}
	for _, c := range s.checks {
		if !c.test(value) {
			ctx.issue(c.code, c.message)
		}
	}
	return value, true
}

// utf16Len is JavaScript's String.prototype.length: UTF-16 code units, not
// Unicode code points. An astral character (outside the Basic Multilingual
// Plane) counts as two. Kept local rather than importing internal/text, so
// this package stays dependency-free for whoever reuses it next.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xffff {
			n += 2
		} else {
			n++
		}
	}
	return n
}
