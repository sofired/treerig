package shape

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// TestJSONSchema pins the draft-07 rendering of every builder.
func TestJSONSchema(t *testing.T) {
	name := String().Regex(regexp.MustCompile(`^[a-z]+$`), "slug").Describe("A name.")
	schema := Object(
		F("name", String().Min(1).Max(9).Describe("Its <name>.")),
		F("port", Number().Int().Min(1).Max(65535).Optional().Describe("A port.")),
		F("ratio", Number().Min(0.5).Default(0.75).Describe("A ratio.")),
		F("on", Bool().Default(true).Describe("A switch.")),
		F("tags", Array(String().Describe("A tag.")).Max(3).Default([]any{}).Describe("Tags.")),
		F("by-name", Record(name, Union(String().Describe("Short."), Object(F("x", Bool().Describe("X."))).Strict().Describe("Long.")).Describe("Either.")).
			Max(2, "too many").Default(NewObj()).Describe("Entries.")),
		F("code", String().SchemaPattern(regexp.MustCompile(`^\d+$`)).Refine(func(string) bool { return true }, "m").Optional().Describe("Digits.")),
		F("two", String().Regex(regexp.MustCompile(`^a`), "a").Regex(regexp.MustCompile(`z$`), "z").Optional().Describe("Two patterns.")),
	).Strict().Describe("Root.")

	const want = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "$id": "https://example.test/t.json",
  "title": "t",
  "type": "object",
  "description": "Root.",
  "properties": {
    "name": {
      "type": "string",
      "description": "Its <name>.",
      "minLength": 1,
      "maxLength": 9
    },
    "port": {
      "type": "integer",
      "description": "A port.",
      "minimum": 1,
      "maximum": 65535
    },
    "ratio": {
      "type": "number",
      "description": "A ratio.",
      "minimum": 0.5,
      "default": 0.75
    },
    "on": {
      "type": "boolean",
      "description": "A switch.",
      "default": true
    },
    "tags": {
      "type": "array",
      "description": "Tags.",
      "items": {
        "type": "string",
        "description": "A tag."
      },
      "maxItems": 3,
      "default": []
    },
    "by-name": {
      "type": "object",
      "description": "Entries.",
      "propertyNames": {
        "type": "string",
        "description": "A name.",
        "pattern": "^[a-z]+$"
      },
      "additionalProperties": {
        "description": "Either.",
        "anyOf": [
          {
            "type": "string",
            "description": "Short."
          },
          {
            "type": "object",
            "description": "Long.",
            "properties": {
              "x": {
                "type": "boolean",
                "description": "X."
              }
            },
            "required": [
              "x"
            ],
            "additionalProperties": false
          }
        ]
      },
      "maxProperties": 2,
      "default": {}
    },
    "code": {
      "type": "string",
      "description": "Digits.",
      "pattern": "^\\d+$"
    },
    "two": {
      "type": "string",
      "description": "Two patterns.",
      "pattern": "^a",
      "allOf": [
        {
          "pattern": "z$"
        }
      ]
    }
  },
  "required": [
    "name"
  ],
  "additionalProperties": false
}
`
	if got := string(JSONSchema(schema, "https://example.test/t.json", "t")); got != want {
		t.Errorf("JSONSchema =\n%s\nwant\n%s", got, want)
	}
}

// TestRecordHintAndMax covers the two Record options that shape its
// parsing: a hint on the wrong-type issue, and an entry limit.
func TestRecordHintAndMax(t *testing.T) {
	key := String().Regex(regexp.MustCompile(`^[a-z]+$`), "slug")
	schema := Record(key, Bool()).Hint("write a table").Max(1, "at most one")

	_, issues := Parse(schema, []any{})
	if len(issues) != 1 || issues[0].Message != "Invalid input: expected object, received array (write a table)" {
		t.Errorf("wrong type issues = %+v", issues)
	}
	_, issues = Parse(schema, NewObj().Set("a", true).Set("b", false))
	if len(issues) != 1 || issues[0].Code != CodeTooBig || issues[0].Message != "at most one" || len(issues[0].Path) != 0 {
		t.Errorf("too many issues = %+v", issues)
	}
	if _, issues = Parse(schema, NewObj().Set("a", true)); len(issues) != 0 {
		t.Errorf("one entry issues = %+v", issues)
	}
}

// TestPatternsMustBeECMACompatible pins what setPattern refuses.
func TestPatternsMustBeECMACompatible(t *testing.T) {
	for _, pattern := range []string{`(?i)abc`, `(?P<x>a)`, `\Aabc`, `abc\z`, `[[:alpha:]]+`} {
		t.Run("refuses "+pattern, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("no panic for %s", pattern)
				}
			}()
			JSONSchema(Object(F("x", String().Regex(regexp.MustCompile(pattern), "m"))), "id", "t")
		})
	}
	t.Run("accepts a non-capturing group and classes", func(t *testing.T) {
		out := JSONSchema(Object(F("x", String().Regex(regexp.MustCompile(`^(?:a|b)[a-z]\d$`), "m"))), "id", "t")
		if !strings.Contains(string(out), `"pattern": "^(?:a|b)[a-z]\\d$"`) {
			t.Errorf("pattern missing:\n%s", out)
		}
	})
}

// TestWriteJSONString covers the escaping JSONSchema's own writer does.
func TestWriteJSONString(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "abc", `"abc"`},
		{"quote and backslash", `a"b\c`, `"a\"b\\c"`},
		{"newline and tab", "a\nb\tc", `"a\nb\tc"`},
		{"control characters", "\x00\x01\x1f", `"\u0000\u0001\u001f"`},
		{"DEL", "\x7f", `"\u007f"`},
		{"HTML is not escaped", "<a&b>", `"<a&b>"`},
		{"invalid UTF-8 becomes U+FFFD", "a\xffb", "\"a\ufffdb\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			writeJSONString(&b, tc.in)
			if b.String() != tc.want {
				t.Errorf("writeJSONString(%q) = %s, want %s", tc.in, b.String(), tc.want)
			}
		})
	}
}

// TestKeyHint checks a hint is added for a rejected key that has one, and
// only then.
func TestKeyHint(t *testing.T) {
	schema := Object(F("a", Bool().Optional())).Strict().
		KeyHint("name", func(path []string) string { return "at " + PathString(path) })
	_, issues := Parse(schema, NewObj().Set("name", true))
	if len(issues) != 1 || issues[0].Message != `Unrecognized key: "name" (at (root))` {
		t.Errorf("hinted key issues = %+v", issues)
	}
	_, issues = Parse(schema, NewObj().Set("other", true))
	if len(issues) != 1 || issues[0].Message != `Unrecognized key: "other"` {
		t.Errorf("other key issues = %+v", issues)
	}
}

// TestRecordMaxDefaultMessage covers Max without a message.
func TestRecordMaxDefaultMessage(t *testing.T) {
	_, issues := Parse(Record(String(), Bool()).Max(1), NewObj().Set("a", true).Set("b", true))
	if len(issues) != 1 || issues[0].Message != "Too big: expected object to have <=1 entries" {
		t.Errorf("issues = %+v", issues)
	}
}

// TestCodeOnlyChecks lists a Refine no SchemaPattern restates and a
// SuperRefine, and not a Refine that one does.
func TestCodeOnlyChecks(t *testing.T) {
	yes := func(string) bool { return true }
	pattern := regexp.MustCompile(`^a`)
	schema := Object(
		F("plain", String().Refine(yes, "m")),
		F("restated", String().SchemaPattern(pattern).Refine(yes, "m")),
		F("nested", Record(String().Refine(yes, "m"), Object(F("deep", String().Refine(yes, "m"))))),
	).SuperRefine(func(*Obj, func([]string, string)) {})
	got := strings.Join(CodeOnlyChecks(schema), ",")
	if want := "(root),plain,nested.<key>,nested.*.deep"; got != want {
		t.Errorf("CodeOnlyChecks = %s, want %s", got, want)
	}
}
