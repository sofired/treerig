package text_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sofired/berth/internal/text"
)

func TestJSONString(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"empty string", "", `""`},
		{"plain text", "hello", `"hello"`},
		{"quote and backslash", `a"b\c`, `"a\"b\\c"`},
		{"short-form controls", "a\b\f\n\r\tb", `"a\b\f\n\r\tb"`},
		{"other C0 controls as \\u00xx lowercase", "a\x00\x01\x1fb", `"a\u0000\u0001\u001fb"`},
		{"U+007F is raw, not escaped", "a\u007fb", "\"a\u007fb\""},
		{"U+2028 and U+2029 are raw, unlike encoding/json", "a b c", "\"a b c\""},
		{"ordinary non-ASCII is raw", "café日本語", "\"café日本語\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := text.JSONString(c.input); got != c.want {
				t.Fatalf("JSONString(%q) = %s, want %s", c.input, got, c.want)
			}
			// Whatever we produce must itself be valid JSON that decodes back
			// to the original string, so this can never diverge from being
			// parseable even as the exact escaping choices change.
			var decoded string
			if err := json.Unmarshal([]byte(text.JSONString(c.input)), &decoded); err != nil {
				t.Fatalf("produced invalid JSON for %q: %v", c.input, err)
			}
			if decoded != c.input {
				t.Fatalf("round trip: got %q, want %q", decoded, c.input)
			}
		})
	}
}

// Built from numeric code points throughout, deliberately: a backslash
// followed by digits, typed literally in a source file, is exactly the kind
// of text a JSON-based tool transport can round-trip through its own
// \uXXXX handling and silently turn into the real character — the very
// confusion this function exists to sort out for callers.
var (
	lineSeparator      = string(rune(0x2028))
	paragraphSeparator = string(rune(0x2029))
	backslash          = string(rune(0x5c))
)

func TestFixJSONLineSeparators(t *testing.T) {
	t.Run("unescapes only the exact six-byte sequences encoding/json produces", func(t *testing.T) {
		original := "a" + lineSeparator + "b" + paragraphSeparator + "c"
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		// Confirms the test's own assumption: encoding/json escapes both
		// separators unconditionally.
		escaped := backslash + "u2028"
		if !bytes.Contains(encoded, []byte(escaped)) {
			t.Fatalf("encoding/json did not escape U+2028 as expected; got %s (test assumption invalid)", encoded)
		}

		fixed := text.FixJSONLineSeparators(encoded)
		if bytes.Contains(fixed, []byte(escaped)) {
			t.Fatalf("expected the escape sequence gone, got %s", fixed)
		}
		if !bytes.Contains(fixed, []byte(lineSeparator)) || !bytes.Contains(fixed, []byte(paragraphSeparator)) {
			t.Fatalf("expected the raw separators in the fixed bytes, got %s", fixed)
		}
		var decoded string
		if err := json.Unmarshal(fixed, &decoded); err != nil {
			t.Fatalf("fixed output is not valid JSON: %v (%s)", err, fixed)
		}
		if decoded != original {
			t.Fatalf("got %q, want %q", decoded, original)
		}
	})

	t.Run("leaves an escaped backslash followed by literal text alone", func(t *testing.T) {
		// The escape sequence for U+2028 (six literal characters: backslash,
		// u, 2, 0, 2, 8) encodes with its backslash itself escaped, as eight
		// bytes -- never the bare six-byte escape sequence -- so it survives untouched.
		original := backslash + "u2028"
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		fixed := text.FixJSONLineSeparators(encoded)
		if string(fixed) != string(encoded) {
			t.Fatalf("got %s, want unchanged %s", fixed, encoded)
		}
		var decoded string
		if err := json.Unmarshal(fixed, &decoded); err != nil || decoded != original {
			t.Fatalf("decoded = %q, %v; want %q, nil", decoded, err, original)
		}
	})

	t.Run("handles a literal backslash immediately followed by a real line separator", func(t *testing.T) {
		// Regression case: encoding/json escapes the leading backslash to two
		// bytes ('\\','\\'), and the second of those bytes is byte-identical
		// to the first byte of the six-byte line-separator escape that
		// follows it — a naive substring search finds a "match" starting one
		// byte late, splitting the escaped backslash in two.
		original := backslash + lineSeparator + "x"
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		fixed := text.FixJSONLineSeparators(encoded)
		var decoded string
		if err := json.Unmarshal(fixed, &decoded); err != nil {
			t.Fatalf("fixed output is not valid JSON: %v (%s)", err, fixed)
		}
		if decoded != original {
			t.Fatalf("got %q, want %q", decoded, original)
		}
	})
}
