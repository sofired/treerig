package text

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// Ported from tests/unit/text.test.ts, one t.Run per "it" title so coverage
// can be compared title by title, plus oracle-driven tests below that check
// Printable, SafeJSON and JSLower against internal/text/testdata/corpus.json,
// a hand-maintained fixture generated from the real src/text.ts running
// under Node; its origin is in git history.

func TestPrintable(t *testing.T) {
	t.Run("replaces what a terminal or a reader would be fooled by", func(t *testing.T) {
		if got, want := Printable("a\u001b[2Kb"), "a\ufffd[2Kb"; got != want {
			t.Errorf("Printable(ESC) = %q, want %q", got, want)
		}
		if got, want := Printable("a\u202eb\u200bc\u00adx"), "a\ufffdb\ufffdc\ufffdx"; got != want {
			t.Errorf("Printable(RTL override, ZWSP, soft hyphen) = %q, want %q", got, want)
		}
		if got, want := Printable("a\U000e0041b"), "a\ufffdb"; got != want {
			t.Errorf("Printable(astral tag character) = %q, want %q", got, want)
		}
		// DEL, the 8-bit control range (U+009B is CSI with no ESC in front of
		// it), and the two separators no newline split can see.
		if got, want := Printable("a\u007fb\u009bc\u0085d\u2028e\u2029f"), "a\ufffdb\ufffdc\ufffdd\ufffde\ufffdf"; got != want {
			t.Errorf("Printable(DEL, C1, separators) = %q, want %q", got, want)
		}
	})

	t.Run("keeps whitespace as whitespace, and the joiners that carry meaning", func(t *testing.T) {
		if got, want := Printable("one\ntwo\tthree\r"), "one two three "; got != want {
			t.Errorf("Printable(whitespace) = %q, want %q", got, want)
		}
		if got, want := Printable("\U0001f468\u200d\U0001f4bb m\u200ci"), "\U0001f468\u200d\U0001f4bb m\u200ci"; got != want {
			t.Errorf("Printable(ZWJ/ZWNJ kept) = %q, want %q", got, want)
		}
	})
}

// TestHasControl pins the definition internal/config's schema reuses to
// reject a control character in [service.index] label and purpose (#19):
// HasControl (and its per-rune IsControl) is exactly the set of characters
// Printable itself replaces — C0 (including the whitespace ones), DEL, C1,
// the line/paragraph separators, and every Cf format character except
// ZWJ/ZWNJ.
func TestHasControl(t *testing.T) {
	t.Run("reports every character Printable would replace", func(t *testing.T) {
		for _, r := range []rune{
			'\t', '\n', '\r', // C0 whitespace
			0x00, 0x1b, 0x1f, // other C0 (NUL, ESC, US)
			0x7f,       // DEL
			0x80, 0x9b, // C1
			0x2028, 0x2029, // line, paragraph separator
			0x202e, // right-to-left override (Cf)
			0x200b, // zero-width space (Cf)
			0x00ad, // soft hyphen (Cf)
		} {
			if !IsControl(r) {
				t.Errorf("IsControl(%U) = false, want true", r)
			}
			if !HasControl(string(r)) {
				t.Errorf("HasControl(%U) = false, want true", r)
			}
		}
	})

	t.Run("keeps the zero-width joiners, and ordinary text, as not control", func(t *testing.T) {
		for _, r := range []rune{'a', ' ', '€', 0x200c, 0x200d, '\U0001f468'} {
			if IsControl(r) {
				t.Errorf("IsControl(%U) = true, want false", r)
			}
		}
		if HasControl("hello, world €") {
			t.Error(`HasControl("hello, world €") = true, want false`)
		}
	})

	t.Run("agrees with Printable: a string is unchanged exactly when it has no control character", func(t *testing.T) {
		cases := []string{
			"plain text",
			"a\u001b[2Kb",
			"one\ntwo\tthree\r",
			"\U0001f468\u200d\U0001f4bb m\u200ci",
			"a\u202eb\u200bc\u00adx",
		}
		for _, c := range cases {
			if got, want := HasControl(c), Printable(c) != c; got != want {
				t.Errorf("HasControl(%q) = %v, want %v (Printable changed it: %v)", c, got, want, Printable(c) != c)
			}
		}
	})
}

