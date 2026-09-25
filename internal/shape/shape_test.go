package shape

import (
	"regexp"
	"testing"
)

// These exercise the generic engine directly — independent of internal/config
// — against zod 4.4.3 behaviour verified interactively under Node when the
// engine was written. internal/config's own
// corpus_test.go is the larger byte-for-byte check, through a real schema;
// these pin down shape's own contract so a future caller (a registry record,
// an index card, Compose JSON) can trust it without re-deriving each rule.

// mustIssue finds and returns the issue at path with code among issues,
// failing the test if none matches.
func mustIssue(t *testing.T, issues []Issue, path string, code string) Issue {
	t.Helper()
	for _, i := range issues {
		if PathString(i.Path) == path && i.Code == code {
			return i
		}
	}
	t.Fatalf("no issue with path %q code %q in %#v", path, code, issues)
	return Issue{}
}

func TestObjectKeyOrderFollowsSchemaNotInput(t *testing.T) {
	schema := Object(
		F("z", String()),
		F("a", String()),
		F("m", String()),
	).Strict()

	// Input built out of order, the way an unordered map would present it.
	input := NewObj().Set("a", "1").Set("z", "2").Set("m", "3")

	value, issues := Parse(schema, input)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %#v", issues)
	}
	got := value.(*Obj).Keys()
	want := []string{"z", "a", "m"}
	if len(got) != len(want) {
		t.Fatalf("Keys() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Keys() = %v, want %v", got, want)
		}
	}
}

func TestObjectDefaultsAndOptionalOmission(t *testing.T) {
	schema := Object(
		F("required", String()),
		F("withDefault", String().Default("fallback")),
		F("optional", String().Optional()),
	).Strict()

	value, issues := Parse(schema, NewObj().Set("required", "x"))
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %#v", issues)
	}
	obj := value.(*Obj)
	if got, ok := obj.Get("withDefault"); !ok || got != "fallback" {
		t.Errorf("withDefault = %v, %v; want fallback, true", got, ok)
	}
	if _, ok := obj.Get("optional"); ok {
		t.Errorf("optional key present in output, want omitted (zod omits an absent optional field)")
	}
}

func TestUnrecognizedKeysSingularAndPlural(t *testing.T) {
	schema := Object(F("a", String())).Strict()

	_, issues := Parse(schema, NewObj().Set("a", "x").Set("bogus", 1))
	issue := mustIssue(t, issues, "(root)", CodeUnrecognizedKeys)
	if issue.Message != `Unrecognized key: "bogus"` {
		t.Errorf("message = %q, want %q", issue.Message, `Unrecognized key: "bogus"`)
	}

	_, issues = Parse(schema, NewObj().Set("a", "x").Set("zeta", 1).Set("alpha", 2))
	issue = mustIssue(t, issues, "(root)", CodeUnrecognizedKeys)
	if want := `Unrecognized keys: "zeta", "alpha"`; issue.Message != want {
		t.Errorf("message = %q, want %q (zod lists them in source order)", issue.Message, want)
	}

	// Divergence 6b in docs/port/divergences.md: a key with a control
	// character in it is quoted by Go's %q, where zod's JSON.stringify would
	// write \u0001.
	_, issues = Parse(schema, NewObj().Set("a", "x").Set("b\x01d", 1))
	issue = mustIssue(t, issues, "(root)", CodeUnrecognizedKeys)
	if want := `Unrecognized key: "b\x01d"`; issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}
}

func TestSuperRefineGating(t *testing.T) {
	dupCheck := func(value *Obj, add func(path []string, message string)) {
		add([]string{"marker"}, "always fires if reached")
	}

	t.Run("a non-fatal issue (too_small) still lets superRefine run", func(t *testing.T) {
		schema := Object(
			F("name", String().Min(3)),
		).SuperRefine(dupCheck)
		_, issues := Parse(schema, NewObj().Set("name", "ab"))
		mustIssue(t, issues, "name", CodeTooSmall)
		mustIssue(t, issues, "marker", CodeCustom)
	})

	t.Run("a fatal issue (invalid_type) suppresses superRefine", func(t *testing.T) {
		schema := Object(
			F("name", String()),
		).SuperRefine(dupCheck)
		_, issues := Parse(schema, NewObj()) // name missing entirely
		mustIssue(t, issues, "name", CodeInvalidType)
		for _, i := range issues {
			if i.Path != nil && PathString(i.Path) == "marker" {
				t.Fatalf("superRefine ran despite a fatal issue: %#v", issues)
			}
		}
	})

	t.Run("an unrecognized key suppresses superRefine", func(t *testing.T) {
		schema := Object(
			F("name", String()),
		).Strict().SuperRefine(dupCheck)
		_, issues := Parse(schema, NewObj().Set("name", "x").Set("bogus", 1))
		mustIssue(t, issues, "(root)", CodeUnrecognizedKeys)
		for _, i := range issues {
			if PathString(i.Path) == "marker" {
				t.Fatalf("superRefine ran despite an unrecognized key: %#v", issues)
			}
		}
	})

	t.Run("a fatal issue deep in a nested object still suppresses the root's superRefine", func(t *testing.T) {
		schema := Object(
			F("child", Object(F("name", String())).Strict()),
		).SuperRefine(dupCheck)
		_, issues := Parse(schema, NewObj().Set("child", NewObj()))
		mustIssue(t, issues, "child.name", CodeInvalidType)
		for _, i := range issues {
			if PathString(i.Path) == "marker" {
				t.Fatalf("superRefine ran despite a nested fatal issue: %#v", issues)
			}
		}
	})
}

