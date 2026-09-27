// Package text makes borrowed strings safe to print and gives berth's own
// output a few JavaScript string semantics it depends on for its column
// widths and escaping.
//
// What a config file, a branch name, another process's holder record, or a
// subprocess's stderr put in a string is made safe here. Escape sequences can
// drive a terminal's cursor and overwrite a line that said `Stopped` with one
// that says `Running`, a right-to-left override reverses the rest of a line,
// and a zero-width or soft hyphen makes two different checkouts look identical.
package text

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// IsControl reports whether r is one of the characters Printable neutralizes:
// the C0 controls (0x00–0x1F, including the common whitespace ones), DEL,
// the C1 controls (0x80–0x9F), the line and paragraph separators (U+2028,
// U+2029, which no newline-based split sees), and every Unicode format
// character (category Cf) — bidi overrides and embeddings among them —
// except the zero-width joiners, which carry meaning in ordinary text.
// Deliberately narrower than every control category: private-use and
// unassigned code points are left alone.
func IsControl(r rune) bool {
	switch {
	case r == '\t' || r == '\n' || r == '\r':
		return true
	case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029:
		return true
	case unicode.Is(unicode.Cf, r) && r != 0x200c && r != 0x200d:
		return true
	default:
		return false
	}
}

// HasControl reports whether value contains any character IsControl reports
// true for.
func HasControl(value string) bool {
	for _, r := range value {
		if IsControl(r) {
			return true
		}
	}
	return false
}

// Printable replaces what could drive a terminal with U+FFFD.
//
// Whitespace becomes a space rather than a replacement character: messages
// from Git and Docker are routinely multi-line, and a line break is a place to
// fold, not something to hide. The zero-width joiners are kept — they carry
// meaning in ordinary text. Deliberately narrower than every control
// category: private-use and unassigned code points are left alone.
func Printable(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteByte(' ')
		case IsControl(r):
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// PrintableLines is Printable per line, so the caller's own line breaks survive it.
func PrintableLines(value string) string {
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = Printable(line)
	}
	return strings.Join(lines, "\n")
}

// OneLine is Printable clamped to limit code points (200 when limit <= 0),
// ending in an ellipsis when it had to cut.
func OneLine(value string, limit int) string {
	if limit <= 0 {
		limit = 200
	}
	p := Printable(value)
	if utf8.RuneCountInString(p) <= limit {
		return p
	}
	runes := []rune(p)
	return string(runes[:limit-1]) + "…"
}

// SafeJSON is JSON.stringify(value, null, 2) with every UTF-16 code unit from
// U+007F to U+FFFF escaped as \uXXXX: the values stay exact, and nothing a
// branch name or a service label carries can drive the terminal printing it.
// Callers must not pass nil slices where JSON wants [] — Go encodes them as
// null.
func SafeJSON(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return EscapeNonASCII(strings.TrimSuffix(buffer.String(), "\n")), nil
}

// EscapeNonASCII escapes every UTF-16 code unit from U+007F upward as a
// lower-case \uXXXX, splitting astral characters into surrogate pairs.
func EscapeNonASCII(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r < 0x7f:
			b.WriteRune(r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			high, low := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, high, low)
		}
	}
	return b.String()
}

// JSLen is JavaScript's String.prototype.length: UTF-16 code units, which is
// what every table berth prints pads its columns by.
func JSLen(value string) int {
	n := 0
	for _, r := range value {
		if r > 0xffff {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// PadEnd is JavaScript's padEnd with spaces: pads to width UTF-16 code units.
func PadEnd(value string, width int) string {
	if pad := width - JSLen(value); pad > 0 {
		return value + strings.Repeat(" ", pad)
	}
	return value
}

// JSLower is JavaScript's toLowerCase where it can change a slug: Go's
// strings.ToLower maps İ (U+0130) to a plain "i", while JavaScript applies the
// unconditional special casing and gives "i" followed by U+0307. The
// conditional final-sigma rule only ever yields non-ASCII letters, which a slug
// replaces anyway, so it is not reproduced.
func JSLower(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if r == 0x130 {
			b.WriteString("i̇")
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// CompareUTF16 orders two strings as JavaScript's default sort does, by
// UTF-16 code units. That is byte order everywhere but one place: a character
// from U+E000 to U+FFFF sorts after one beyond U+FFFF, whose surrogates
// (U+D800–U+DFFF) come first in UTF-16 and last in UTF-8.
func CompareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

// TruncateUTF16 is JavaScript's value.slice(0, n): its first n UTF-16 code
// units. A surrogate pair cut in half leaves a lone surrogate, which Node
// writes out as U+FFFD, and so does this.
func TruncateUTF16(value string, n int) string {
	n = max(n, 0) // as slice(0, -1) gives "", never throws
	units := utf16.Encode([]rune(value))
	if len(units) <= n {
		return value
	}
	return string(utf16.Decode(units[:n]))
}

// ShellQuote single-quotes a value for a POSIX shell, so a pasted command
// survives any character in the path.
func ShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
