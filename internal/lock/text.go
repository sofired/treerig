package lock

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/sofired/berth/internal/text"
)

// quoteOneLine is JSON.stringify(oneLine(value)) the way advice() in
// src/operation-lock.ts always quoted a holder's hostname or command: quoted
// like the command beside it, so plain prose in a hostname cannot close the
// clause and read as part of this tool's own sentence.
func quoteOneLine(value string) string {
	return jsonString(text.OneLine(value, 0))
}

// HolderLabel is holderLabel from src/operation-lock.ts, ported unchanged: a
// record's PID and host, with its command in front when it recorded one. nil
// (no record at all) is "an unidentified holder", exactly as there. Exported
// for doctor's Locks section, which needs the same holder label a lock's own
// wait notice does (see quoteOneLine and docs/port/divergences.md row 3).
func HolderLabel(rec *Record) string {
	if rec == nil {
		return "an unidentified holder"
	}
	where := fmt.Sprintf("PID %d on %s", rec.PID, quoteOneLine(rec.Host))
	if rec.Command == "" {
		return where
	}
	return fmt.Sprintf("%s (%s)", quoteOneLine(rec.Command), where)
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