func TestNumberIntAcceptsWholeFloat(t *testing.T) {
	schema := Number().Int().Min(1).Max(65_535)

	value, issues := Parse(schema, float64(80))
	if len(issues) != 0 {
		t.Fatalf("Parse(80.0): unexpected issues: %#v", issues)
	}
	if value != 80 {
		t.Errorf("Parse(80.0) = %v, want 80 (int)", value)
	}

	_, issues = Parse(schema, 80.5)
	issue := mustIssue(t, issues, "(root)", CodeInvalidType)
	if want := "Invalid input: expected int, received number"; issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}
}

func TestNumberTypeMismatchNeverSaysInt(t *testing.T) {
	// Verified against zod 4.4.3: z.number().int().safeParse("80") reports
	// "expected number", not "expected int" — .int() only changes the
	// message once the value is confirmed to be a number.
	schema := Number().Int()
	_, issues := Parse(schema, "80")
	issue := mustIssue(t, issues, "(root)", CodeInvalidType)
	if want := "Invalid input: expected number, received string"; issue.Message != want {
		t.Errorf("message = %q, want %q", issue.Message, want)
	}
}

func TestStringChainedChecksAllRun(t *testing.T) {
	// Every check in a chain runs regardless of an earlier one failing,
	// matching zod: a value can carry more than one issue for a single field.
	schema := String().
		Regex(regexp.MustCompile(`^[a-z]+$`), "must be lower-case letters").
		Max(3, "must be at most 3 characters")

	_, issues := Parse(schema, "ABCDEF")
	if len(issues) != 2 {
		t.Fatalf("issues = %#v, want 2 (both the regex and the max check)", issues)
	}
	mustIssue(t, issues, "(root)", CodeInvalidFormat)
	mustIssue(t, issues, "(root)", CodeTooBig)
}

func TestArrayDefaultAndElementPaths(t *testing.T) {
	schema := Array(String().Min(2)).Default([]any{})

	value, issues := Parse(schema, Undefined)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %#v", issues)
	}
	if items, ok := value.([]any); !ok || len(items) != 0 {
		t.Errorf("Parse(Undefined) = %#v, want an empty, non-nil []any", value)
	}

	_, issues = Parse(schema, []any{"ok", "x"})
	mustIssue(t, issues, "1", CodeTooSmall)
}

// TestArrayMax pins ArraySchema.Max, added for internal/config's own
// [[service]] limit (#16, row 16 in the divergence ledger): a custom message
// is reported verbatim, and the check runs whether or not an element itself
// also failed, matching StringSchema.Max's own "every check runs" contract.
func TestArrayMax(t *testing.T) {
	schema := Array(String()).Max(2, "at most 2 items")

	if _, issues := Parse(schema, []any{"a", "b"}); len(issues) != 0 {
		t.Fatalf("at the limit: unexpected issues: %#v", issues)
	}

	_, issues := Parse(schema, []any{"a", "b", "c"})
	if len(issues) != 1 || issues[0].Message != "at most 2 items" || issues[0].Code != CodeTooBig {
		t.Fatalf("over the limit: issues = %#v, want one CodeTooBig \"at most 2 items\"", issues)
	}

	// Elements are parsed before Max applies, so an over-long array with a
	// bad element reports both.
	_, issues = Parse(schema, []any{"a", 7, "c"})
	if len(issues) != 2 {
		t.Fatalf("over the limit with a bad element: issues = %#v, want an element error and CodeTooBig", issues)
	}
	mustIssue(t, issues, "1", CodeInvalidType)
	mustIssue(t, issues, "(root)", CodeTooBig)
}

func TestObjMarshalJSONPreservesOrder(t *testing.T) {
	obj := NewObj().Set("z", 1).Set("a", 2)
	got, err := obj.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if want := `{"z":1,"a":2}`; string(got) != want {
		t.Errorf("MarshalJSON() = %s, want %s", got, want)
	}
}
