package shape

import (
	"regexp"
	"testing"
)

// These pin down [Record] and [Union] (#103), added for a future `[commands]`
// table (#94) whose keys are arbitrary command names and whose values are
// either a bare string or a table — something [Object]'s fixed field list and
// every other schema's single accepted type can't express on their own.

// commandNamePattern is a stand-in for whatever #94's own [commands] key
// pattern turns out to be: lower-case letters, digits and hyphens, the same
// shape config.go's own slugPattern already uses for a Compose service name.
var commandNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// commandKeySchema is the key schema built from commandNamePattern, with the
// message a caller would supply.
func commandKeySchema() *StringSchema {
	return String().Regex(commandNamePattern, "must be lower-case letters, digits, and hyphens")
}

func TestRecordValidatesEachValueAndKeepsInputKeyOrder(t *testing.T) {
	schema := Record(commandKeySchema(), String())

	// Built out of order, the way an unordered map's iteration would
	// present it — Record has no schema-declared field order to fall back
	// on, so it must follow the input's own order instead of [Object]'s.
	input := NewObj().Set("zeta", "z").Set("alpha", "a").Set("mid", "m")

	value, issues := Parse(schema, input)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %#v", issues)
	}
	got := value.(*Obj).Keys()
	want := []string{"zeta", "alpha", "mid"}
	if len(got) != len(want) {
		t.Fatalf("Keys() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Keys() = %v, want %v", got, want)
		}
	}
}

func TestRecordBadKeyReportsTheKeySchemasOwnCodeAndMessage(t *testing.T) {
	schema := Record(commandKeySchema(), String())

	_, issues := Parse(schema, NewObj().Set("Bad Key!", "x"))
	issue := mustIssue(t, issues, "Bad Key!", CodeInvalidFormat)
	want := "must be lower-case letters, digits, and hyphens"
	if issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}
}

func TestRecordSkipsValueValidationForABadKey(t *testing.T) {
	// A key that fails its own schema never has its value parsed at all, so
	// a bad key does not also produce a spurious value issue.
	schema := Record(commandKeySchema(), String().Min(3))

	_, issues := Parse(schema, NewObj().Set("Bad Key!", "x"))
	if len(issues) != 1 {
		t.Fatalf("issues = %#v, want exactly the one bad-key issue", issues)
	}
	mustIssue(t, issues, "Bad Key!", CodeInvalidFormat)
}

func TestRecordReportsEveryBadKeyAndValue(t *testing.T) {
	schema := Record(commandKeySchema(), String().Min(3))

	_, issues := Parse(schema, NewObj().
		Set("ok", "fine").
		Set("Bad!", "x").
		Set("short", "ab"))
	if len(issues) != 2 {
		t.Fatalf("issues = %#v, want 2 (the bad key and the too-short value)", issues)
	}
	mustIssue(t, issues, "Bad!", CodeInvalidFormat)
	mustIssue(t, issues, "short", CodeTooSmall)
}

func TestRecordWrongRootTypeIsFatal(t *testing.T) {
	schema := Record(commandKeySchema(), String())

	_, issues := Parse(schema, "not an object")
	issue := mustIssue(t, issues, "(root)", CodeInvalidType)
	want := "Invalid input: expected object, received string"
	if issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}
}

func TestRecordOmitsAValueThatParsesToUndefined(t *testing.T) {
	// Mirrors ObjectSchema.parse's own guard: a present key whose value
	// schema is Optional and never applies a default leaves the key out of
	// the result rather than setting it to Undefined.
	schema := Record(commandKeySchema(), String().Optional())

	value, issues := Parse(schema, NewObj().Set("present", Undefined))
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %#v", issues)
	}
	obj := value.(*Obj)
	if _, ok := obj.Get("present"); ok {
		t.Errorf("key present in output, want omitted (value parsed to Undefined)")
	}
}

