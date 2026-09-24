package backend

import (
	"encoding/json"
	"os"
	"testing"
)

// composeConfigCorpusCase mirrors one entry tools/oracle/compose-config-corpus.ts
// writes: a `docker compose config --format json` document, plus whether the
// TypeScript build's composeConfigSchema (run through the real, private
// readModel) accepted it or the exact refusal message it gave.
type composeConfigCorpusCase struct {
	Name   string          `json:"name"`
	Config json.RawMessage `json:"config"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error"`
}

// TestComposeConfigCorpus checks decodeOrderedComposeModel — this package's
// port of readModel's schema validation — against every fixture in
// testdata/compose-config-corpus.json: a malformed nested shape (a
// non-object build, a wrongly typed expose or ports entry, an external
// value zod's union has no room for) must be refused exactly as completely
// as the TypeScript build refuses it, before overrideDocument ever sees the
// data.
func TestComposeConfigCorpus(t *testing.T) {
	data, err := os.ReadFile("testdata/compose-config-corpus.json")
	if err != nil {
		t.Fatalf("reading testdata/compose-config-corpus.json: %v (regenerate with tools/oracle/compose-config-corpus.ts)", err)
	}
	var cases []composeConfigCorpusCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parsing testdata/compose-config-corpus.json: %v", err)
	}
	if len(cases) < 10 {
		t.Fatalf("only %d corpus cases, expected at least 10; regenerate testdata/compose-config-corpus.json", len(cases))
	}

	const composeFile = "/work/acme/docker/compose.yaml"
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			_, _, _, err := decodeOrderedComposeModel(c.Config, composeFile)
			if c.OK {
				if err != nil {
					t.Fatalf("expected the document to be accepted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error %q, got none", c.Error)
			}
			if err.Error() != c.Error {
				t.Fatalf("got %q, want %q", err.Error(), c.Error)
			}
		})
	}
}
