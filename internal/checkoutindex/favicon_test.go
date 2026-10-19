package checkoutindex

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// faviconCopies maps each embedded favicon to the brand file it is a copy of.
func faviconCopies() map[string]struct{ brand, embedded string } {
	return map[string]struct{ brand, embedded string }{
		"16 px, light":  {"treerig-favicon-16.svg", favicon16},
		"16 px, dark":   {"treerig-favicon-16-dark.svg", favicon16Dark},
		"master, light": {"treerig-mark.svg", faviconMaster},
		"master, dark":  {"treerig-mark-dark.svg", faviconMasterDark},
	}
}

// TestFaviconsMatchBrand fails when an embedded favicon differs from the
// brand file it was copied from, or when that file is missing: the copies in
// assets/ must change together with docs/brand (regenerate with
// docs/brand/source/build.py, then copy).
func TestFaviconsMatchBrand(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "brand")
	for name, pair := range faviconCopies() {
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(dir, pair.brand))
			if err != nil {
				t.Fatalf("reading brand file: %v", err)
			}
			if pair.embedded != string(want) {
				t.Errorf("assets copy differs from docs/brand/%s; copy the brand file over it", pair.brand)
			}
		})
	}
}

// TestFaviconLinks pins the shape of the icon links and the CSP that lets
// the page use them.
func TestFaviconLinks(t *testing.T) {
	html := RenderIndexHTML(renderTestSnapshot())

	t.Run("carries one data URI icon per size and colour scheme", func(t *testing.T) {
		links := regexp.MustCompile(`<link rel="icon" type="image/svg\+xml" sizes="([^"]+)" media="\(prefers-color-scheme: ([a-z]+)\)" href="data:image/svg\+xml;base64,([A-Za-z0-9+/=]+)">`).
			FindAllStringSubmatch(html, -1)
		if len(links) != 4 {
			t.Fatalf("icon links = %d, want 4", len(links))
		}
		want := []struct{ sizes, scheme, svg string }{
			{"16x16", "dark", favicon16Dark},
			{"16x16", "light", favicon16},
			{"any", "dark", faviconMasterDark},
			{"any", "light", faviconMaster},
		}
		for i, link := range links {
			decoded, err := base64.StdEncoding.DecodeString(link[3])
			if err != nil {
				t.Fatalf("link %d: href is not base64: %v", i, err)
			}
			if link[1] != want[i].sizes || link[2] != want[i].scheme || string(decoded) != strings.TrimSpace(want[i].svg) {
				t.Errorf("link %d = sizes %q, scheme %q; want sizes %q, scheme %q, and its own svg",
					i, link[1], link[2], want[i].sizes, want[i].scheme)
			}
		}
	})

	t.Run("each icon file has the viewBox of its size", func(t *testing.T) {
		for name, svg := range map[string]string{"favicon-master.svg": faviconMaster, "favicon-master-dark.svg": faviconMasterDark} {
			if !strings.Contains(svg, `viewBox="0 0 64 64"`) {
				t.Errorf("%s is not the 64-unit master", name)
			}
		}
		for name, svg := range map[string]string{"favicon-16.svg": favicon16, "favicon-16-dark.svg": favicon16Dark} {
			if !strings.Contains(svg, `viewBox="0 0 16 16"`) {
				t.Errorf("%s is not the 16-unit file", name)
			}
		}
	})

	t.Run("the policy allows data: images and nothing else new", func(t *testing.T) {
		// The whole policy, so any widening (an object-src, a remote img-src)
		// fails here rather than passing quietly. Only the script hash varies.
		policy := regexp.MustCompile(`content="(default-src[^"]*)"`).FindStringSubmatch(html)
		if policy == nil {
			t.Fatal("no Content-Security-Policy in the page")
		}
		hashed := regexp.MustCompile(`'sha256-[A-Za-z0-9+/=]+'`).ReplaceAllString(policy[1], "'sha256-HASH'")
		want := "default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'sha256-HASH'; connect-src 'self'; form-action 'none'; base-uri 'none'"
		if hashed != want {
			t.Errorf("policy = %q, want %q", hashed, want)
		}
	})
}
