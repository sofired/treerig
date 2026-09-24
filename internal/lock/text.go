package lock

import (
	"bytes"
	"encoding/json"

	"github.com/sofired/berth/internal/text"
)

// quoteOneLine is JSON.stringify(oneLine(value)) the way advice() in
// src/operation-lock.ts always quoted a holder's hostname or command: quoted
// like the command beside it, so plain prose in a hostname cannot close the
// clause and read as part of this tool's own sentence.
func quoteOneLine(value string) string {
	return jsonString(text.OneLine(value, 0))
}

// jsonString is a JSON string literal without the HTML escaping
// encoding/json applies by default, matching JSON.stringify's output.
func jsonString(value string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	return trimNewline(buf.String())
}

func trimNewline(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\n' {
		return s[:n-1]
	}
	return s
}
