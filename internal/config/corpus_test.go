package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/text"
)

// corpusCase is one entry of the hand-maintained testdata/config-corpus.json
// (its origin is in git history): a TOML fixture plus the exact
// byte-for-byte result the real parseConfig (real smol-toml, real zod
// schema, under Node) produced for it. The service cases are hand-edited.
type corpusCase struct {
	Name            string `json:"name"`
	TOML            string `json:"toml"`
	OK              bool   `json:"ok"`
	Want            string `json:"want"`
	TOMLSyntaxError bool   `json:"tomlSyntaxError"`
}

// TestConfigCorpus runs every fixture in testdata/config-corpus.json through
// Parse and compares byte for byte — except a TOML syntax error's own detail
// text, which Go's TOML library phrases differently than smol-toml's; those
// cases only check the "<file> is not valid TOML:" prefix.
func TestConfigCorpus(t *testing.T) {
	raw, err := os.ReadFile("testdata/config-corpus.json")
	if err != nil {
		t.Fatalf("reading testdata/config-corpus.json: %v", err)
	}
	var cases []corpusCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parsing testdata/config-corpus.json: %v", err)
	}
	if len(cases) < 40 {
		t.Fatalf("only %d corpus cases, expected at least 40; testdata/config-corpus.json needs more fixtures", len(cases))
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			cfg, err := Parse([]byte(c.TOML), "test.toml")
			switch {
			case c.OK && err != nil:
				t.Fatalf("Parse: unexpected error: %v", err)
			case c.OK:
				got, err := text.SafeJSON(cfg)
				if err != nil {
					t.Fatalf("text.SafeJSON: %v", err)
				}
				if got != c.Want {
					t.Errorf("Parse result =\n%s\nwant\n%s", got, c.Want)
				}
			case !c.OK && err == nil:
				t.Fatalf("Parse: expected an error, got none")
			case c.TOMLSyntaxError:
				prefix := "test.toml is not valid TOML:"
				if !strings.HasPrefix(err.Error(), prefix) {
					t.Errorf("error = %q, want prefix %q", err.Error(), prefix)
				}
			default:
				if err.Error() != c.Want {
					t.Errorf("error =\n%s\nwant\n%s", err.Error(), c.Want)
				}
			}
		})
	}
}