// Go strings hold bytes and decode per byte, so ill-formed UTF-8 becomes
// one U+FFFD per invalid byte. Node's decoder gives one per maximal
// ill-formed subsequence, so a truncated E2 82 is one replacement there and
// two here.
func TestInvalidUTF8(t *testing.T) {
	t.Run("replaces each invalid byte, not each ill-formed sequence", func(t *testing.T) {
		if got, want := Printable("a\xe2\x82b\xffc"), "a\ufffd\ufffdb\ufffdc"; got != want {
			t.Errorf("Printable = %q, want %q", got, want)
		}
		if got, want := TruncateUTF16("a\xe2\x82b\xffc", 3), "a\ufffd\ufffd"; got != want {
			t.Errorf("TruncateUTF16 = %q, want %q", got, want)
		}
	})
	t.Run("treats a negative limit as zero, as slice(0, n) does", func(t *testing.T) {
		if got := TruncateUTF16("abc", -1); got != "" {
			t.Errorf("TruncateUTF16(abc, -1) = %q, want empty", got)
		}
	})
}

func TestPrintableLines(t *testing.T) {
	t.Run("keeps the line breaks it was given and nothing else that breaks a line", func(t *testing.T) {
		if got, want := PrintableLines("a\nb\u001bc"), "a\nb\ufffdc"; got != want {
			t.Errorf("PrintableLines(newline kept) = %q, want %q", got, want)
		}
		// Carriage return and the Unicode separators are not line breaks here.
		if got, want := PrintableLines("a\rb\u2028c"), "a b\ufffdc"; got != want {
			t.Errorf("PrintableLines(CR, U+2028) = %q, want %q", got, want)
		}
	})
}

func TestOneLine(t *testing.T) {
	t.Run("clamps to something a terminal can show, counting characters not units", func(t *testing.T) {
		if got, want := OneLine("short", 0), "short"; got != want {
			t.Errorf("OneLine(short) = %q, want %q", got, want)
		}
		// Exactly the limit is not too long.
		if got, want := OneLine(strings.Repeat("x", 200), 0), strings.Repeat("x", 200); got != want {
			t.Errorf("OneLine(200 x) = %q, want %q", got, want)
		}
		if got, want := OneLine(strings.Repeat("x", 201), 0), strings.Repeat("x", 199)+"\u2026"; got != want {
			t.Errorf("OneLine(201 x) = %q, want %q", got, want)
		}
		if got, want := OneLine(strings.Repeat("x", 250), 0), strings.Repeat("x", 199)+"\u2026"; got != want {
			t.Errorf("OneLine(250 x) = %q, want %q", got, want)
		}
		// An astral character is one character, not two.
		got := []rune(OneLine(strings.Repeat("\U0001f468", 250), 0))
		if len(got) != 200 {
			t.Errorf("OneLine(250 astral) has %d runes, want 200", len(got))
		}
	})

	t.Run("filters before it clamps", func(t *testing.T) {
		if got, want := OneLine("a\u001bb", 0), "a\ufffdb"; got != want {
			t.Errorf("OneLine(filters) = %q, want %q", got, want)
		}
	})
}

