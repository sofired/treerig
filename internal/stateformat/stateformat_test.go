package stateformat_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/stateformat"
)

// TestVersions pins the format version each store writes today
// (docs/design/state.md). Raising one is a change to that store's shape and
// goes with the migration that reads the old one, so it is never a drive-by.
func TestVersions(t *testing.T) {
	t.Run("each store's current version", func(t *testing.T) {
		for name, got := range map[string][2]int{
			"control home":    {stateformat.ControlHome, 2},
			"identity marker": {stateformat.IdentityMarker, 1},
			"registration":    {stateformat.Registration, 1},
			"shared record":   {stateformat.SharedRecord, 1},
			"router state":    {stateformat.RouterState, 1},
			"index card":      {stateformat.IndexCard, 1},
			"docker label":    {stateformat.DockerLabel, 1},
		} {
			if got[0] != got[1] {
				t.Errorf("%s = %d, want %d", name, got[0], got[1])
			}
		}
	})
}

func TestJSON(t *testing.T) {
	for name, tc := range map[string]struct {
		data      string
		verdict   stateformat.Verdict
		wantFound string
	}{
		"the current version":                                   {`{"version":1}`, stateformat.Current, "1"},
		"the current version written as 1.0":                    {`{"version":1.0}`, stateformat.Current, "1"},
		"a newer version":                                       {`{"version":2,"anything":["else"]}`, stateformat.FromNewer, "2"},
		"a newer version beside a shape this build cannot read": {`{"version":7,"records":{"x":1}}`, stateformat.FromNewer, "7"},
		"a version too large to be one anyone wrote":            {`{"version":1e300}`, stateformat.Foreign, ""},
		"no version":           {`{"name":"x"}`, stateformat.Foreign, ""},
		"a null version":       {`{"version":null}`, stateformat.Foreign, ""},
		"version zero":         {`{"version":0}`, stateformat.Foreign, ""},
		"a negative version":   {`{"version":-3}`, stateformat.Foreign, ""},
		"a fractional version": {`{"version":1.5}`, stateformat.Foreign, ""},
		"a string version":     {`{"version":"2"}`, stateformat.Foreign, ""},
		"an array":             {`[1]`, stateformat.Foreign, ""},
		"not JSON":             {`{nope`, stateformat.Foreign, ""},
		"empty":                {``, stateformat.Foreign, ""},
	} {
		t.Run(name, func(t *testing.T) {
			verdict, found := stateformat.JSON([]byte(tc.data), 1)
			if verdict != tc.verdict || (tc.verdict == stateformat.FromNewer && found != tc.wantFound) {
				t.Errorf("JSON(%s) = %v, %q; want %v, %q", tc.data, verdict, found, tc.verdict, tc.wantFound)
			}
		})
	}

	t.Run("judges against the version it is given, not a fixed one", func(t *testing.T) {
		if verdict, _ := stateformat.JSON([]byte(`{"version":2}`), 2); verdict != stateformat.Current {
			t.Errorf("verdict = %v", verdict)
		}
		if verdict, _ := stateformat.JSON([]byte(`{"version":1}`), 2); verdict != stateformat.Foreign {
			t.Errorf("a version below the supported one is not state this build wrote: %v", verdict)
		}
	})
}

func TestLabelAndLine(t *testing.T) {
	t.Run("a label is a plain decimal number", func(t *testing.T) {
		for value, want := range map[string]stateformat.Verdict{
			"1": stateformat.Current, "2": stateformat.FromNewer, "10": stateformat.FromNewer,
			"": stateformat.Foreign, "0": stateformat.Foreign, "01": stateformat.Foreign, "+2": stateformat.Foreign,
			"1.0": stateformat.Foreign, " 1": stateformat.Foreign, "one": stateformat.Foreign, "-1": stateformat.Foreign,
			"99999999999999999999": stateformat.Foreign,
		} {
			if verdict, _ := stateformat.Label(value, 1); verdict != want {
				t.Errorf("Label(%q) = %v, want %v", value, verdict, want)
			}
		}
	})

	t.Run("a line is format and a number, nothing else", func(t *testing.T) {
		for line, want := range map[string]stateformat.Verdict{
			"format 2": stateformat.Current, "format 3": stateformat.FromNewer, "format 1": stateformat.Foreign,
			"format": stateformat.Foreign, "format  2": stateformat.Foreign, "Format 2": stateformat.Foreign,
			"2": stateformat.Foreign, "format 2 ": stateformat.Foreign, "": stateformat.Foreign,
		} {
			if verdict, _ := stateformat.Line(line, 2); verdict != want {
				t.Errorf("Line(%q) = %v, want %v", line, verdict, want)
			}
		}
	})

	t.Run("FormatLine is the line Line reads back as current", func(t *testing.T) {
		if got := stateformat.FormatLine(2); got != "format 2" {
			t.Errorf("FormatLine = %q", got)
		}
		if verdict, _ := stateformat.Line(stateformat.FormatLine(5), 5); verdict != stateformat.Current {
			t.Errorf("verdict = %v", verdict)
		}
	})
}

func TestNewer(t *testing.T) {
	t.Run("names the store, the version found and the version supported, and says to upgrade", func(t *testing.T) {
		err := stateformat.Newer("use", "the router state /home/me/.berth/router/state.json", "3", 1)
		want := "Refusing to use the router state /home/me/.berth/router/state.json: it is format 3, written by a newer berth, and this build reads format 1. Upgrade berth, then run this again."
		if err.Error() != want {
			t.Errorf("got  %q\nwant %q", err.Error(), want)
		}
		if clierr.Code(err) != 1 || !clierr.Expected(err) {
			t.Errorf("exit = %d, expected = %v", clierr.Code(err), clierr.Expected(err))
		}
		var refusal *clierr.Error
		if !errors.As(err, &refusal) || refusal.Cause != nil {
			t.Errorf("not a plain *clierr.Error: %#v", refusal)
		}
	})

	t.Run("is recognisable as a newer berth's state, which no other refusal is", func(t *testing.T) {
		if !errors.Is(stateformat.Newer("use", "x", "2", 1), stateformat.ErrNewer) {
			t.Error("Newer does not wrap ErrNewer")
		}
		if errors.Is(clierr.New("Invalid identity marker: x"), stateformat.ErrNewer) {
			t.Error("another refusal wraps ErrNewer")
		}
	})

	t.Run("takes the verb of what was declined", func(t *testing.T) {
		if got := stateformat.Newer("touch", "container c1", "2", 1).Error(); !strings.HasPrefix(got, "Refusing to touch container c1: ") {
			t.Errorf("got %q", got)
		}
	})
}
