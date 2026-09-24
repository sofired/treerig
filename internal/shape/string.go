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
}

// StringSchema validates a string, built with [String].
type StringSchema struct {
	mod    modifiers
	checks []stringCheck
}

// String starts a string schema.
func String() *StringSchema { return &StringSchema{} }

// Regex requires value to match re, reporting message on failure. berth's own
// schemas always supply their own message — zod's builtin "invalid_string"
// wording is not reproduced here.
func (s *StringSchema) Regex(re *regexp.Regexp, message string) *StringSchema {
	s.checks = append(s.checks, stringCheck{code: CodeInvalidFormat, message: message, test: re.MatchString})
	return s
}

// Min requires at least n UTF-16 code units — JavaScript's String.length,
// which is what zod measures. message overrides zod's builtin wording.
func (s *StringSchema) Min(n int, message ...string) *StringSchema {
	msg := fmt.Sprintf("Too small: expected string to have >=%d characters", n)
	if len(message) > 0 {
		msg = message[0]
	}
	s.checks = append(s.checks, stringCheck{code: CodeTooSmall, message: msg, test: func(v string) bool { return utf16Len(v) >= n }})
	return s
}

// Max requires at most n UTF-16 code units. message overrides zod's builtin wording.
func (s *StringSchema) Max(n int, message ...string) *StringSchema {
	msg := fmt.Sprintf("Too big: expected string to have <=%d characters", n)
	if len(message) > 0 {
		msg = message[0]
	}
	s.checks = append(s.checks, stringCheck{code: CodeTooBig, message: msg, test: func(v string) bool { return utf16Len(v) <= n }})
	return s
}

// Refine adds a custom check: test reports whether value is acceptable, and
// message is reported (as zod's "custom" code) when it is not.
func (s *StringSchema) Refine(test func(string) bool, message string) *StringSchema {
	s.checks = append(s.checks, stringCheck{code: CodeCustom, message: message, test: test})
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
