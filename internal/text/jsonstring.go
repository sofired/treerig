package text

import (
	"bytes"
	"fmt"
	"strings"
)

// JSONString quotes s exactly as JavaScript's JSON.stringify does: the quote
// and backslash characters are escaped; backspace, form feed, newline,
// carriage return and tab get their short escape forms; every other C0
// control character (everything below code point 0x20) is written as a
// lowercase four-digit numeric escape; everything else — including DEL
// (0x7F) and the two JavaScript line separators (0x2028 and 0x2029) — is
// written raw.
//
// encoding/json already matches this for every character but those two line
// separators, which it escapes unconditionally (regardless of
// SetEscapeHTML) because they are not valid unescaped in JavaScript
// *source* — irrelevant to JSON, which allows them raw, and exactly what
// JSON.stringify does. A card or an override document that borrows a branch
// name or a route path can carry either, and needs to match TypeScript's
// bytes exactly, so this package writes its own quoting rather than lean on
// an internal that was never a fit for this one case. See
// FixJSONLineSeparators for a writer that already has encoding/json output
// and only needs that one difference fixed.
func JSONString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteByte('\\')
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
			b.WriteByte('\\')
		case '\b':
			b.WriteByte('\\')
			b.WriteByte('b')
		case '\f':
			b.WriteByte('\\')
			b.WriteByte('f')
		case '\n':
			b.WriteByte('\\')
			b.WriteByte('n')
		case '\r':
			b.WriteByte('\\')
			b.WriteByte('r')
		case '\t':
			b.WriteByte('\\')
			b.WriteByte('t')
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, "\\u%04x", r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// lineSeparatorDigits and paragraphSeparatorDigits are the four hex digits
// encoding/json writes after "\u" for code points 0x2028 and 0x2029 — built
// with Sprintf, deliberately, rather than typed as a backslash-u literal
// anywhere in this source file: that literal text is exactly the shape a
// JSON-based tool transport recognizes and silently decodes to the real
// character on the way to disk, which would break the very comparison this
// file exists to get right.
var (
	lineSeparatorDigits      = fmt.Sprintf("%04x", 0x2028)
	paragraphSeparatorDigits = fmt.Sprintf("%04x", 0x2029)
	lineSeparator            = string(rune(0x2028))
	paragraphSeparator       = string(rune(0x2029))
)

// FixJSONLineSeparators undoes encoding/json's unconditional escaping of the
// two JavaScript line separators in data it has already produced (with
// SetEscapeHTML(false)), bringing it into line with JSON.stringify: every
// other character already matches, so this is the one correction a caller
// built on encoding/json — files.AtomicJSON, notably — needs, rather than
// re-quoting the whole document with JSONString.
//
// A plain substring replace is not safe here: encoding/json's own escape for
// a literal backslash is two bytes ('\\', '\\'), and its second byte lines
// up with the first byte of the six-byte line-separator escape, so a naive
// search can find that escape starting one byte into an unrelated one. This
// walks the bytes instead, consuming each escape encoding/json can produce
// as one unit, so a backslash that was itself escaped is never mistaken for
// the start of another escape.
func FixJSONLineSeparators(data []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(data))
	for i := 0; i < len(data); {
		if data[i] != '\\' || i+1 >= len(data) {
			out.WriteByte(data[i])
			i++
			continue
		}
		if data[i+1] != 'u' {
			// A two-byte escape: \\, \", \n, \r, \t, \b, or \f. Copied as a
			// unit so its second byte is never reconsidered as the start of
			// another escape.
			out.Write(data[i : i+2])
			i += 2
			continue
		}
		if i+6 <= len(data) {
			switch string(data[i+2 : i+6]) {
			case lineSeparatorDigits:
				out.WriteString(lineSeparator)
				i += 6
				continue
			case paragraphSeparatorDigits:
				out.WriteString(paragraphSeparator)
				i += 6
				continue
			}
		}
		// Some other \uXXXX escape, or a truncated one: copy it verbatim,
		// still as \u plus whatever follows, two bytes at a time so a
		// digit that happened to be a backslash (never valid hex, but this
		// is meant to be robust rather than assume well-formed input) is
		// not misread either.
		out.Write(data[i : i+2])
		i += 2
	}
	return out.Bytes()
}