func TestRecordDefaultAndOptionalOmission(t *testing.T) {
	withDefault := Record(commandKeySchema(), String()).Default(NewObj())
	value, issues := Parse(withDefault, Undefined)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %#v", issues)
	}
	if obj, ok := value.(*Obj); !ok || obj.Len() != 0 {
		t.Errorf("Parse(Undefined) = %#v, want an empty *Obj", value)
	}

	optional := Record(commandKeySchema(), String()).Optional()
	value, issues = Parse(optional, Undefined)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %#v", issues)
	}
	if value != Undefined {
		t.Errorf("Parse(Undefined) = %#v, want Undefined", value)
	}
}

func TestRecordPanicsOnNilKeyOrValue(t *testing.T) {
	t.Run("nil key", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("Record(nil, String()) did not panic")
			}
		}()
		Record(nil, String())
	})

	t.Run("nil value", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("Record(commandKeySchema(), nil) did not panic")
			}
		}()
		Record(commandKeySchema(), nil)
	})
}

func TestUnionAcceptsTheFirstMatchingType(t *testing.T) {
	schema := Union(String(), Object(F("cmd", String())).Strict())

	value, issues := Parse(schema, "make build")
	if len(issues) != 0 {
		t.Fatalf("string branch: unexpected issues: %#v", issues)
	}
	if value != "make build" {
		t.Errorf("string branch: value = %#v, want %q", value, "make build")
	}

	value, issues = Parse(schema, NewObj().Set("cmd", "go test ./..."))
	if len(issues) != 0 {
		t.Fatalf("object branch: unexpected issues: %#v", issues)
	}
	obj, ok := value.(*Obj)
	if !ok {
		t.Fatalf("object branch: value = %#v, want *Obj", value)
	}
	if got, _ := obj.Get("cmd"); got != "go test ./..." {
		t.Errorf("object branch: cmd = %#v, want %q", got, "go test ./...")
	}
}

func TestUnionReportsInnerIssuesWhenTypeMatchesButValidationFails(t *testing.T) {
	// A table whose type matches the object branch, but whose own field is
	// wrong, should report that field's own issue — not the generic
	// "expected string or object" — because the object branch DID match.
	schema := Union(String(), Object(F("cmd", String())).Strict())

	_, issues := Parse(schema, NewObj().Set("cmd", 5))
	if len(issues) != 1 {
		t.Fatalf("issues = %#v, want exactly the inner cmd issue", issues)
	}
	issue := mustIssue(t, issues, "cmd", CodeInvalidType)
	want := "Invalid input: expected string, received number"
	if issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}

	// Likewise for a strict object's own unrecognized key: the object
	// branch's type matched, so its own unrecognized_keys issue is what's
	// reported.
	_, issues = Parse(schema, NewObj().Set("cmd", "x").Set("bogus", 1))
	mustIssue(t, issues, "(root)", CodeUnrecognizedKeys)
}

func TestUnionReportsExpectedWhenNoAlternativeTypeMatches(t *testing.T) {
	schema := Union(String(), Object(F("cmd", String())).Strict())

	_, issues := Parse(schema, 5)
	issue := mustIssue(t, issues, "(root)", CodeInvalidType)
	want := "Invalid input: expected string or object, received number"
	if issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}
}

func TestUnionIntBranchTypeMismatchesOnAFractionalNumber(t *testing.T) {
	// Number().Int() reports its own "expected int" issue for 4.5, which is
	// itself a top-level invalid_type issue, so the int branch counts as a
	// type mismatch just like a wrong JS type would — Union moves on to the
	// next alternative, which also type-mismatches, so both are named.
	schema := Union(Number().Int(), String())

	_, issues := Parse(schema, 4.5)
	issue := mustIssue(t, issues, "(root)", CodeInvalidType)
	want := "Invalid input: expected int or string, received number"
	if issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}
}

func TestUnionExpectedNameFlattensANestedUnion(t *testing.T) {
	schema := Union(Union(String(), Number()), Object(F("cmd", String())).Strict())

	_, issues := Parse(schema, true)
	issue := mustIssue(t, issues, "(root)", CodeInvalidType)
	want := "Invalid input: expected string, number or object, received boolean"
	if issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}
}

