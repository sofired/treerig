package shape

import "fmt"

// RecordSchema validates an object with arbitrary keys, each checked by key
// and, once accepted, validated by value — built with [Record]. Unlike
// [Object], there is no fixed field list and no Strict/unrecognized-keys
// issue: every key present is accepted, subject only to key's own checks.
// This is what a table of arbitrary, caller-named entries needs — a future
// `[commands]` (#94), a registry of named hooks, anything TOML or JSON
// would decode as an object whose keys are data, not schema.
type RecordSchema struct {
	mod   modifiers
	key   *StringSchema
	value Schema
}

// Record declares a schema for an object whose keys must each satisfy key
// — typically a bare [String] with a [StringSchema.Regex] check, so a
// rejected key reports that check's own code and the caller's own message,
// the same as any other field would. Every key that key accepts has its value
// then validated by value. Panics if key or value is nil: both are
// construction mistakes to catch immediately, not something to discover
// mid-parse.
//
// A key that key rejects has its value never parsed at all — this
// package's own rule, documented further on [RecordSchema.parse].
//
// The parsed result's [*Obj] keeps the source object's own key order — an
// [*Obj] remembers the order in which its keys were [Obj.Set] — not
// sorted, and not a declared order, since Record declares none.
//
// Choose key with care: a pattern that allows "." makes a reported issue's
// [PathString] ambiguous with a nested field's own path.
func Record(key *StringSchema, value Schema) *RecordSchema {
	if key == nil {
		panic("shape: Record key must not be nil")
	}
	if value == nil {
		panic("shape: Record value must not be nil")
	}
	return &RecordSchema{key: key, value: value}
}

// Optional lets the field be absent, with no default.
func (r *RecordSchema) Optional() *RecordSchema { r.mod.optional = true; return r }

// Default supplies a value used when the field is absent.
func (r *RecordSchema) Default(value *Obj) *RecordSchema {
	r.mod.hasDefault = true
	r.mod.defaultValue = value
	return r
}

// parse validates raw as an object: every key present is parsed by r.key at
// its own path first. A key that reports any issue there is rejected and
// its value is never parsed — a deliberate rule of this package, kept
// because a caller validating a key's meaning (a command name, say) rarely
// wants a second, likely-redundant issue about a value it never got to
// check. A key r.key accepts has its value parsed by r.value at that same
// path; a value present in the input but parsed to [Undefined] is left out
// of the result, mirroring [ObjectSchema]'s own rule for an absent optional
// field. Every key is checked this way regardless of an earlier one
// failing, the "every check runs" contract [ArraySchema] and [StringSchema]
// already promise for their own checks.
func (r *RecordSchema) parse(ctx *context, raw any) (any, bool) {
	if raw == Undefined {
		return parseAbsent(ctx, r.mod, "object")
	}
	input, ok := raw.(*Obj)
	if !ok {
		ctx.issue(CodeInvalidType, fmt.Sprintf("Invalid input: expected object, received %s", typeName(raw)))
		return nil, false
	}

	out := NewObj()
	sound := true
	for _, key := range input.Keys() {
		fieldCtx := ctx.field(key)
		before := len(*ctx.issues)
		r.key.parse(fieldCtx, key)
		if len(*ctx.issues) > before {
			sound = false
			continue
		}
		fieldRaw, _ := input.Get(key)
		value, fieldSound := r.value.parse(fieldCtx, fieldRaw)
		if !fieldSound {
			sound = false
		}
		if value != Undefined {
			out.Set(key, value)
		}
	}
	return out, sound
}