func TestSafeJSON(t *testing.T) {
	t.Run("escapes everything a terminal could act on, and parses back to what it was", func(t *testing.T) {
		value := map[string]string{
			"branch": "feature/\u202ex",
			"note":   "a\u200bb\u007f\u009b",
			"emoji":  "\U0001f468\u200d\U0001f4bb",
		}
		out, err := SafeJSON(value)
		if err != nil {
			t.Fatalf("SafeJSON: %v", err)
		}
		for _, r := range out {
			if r >= 0x7f && r <= 0xffff {
				t.Fatalf("SafeJSON output still contains U+%04X unescaped: %q", r, out)
			}
		}
		if !strings.Contains(out, `\u202e`) {
			t.Errorf("SafeJSON output missing \\u202e: %q", out)
		}
		if !strings.Contains(out, `\u007f`) {
			t.Errorf("SafeJSON output missing \\u007f: %q", out)
		}
		if !strings.Contains(out, `\u009b`) {
			t.Errorf("SafeJSON output missing \\u009b: %q", out)
		}
		var back map[string]string
		if err := json.Unmarshal([]byte(out), &back); err != nil {
			t.Fatalf("json.Unmarshal(SafeJSON output): %v", err)
		}
		for k, v := range value {
			if back[k] != v {
				t.Errorf("round trip [%s] = %q, want %q", k, back[k], v)
			}
		}
	})

	t.Run("keeps an astral character whole across the escape", func(t *testing.T) {
		out, err := SafeJSON(map[string]string{"emoji": "\U0001f468"})
		if err != nil {
			t.Fatalf("SafeJSON: %v", err)
		}
		if !strings.Contains(out, `\ud83d\udc68`) {
			t.Errorf("SafeJSON output missing surrogate pair: %q", out)
		}
		var back map[string]string
		if err := json.Unmarshal([]byte(out), &back); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if back["emoji"] != "\U0001f468" {
			t.Errorf("round trip emoji = %q, want %q", back["emoji"], "\U0001f468")
		}
	})

	t.Run("answers for a value JSON has no text for", func(t *testing.T) {
		out, err := SafeJSON(nil)
		if err != nil {
			t.Fatalf("SafeJSON(nil): %v", err)
		}
		if out != "null" {
			t.Errorf("SafeJSON(nil) = %q, want %q", out, "null")
		}
	})

	t.Run("indents the way a reader expects", func(t *testing.T) {
		out, err := SafeJSON(map[string]int{"a": 1})
		if err != nil {
			t.Fatalf("SafeJSON: %v", err)
		}
		if want := "{\n  \"a\": 1\n}"; out != want {
			t.Errorf("SafeJSON({a:1}) = %q, want %q", out, want)
		}
	})
}

// --- Oracle-driven coverage below, from internal/text/testdata/corpus.json.

type corpus struct {
	CfCodePointCount    int             `json:"cfCodePointCount"`
	PrintableCodePoints []codePointCase `json:"printableCodePoints"`
	PrintableStrings    []codePointCase `json:"printableStrings"`
	SafeJSON            []safeJSONCase  `json:"safeJson"`
	Lower               []lowerCase     `json:"lower"`
	Sort                []sortCase      `json:"sort"`
	Slice               []sliceCase     `json:"slice"`
}

type codePointCase struct {
	Name  string `json:"name"`
	Input string `json:"input"`
	Want  string `json:"want"`
}

type safeJSONCase struct {
	Name        string `json:"name"`
	ValueJSON   string `json:"valueJSON"`
	IsUndefined bool   `json:"isUndefined"`
	Want        string `json:"want"`
}

type lowerCase struct {
	Name  string `json:"name"`
	Input string `json:"input"`
	Want  string `json:"want"`
}

type sortCase struct {
	Name  string   `json:"name"`
	Input []string `json:"input"`
	Want  []string `json:"want"`
}

type sliceCase struct {
	Name  string `json:"name"`
	Input string `json:"input"`
	Limit int    `json:"limit"`
	Want  string `json:"want"`
}

// TestCompareUTF16Oracle sorts as Array.prototype.sort() does with no
// comparator, including where that differs from byte order.
func TestCompareUTF16Oracle(t *testing.T) {
	c := loadCorpus(t)
	if len(c.Sort) == 0 {
		t.Fatal("no sort cases in hand-maintained testdata/corpus.json")
	}
	for _, tc := range c.Sort {
		t.Run(tc.Name, func(t *testing.T) {
			got := slices.Clone(tc.Input)
			slices.SortFunc(got, CompareUTF16)
			if !slices.Equal(got, tc.Want) {
				t.Errorf("sorted %q = %q, want %q", tc.Input, got, tc.Want)
			}
		})
	}
}