func TestUnionDefaultAndOptionalOmission(t *testing.T) {
	withDefault := Union(String(), Object(F("cmd", String())).Strict()).Default("fallback")
	value, issues := Parse(withDefault, Undefined)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %#v", issues)
	}
	if value != "fallback" {
		t.Errorf("value = %#v, want %q", value, "fallback")
	}

	optional := Union(String(), Object(F("cmd", String())).Strict()).Optional()
	value, issues = Parse(optional, Undefined)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %#v", issues)
	}
	if value != Undefined {
		t.Errorf("value = %#v, want Undefined", value)
	}
}

func TestUnionAbsenceIgnoresAnAlternativesOwnDefault(t *testing.T) {
	// An alternative's own Default (or Optional) is never consulted for an
	// absent value: absence is decided by the union's own modifiers alone,
	// which here are unset, so Undefined is fatal — even though the first
	// alternative would happily have supplied "x".
	schema := Union(String().Default("x"), Number())

	_, issues := Parse(schema, Undefined)
	issue := mustIssue(t, issues, "(root)", CodeInvalidType)
	want := "Invalid input: expected string or number, received undefined"
	if issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}
}

func TestUnionMissingRequiredFieldNamesEveryAlternative(t *testing.T) {
	schema := Object(
		F("entry", Union(String(), Object(F("cmd", String())).Strict())),
	).Strict()

	_, issues := Parse(schema, NewObj())
	issue := mustIssue(t, issues, "entry", CodeInvalidType)
	want := "Invalid input: expected string or object, received undefined"
	if issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}
}

func TestUnionPanicsOnNoAlternatives(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Union() did not panic")
		}
	}()
	Union()
}

// commandsSchema builds a [commands]-shaped schema the way #94's future
// config schema is expected to: a record of arbitrary command names, each
// either a bare string or a table naming its own cmd (and, in a real
// schema, more fields like cwd) — the case #103 exists for.
func commandsSchema() *ObjectSchema {
	return Object(
		F("commands", Record(commandKeySchema(), Union(
			String(),
			Object(F("cmd", String().Min(1))).Strict(),
		)).Default(NewObj())),
	).Strict()
}

func TestCommandsShapedSchemaReportsABadEntryAtItsOwnPath(t *testing.T) {
	schema := commandsSchema()

	input := NewObj().Set("commands", NewObj().
		Set("build", "make build").
		Set("test", NewObj().Set("cmd", "go test ./...")).
		Set("bad", NewObj().Set("cmd", 5)))

	value, issues := Parse(schema, input)
	if len(issues) != 1 {
		t.Fatalf("issues = %#v, want exactly the one bad entry", issues)
	}
	issue := mustIssue(t, issues, "commands.bad.cmd", CodeInvalidType)
	want := "Invalid input: expected string, received number"
	if issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}

	obj := value.(*Obj)
	commands, _ := obj.Get("commands")
	got := commands.(*Obj).Keys()
	wantKeys := []string{"build", "test", "bad"}
	if len(got) != len(wantKeys) {
		t.Fatalf("commands.Keys() = %v, want %v", got, wantKeys)
	}
	for i := range wantKeys {
		if got[i] != wantKeys[i] {
			t.Fatalf("commands.Keys() = %v, want %v", got, wantKeys)
		}
	}
}

func TestCommandsShapedSchemaReportsAnEntryMatchingNeitherBranch(t *testing.T) {
	schema := commandsSchema()

	input := NewObj().Set("commands", NewObj().Set("bad", 5))

	_, issues := Parse(schema, input)
	issue := mustIssue(t, issues, "commands.bad", CodeInvalidType)
	want := "Invalid input: expected string or object, received number"
	if issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}
}

func TestCommandsShapedSchemaReportsABadCommandName(t *testing.T) {
	schema := commandsSchema()

	input := NewObj().Set("commands", NewObj().Set("Bad Name", "echo hi"))

	_, issues := Parse(schema, input)
	mustIssue(t, issues, "commands.Bad Name", CodeInvalidFormat)
}
