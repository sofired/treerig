package backend

import (
	"encoding/json"
	"os"
	"testing"
)

// composeConfigCorpusCase is one entry of the hand-maintained
// testdata/compose-config-corpus.json (its origin is in git history): a
// `docker compose config --format json` document, plus whether berth's own
// schema validation accepts it or the exact refusal message it gives.
type composeConfigCorpusCase struct {
	Name   string          `json:"name"`
	Config json.RawMessage `json:"config"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error"`
}

// TestComposeConfigCorpus checks decodeOrderedComposeModel — berth's own
// schema validation for a `docker compose config` document — against every
// fixture in testdata/compose-config-corpus.json: a malformed nested shape
// (a non-object build, a wrongly typed expose or ports entry, an external
// value with no valid shape) must be refused completely before
// overrideDocument ever sees the data.
func TestComposeConfigCorpus(t *testing.T) {
	data, err := os.ReadFile("testdata/compose-config-corpus.json")
	if err != nil {
		t.Fatalf("reading testdata/compose-config-corpus.json: %v", err)
	}
	var cases []composeConfigCorpusCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parsing testdata/compose-config-corpus.json: %v", err)
	}
	if len(cases) < 10 {
		t.Fatalf("only %d corpus cases, expected at least 10; testdata/compose-config-corpus.json needs more fixtures", len(cases))
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
