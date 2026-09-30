package shape

import (
	"fmt"
	"math"
)

type numberCheck struct {
	code    string
	message string
	test    func(float64) bool
	// doc, when set, adds this check's JSON Schema keywords to a node.
	doc func(node *Obj)
}

// NumberSchema validates a number, built with [Number].
type NumberSchema struct {
	mod     modifiers
	intOnly bool
	checks  []numberCheck
}

// Number starts a number schema.
func Number() *NumberSchema { return &NumberSchema{} }

// Int requires an integer: no fractional part. Like zod, this is part of the
// type check itself (a non-integer is reported as invalid_type, "expected
// int", not as a separate too-small/too-big style check), and accepts a
// float that happens to be whole — 80.0 passes — the same way JavaScript's
// Number.isInteger does, because JavaScript has no separate integer type.
func (n *NumberSchema) Int() *NumberSchema { n.intOnly = true; return n }

// Min requires value >= v. message overrides zod's builtin wording.
func (n *NumberSchema) Min(v float64, message ...string) *NumberSchema {
	msg := fmt.Sprintf("Too small: expected number to be >=%s", formatNum(v))
	if len(message) > 0 {
		msg = message[0]
	}
	n.checks = append(n.checks, numberCheck{code: CodeTooSmall, message: msg, test: func(x float64) bool { return x >= v },
		doc: func(node *Obj) { node.Set("minimum", v) }})
	return n
}

// Max requires value <= v. message overrides zod's builtin wording.
func (n *NumberSchema) Max(v float64, message ...string) *NumberSchema {
	msg := fmt.Sprintf("Too big: expected number to be <=%s", formatNum(v))
	if len(message) > 0 {
		msg = message[0]
	}
	n.checks = append(n.checks, numberCheck{code: CodeTooBig, message: msg, test: func(x float64) bool { return x <= v },
		doc: func(node *Obj) { node.Set("maximum", v) }})
	return n
}

// Describe sets the text JSONSchema publishes as this schema's description.
func (n *NumberSchema) Describe(text string) *NumberSchema { n.mod.description = text; return n }

// Optional lets the field be absent, with no default.
func (n *NumberSchema) Optional() *NumberSchema { n.mod.optional = true; return n }

// Default supplies a value used when the field is absent.
func (n *NumberSchema) Default(value float64) *NumberSchema {
	n.mod.hasDefault = true
	n.mod.defaultValue = value
	return n
}

// parse's "expected" name is "number" for every type mismatch — absent,
// wrong JS type, even NaN — matching zod: .int() only changes the message to
// "expected int" for a value that IS a number but has a fractional part.
// Verified against zod 4.4.3: z.number().int().safeParse("80") reports
// "expected number, received string", not "expected int".
func (n *NumberSchema) parse(ctx *context, raw any) (any, bool) {
	if raw == Undefined {
		return parseAbsent(ctx, n.mod, "number")
	}
	value, ok := numericValue(raw)
	if !ok {
		ctx.issue(CodeInvalidType, fmt.Sprintf("Invalid input: expected number, received %s", typeName(raw)))
		return nil, false
	}
	// zod 4 takes no infinity as a number at all, and says so in these words.
	if math.IsInf(value, 0) {
		ctx.issue(CodeInvalidType, "Invalid input: expected number, received number")
		return nil, false
	}
	safe := true
	if n.intOnly {
		// Number.isInteger: whole, however large — int64 conversion would misjudge 1e20.
		if math.Trunc(value) != value {
			ctx.issue(CodeInvalidType, "Invalid input: expected int, received number")
			return nil, false
		}
		// zod's int is a safe integer: past 2^53-1 either way is its own issue,
		// reported before, and alongside, the schema's own range.
		switch {
		case value > maxSafeInteger:
			ctx.issue(CodeTooBig, "Too big: expected int to be <=9007199254740991")
			safe = false
		case value < -maxSafeInteger:
			ctx.issue(CodeTooSmall, "Too small: expected int to be >=-9007199254740991")
			safe = false
		}
	}
	for _, c := range n.checks {
		if !c.test(value) {
			ctx.issue(c.code, c.message)
		}
	}
	if n.intOnly {
		if !safe {
			return nil, false
		}
		return int(value), true
	}
	return value, true
}

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, 2^53-1.
const maxSafeInteger = 1<<53 - 1

// numericValue is what raw is worth as a JavaScript number: int and int64
// from a caller building values directly, float64 from decoded TOML/JSON —
// except NaN, which zod's own number check rejects by name (see typeName).
func numericValue(raw any) (float64, bool) {
	switch v := raw.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		if isNaN(v) {
			return 0, false
		}
		return v, true
	default:
		return 0, false
	}
}
