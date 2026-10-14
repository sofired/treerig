package teardown_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/stateformat"
	"github.com/sofired/berth/internal/teardown"
)

// fixture reads a file from testdata.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReadPin(t *testing.T) {
	t.Run("no pin is not an error", func(t *testing.T) {
		l, _ := newControl(t)
		pin, found, err := teardown.ReadPin(l)
		if err != nil || found || pin.Release != "" {
			t.Fatalf("ReadPin = %+v, %v, %v", pin, found, err)
		}
	})

	t.Run("round trip: the fixtures read back to the pins that write them, byte for byte", func(t *testing.T) {
		for name, want := range map[string]teardown.Pin{
			"pin-release.json": releasePin("1.2.3", "checksums", nil),
			"pin-file.json":    filePin("my-build", "binary linux_amd64"),
		} {
			t.Run(name, func(t *testing.T) {
				l, _ := newControl(t)
				data := fixture(t, name)
				writeRaw(t, l, data)
				got, found, err := teardown.ReadPin(l)
				if err != nil || !found {
					t.Fatalf("ReadPin: %v, %v", found, err)
				}
				// The fixture's digests are stand-ins; compare the shape that
				// does not depend on them, then the exact bytes of a rewrite.
				if got.Release != want.Release || got.Source != want.Source || got.IsFile() != want.IsFile() {
					t.Errorf("got %+v", got)
				}
				if err := teardown.WritePin(l, got); err != nil {
					t.Fatal(err)
				}
				rewritten, err := os.ReadFile(l.PinPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(rewritten) != string(data) {
					t.Errorf("rewrite differs from the fixture:\n%s\nwant\n%s", rewritten, data)
				}
				again, _, err := teardown.ReadPin(l)
				if err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(got, again); diff != "" {
					t.Errorf("second read differs: %s", diff)
				}
			})
		}
	})

	t.Run("round trip through Save: what is saved is what is read", func(t *testing.T) {
		l, home := newControl(t)
		pin := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "binary one"})
		if _, err := teardown.Save(l, home, []byte("binary one"), pin, time.Now()); err != nil {
			t.Fatal(err)
		}
		got, found, err := teardown.ReadPin(l)
		if err != nil || !found {
			t.Fatalf("ReadPin: %v, %v", found, err)
		}
		if diff := cmp.Diff(pin, got); diff != "" {
			t.Errorf("pin differs: %s", diff)
		}
	})

	t.Run("a newer version is the use refusal of state.md, whatever else it holds", func(t *testing.T) {
		for name, tc := range map[string]struct{ body, version string }{
			"unknown shape":       {`{"version":2,"anything":["else"]}`, "2"},
			"a known pin's shape": {`{"version":2,"release":5,"platforms":"x"}`, "2"},
			"an unknown key":      {`{"version":9,"surprise":true}`, "9"},
		} {
			body := tc.body
			t.Run(name, func(t *testing.T) {
				l, _ := newControl(t)
				writeRaw(t, l, []byte(body))
				_, found, err := teardown.ReadPin(l)
				if found || !errors.Is(err, stateformat.ErrNewer) || errors.Is(err, teardown.ErrForeign) {
					t.Fatalf("found = %v, err = %v", found, err)
				}
				want := "Refusing to use the teardown pin " + l.PinPath + ": it is format " + tc.version +
					", written by a newer berth, and this build reads format 1. Upgrade berth, then run this again."
				if err.Error() != want {
					t.Errorf("got  %q\nwant %q", err, want)
				}
				if clierr.Code(err) != 1 || !clierr.Expected(err) {
					t.Errorf("exit = %d, expected = %v", clierr.Code(err), clierr.Expected(err))
				}
				if after, _ := os.ReadFile(l.PinPath); string(after) != body {
					t.Errorf("the pin was rewritten: %q", after)
				}
			})
		}
	})

	t.Run("a foreign pin says to delete the directory and pin again, never to upgrade", func(t *testing.T) {
		valid := releasePin("1.2.3", "checksums", map[string]string{"linux_amd64": "b"})
		file := filePin("my-build", "b")
		mutations := map[string]struct {
			pin    teardown.Pin
			mutate func(map[string]any)
		}{
			"no version":                            {valid, func(m map[string]any) { delete(m, "version") }},
			"version zero":                          {valid, func(m map[string]any) { m["version"] = 0 }},
			"a version below the current":           {valid, func(m map[string]any) { m["version"] = -1 }},
			"a string version":                      {valid, func(m map[string]any) { m["version"] = "1" }},
			"a fractional version":                  {valid, func(m map[string]any) { m["version"] = 1.5 }},
			"an unknown key":                        {valid, func(m map[string]any) { m["extra"] = "x" }},
			"an unknown key naming a newer feature": {valid, func(m map[string]any) { m["attestation"] = map[string]any{} }},
			"a key in another case":                 {valid, func(m map[string]any) { m["Release"] = m["release"]; delete(m, "release") }},
			"a missing key":                         {valid, func(m map[string]any) { delete(m, "signer") }},
			"a file pin missing signer":             {file, func(m map[string]any) { delete(m, "signer") }},
			"a file pin missing checksums_sha256":   {file, func(m map[string]any) { delete(m, "checksums_sha256") }},
			"a file pin entry missing archive_sha256": {file, func(m map[string]any) {
				delete(m["platforms"].(map[string]any)["linux_amd64"].(map[string]any), "archive_sha256")
			}},
			"a file pin with a null signer": {file, func(m map[string]any) { m["signer"] = nil }},
			"a file pin with a null archive_sha256": {file, func(m map[string]any) {
				m["platforms"].(map[string]any)["linux_amd64"].(map[string]any)["archive_sha256"] = nil
			}},
			"a null field":          {valid, func(m map[string]any) { m["signer"] = nil }},
			"an unknown nested key": {valid, func(m map[string]any) { m["platforms"].(map[string]any)["linux_amd64"].(map[string]any)["extra"] = 1 }},
			"a nested key in another case": {valid, func(m map[string]any) {
				entry := m["platforms"].(map[string]any)["linux_amd64"].(map[string]any)
				entry["Binary_SHA256"] = entry["binary_sha256"]
				delete(entry, "binary_sha256")
			}},
			"a null nested field": {valid, func(m map[string]any) {
				m["platforms"].(map[string]any)["linux_amd64"].(map[string]any)["binary_sha256"] = nil
			}},
			"a missing archive digest": {valid, func(m map[string]any) {
				delete(m["platforms"].(map[string]any)["linux_arm64"].(map[string]any), "archive_sha256")
			}},
			"platforms not an object":  {valid, func(m map[string]any) { m["platforms"] = []any{} }},
			"a platform not an object": {valid, func(m map[string]any) { m["platforms"].(map[string]any)["linux_amd64"] = "x" }},
			"no platforms":             {valid, func(m map[string]any) { m["platforms"] = map[string]any{} }},
			"an unknown platform": {valid, func(m map[string]any) {
				m["platforms"].(map[string]any)["plan9_amd64"] = map[string]any{"archive_sha256": sum("x")}
			}},
			"a release pin missing a platform": {valid, func(m map[string]any) { delete(m["platforms"].(map[string]any), "darwin_arm64") }},
			"no binary named":                  {releasePin("1.2.3", "c", nil), func(map[string]any) {}},
			"an uppercase digest":              {valid, func(m map[string]any) { m["checksums_sha256"] = strings.ToUpper(valid.ChecksumsSHA256) }},
			"a short digest":                   {valid, func(m map[string]any) { m["checksums_sha256"] = "abc" }},
			"a binary digest too long": {valid, func(m map[string]any) {
				m["platforms"].(map[string]any)["linux_amd64"].(map[string]any)["binary_sha256"] = valid.ChecksumsSHA256 + "0"
			}},
			"a release that is no version": {valid, func(m map[string]any) { m["release"] = "my-build" }},
			"a leading-zero release": {valid, func(m map[string]any) {
				m["release"] = "01.2.3"
				m["source"] = "https://github.com/sofired/berth/releases/download/v01.2.3/"
			}},
			"a release the pattern allows and SemVer does not": {valid, func(m map[string]any) {
				m["release"] = "1.2.3-01"
				m["source"] = "https://github.com/sofired/berth/releases/download/v1.2.3-01/"
			}},
			"an empty binary_sha256": {valid, func(m map[string]any) {
				m["platforms"].(map[string]any)["linux_arm64"].(map[string]any)["binary_sha256"] = ""
			}},
			"a file pin with two platforms": {file, func(m map[string]any) {
				m["platforms"].(map[string]any)["linux_arm64"] = map[string]any{"archive_sha256": "", "binary_sha256": sum("b")}
			}},
			"an empty release":                           {valid, func(m map[string]any) { m["release"] = "" }},
			"a release over 64 characters on a file pin": {file, func(m map[string]any) { m["release"] = strings.Repeat("a", 65) }},
			"a file label with a space":                  {file, func(m map[string]any) { m["release"] = "my build" }},
			"a file label starting with a dot":           {file, func(m map[string]any) { m["release"] = ".build" }},
			"a source for another release": {valid, func(m map[string]any) {
				m["source"] = "https://github.com/sofired/berth/releases/download/v9.9.9/"
			}},
			"an http source":                      {valid, func(m map[string]any) { m["source"] = "http://github.com/sofired/berth/releases/download/v1.2.3/" }},
			"a source without the trailing slash": {valid, func(m map[string]any) { m["source"] = "https://github.com/sofired/berth/releases/download/v1.2.3" }},
			"a source with a query":               {valid, func(m map[string]any) { m["source"] = "https://github.com/x/releases/download/v1.2.3/?a=b" }},
			"a source with a user":                {valid, func(m map[string]any) { m["source"] = "https://u@github.com/x/releases/download/v1.2.3/" }},
			"a source with no host":               {valid, func(m map[string]any) { m["source"] = "https:///releases/download/v1.2.3/" }},
			"a relative file source":              {file, func(m map[string]any) { m["source"] = "file:berth" }},
			"an uncleaned file source":            {file, func(m map[string]any) { m["source"] = "file:/home/me/../me/berth" }},
			"a file pin with a signer":            {file, func(m map[string]any) { m["signer"] = "someone" }},
			"a file pin with checksums":           {file, func(m map[string]any) { m["checksums_sha256"] = sum("x") }},
			"a file pin with an archive digest": {file, func(m map[string]any) {
				m["platforms"].(map[string]any)["linux_amd64"].(map[string]any)["archive_sha256"] = sum("x")
			}},
			"a file pin without a binary digest": {file, func(m map[string]any) {
				delete(m["platforms"].(map[string]any)["linux_amd64"].(map[string]any), "binary_sha256")
			}},
			"an empty signer on a release pin":  {valid, func(m map[string]any) { m["signer"] = "" }},
			"a signer with a control character": {valid, func(m map[string]any) { m["signer"] = "a\x1bb" }},
			"a pinned_at with an offset":        {valid, func(m map[string]any) { m["pinned_at"] = "2026-10-14T12:00:00+00:00" }},
			"a pinned_at with a zone":           {valid, func(m map[string]any) { m["pinned_at"] = "2026-10-14T14:00:00+02:00" }},
			"a pinned_at with fractions":        {valid, func(m map[string]any) { m["pinned_at"] = "2026-10-14T12:00:00.5Z" }},
			"a pinned_at that is no time":       {valid, func(m map[string]any) { m["pinned_at"] = "yesterday" }},
		}
		for name, tc := range mutations {
			t.Run(name, func(t *testing.T) {
				l, _ := newControl(t)
				before := writeMutated(t, l, tc.pin, tc.mutate)
				requireForeign(t, l, before)
			})
		}

		for name, body := range map[string]string{
			"an empty file":             ``,
			"not JSON":                  `{nope`,
			"an array":                  `[1]`,
			"trailing data":             string(fixture(t, "pin-release.json")) + `{}`,
			"a lone version":            `{"version":1}`,
			"a duplicate top-level key": strings.Replace(string(fixture(t, "pin-file.json")), `"release": "my-build",`, `"release": "my-build", "release": "other",`, 1),
			"a duplicate nested key":    strings.Replace(string(fixture(t, "pin-file.json")), `"archive_sha256": "",`, `"archive_sha256": "", "archive_sha256": "",`, 1),
			"the version as null":       `{"version":null}`,
		} {
			t.Run(name, func(t *testing.T) {
				l, _ := newControl(t)
				writeRaw(t, l, []byte(body))
				requireForeign(t, l, []byte(body))
			})
		}
	})

	t.Run("a foreign refusal keeps why behind Cause, not Unwrap, so the report prints it once", func(t *testing.T) {
		l, _ := newControl(t)
		writeRaw(t, l, []byte(`{"version":1,"extra":true}`))
		_, _, err := teardown.ReadPin(l)
		var withCause interface{ Cause() error }
		if !errors.As(err, &withCause) || withCause.Cause() == nil || !strings.Contains(withCause.Cause().Error(), "missing key") && !strings.Contains(withCause.Cause().Error(), "unknown key") {
			t.Fatalf("Cause = %v", withCause)
		}
		if errors.Unwrap(err) != nil {
			t.Errorf("Unwrap = %v: the reporter would print a caused-by line", errors.Unwrap(err))
		}
		writeRaw(t, l, []byte(`{"version":0}`))
		_, _, err = teardown.ReadPin(l)
		if !errors.As(err, &withCause) || withCause.Cause() != nil {
			t.Errorf("a version-foreign pin has a cause: %v", err)
		}
	})

	t.Run("an unreadable pin says what to check, and is neither newer nor foreign", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads anything")
		}
		l, _ := newControl(t)
		writeRaw(t, l, fixture(t, "pin-file.json"))
		if err := os.Chmod(l.PinPath, 0); err != nil {
			t.Fatal(err)
		}
		_, found, err := teardown.ReadPin(l)
		if found || err == nil || errors.Is(err, teardown.ErrForeign) || errors.Is(err, stateformat.ErrNewer) {
			t.Fatalf("found = %v, err = %v", found, err)
		}
		if strings.Count(err.Error(), l.PinPath) != 1 || strings.Contains(err.Error(), "open ") {
			t.Errorf("the path is not named exactly once: %q", err)
		}
		if want := ". Check its permissions, or delete " + l.TeardownDir + "/ and pin again."; !strings.HasPrefix(err.Error(), "Could not read the teardown pin "+l.PinPath+": ") || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("got %q", err)
		}
	})

	t.Run("a valid fixture still reads when its keys are reordered", func(t *testing.T) {
		l, _ := newControl(t)
		writeRaw(t, l, []byte(`{"platforms":{"linux_amd64":{"binary_sha256":"`+sum("b")+`","archive_sha256":""}},"pinned_at":"2026-10-14T12:00:00Z","checksums_sha256":"","signer":"","source":"file:/x","release":"r","version":1}`))
		if _, found, err := teardown.ReadPin(l); err != nil || !found {
			t.Fatalf("ReadPin: %v, %v", found, err)
		}
	})
}

// requireForeign asserts ReadPin refuses with the foreign text and leaves the
// pin as it was.
func requireForeign(t *testing.T, l layout.Control, before []byte) {
	t.Helper()
	_, found, err := teardown.ReadPin(l)
	if found || err == nil {
		t.Fatalf("found = %v, err = %v; want a refusal", found, err)
	}
	want := "Refusing to use the teardown pin " + l.PinPath + ": it is not a pin this build wrote. Delete " + l.TeardownDir + "/ and pin again."
	if err.Error() != want {
		t.Errorf("got  %q\nwant %q", err, want)
	}
	if !errors.Is(err, teardown.ErrForeign) || errors.Is(err, stateformat.ErrNewer) || strings.Contains(err.Error(), "Upgrade") {
		t.Errorf("not a plain foreign refusal: %v", err)
	}
	if clierr.Code(err) != 1 || !clierr.Expected(err) {
		t.Errorf("exit = %d, expected = %v", clierr.Code(err), clierr.Expected(err))
	}
	if after, readErr := os.ReadFile(l.PinPath); readErr != nil || string(after) != string(before) {
		t.Errorf("the pin was changed: %q, %v", after, readErr)
	}
}