// TestTruncateUTF16Oracle slices as String.prototype.slice(0, n) does,
// including through a surrogate pair.
func TestTruncateUTF16Oracle(t *testing.T) {
	c := loadCorpus(t)
	if len(c.Slice) == 0 {
		t.Fatal("no slice cases in hand-maintained testdata/corpus.json")
	}
	for _, tc := range c.Slice {
		t.Run(tc.Name, func(t *testing.T) {
			if got := TruncateUTF16(tc.Input, tc.Limit); got != tc.Want {
				t.Errorf("TruncateUTF16(%q, %d) = %q, want %q", tc.Input, tc.Limit, got, tc.Want)
			}
		})
	}
}

// loadCorpus reads and parses the hand-maintained testdata/corpus.json,
// failing the test if it is missing or malformed.
func loadCorpus(t *testing.T) corpus {
	t.Helper()
	raw, err := os.ReadFile("testdata/corpus.json")
	if err != nil {
		t.Fatalf("reading testdata/corpus.json: %v", err)
	}
	var c corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parsing testdata/corpus.json: %v", err)
	}
	return c
}

// TestPrintableOracle checks Printable against every C0/C1 control, DEL, both
// Unicode paragraph separators, and every code point Node's \p{Cf} matches —
// not a hand-picked sample of Cf, all of it, so a Unicode-version mismatch
// between Go's unicode.Cf table and Node's ICU would show up here.
func TestPrintableOracle(t *testing.T) {
	c := loadCorpus(t)
	if c.CfCodePointCount == 0 {
		t.Fatal("testdata/corpus.json reports zero Cf code points")
	}
	if got := countCf(c.PrintableCodePoints); got != c.CfCodePointCount {
		t.Fatalf("corpus fixture mismatch: %d Cf-named cases, cfCodePointCount says %d", got, c.CfCodePointCount)
	}
	for _, tc := range append(append([]codePointCase{}, c.PrintableCodePoints...), c.PrintableStrings...) {
		t.Run(tc.Name, func(t *testing.T) {
			if got := Printable(tc.Input); got != tc.Want {
				t.Errorf("Printable(%q) = %q, want %q", tc.Input, got, tc.Want)
			}
		})
	}
}

// countCf counts the cases whose Name is tagged "Cf ", for checking the
// corpus's own Cf count against Go's unicode.Cf table.
func countCf(cases []codePointCase) int {
	n := 0
	for _, c := range cases {
		if strings.HasPrefix(c.Name, "Cf ") {
			n++
		}
	}
	return n
}

// TestSafeJSONOracle checks SafeJSON against hostile strings and values,
// generated from real JSON.stringify + the escape regex in src/text.ts.
func TestSafeJSONOracle(t *testing.T) {
	c := loadCorpus(t)
	for _, tc := range c.SafeJSON {
		t.Run(tc.Name, func(t *testing.T) {
			var value any
			if !tc.IsUndefined {
				if err := json.Unmarshal([]byte(tc.ValueJSON), &value); err != nil {
					t.Fatalf("decoding fixture %s: %v", tc.ValueJSON, err)
				}
			}
			got, err := SafeJSON(value)
			if err != nil {
				t.Fatalf("SafeJSON: %v", err)
			}
			if got != tc.Want {
				t.Errorf("SafeJSON(%s) = %q, want %q", tc.ValueJSON, got, tc.Want)
			}
		})
	}
}

// TestJSLowerOracle checks JSLower against real .toLowerCase(), centered on
// U+0130 (the case JSLower exists for) but broad enough to catch a silent
// gap elsewhere in the casing table.
func TestJSLowerOracle(t *testing.T) {
	c := loadCorpus(t)
	for _, tc := range c.Lower {
		t.Run(tc.Name, func(t *testing.T) {
			if got := JSLower(tc.Input); got != tc.Want {
				t.Errorf("JSLower(%q) = %q, want %q", tc.Input, got, tc.Want)
			}
		})
	}
}
