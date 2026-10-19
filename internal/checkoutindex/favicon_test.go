package checkoutindex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/logger"
)

// brandOf maps each served icon file to the docs/brand file it is a copy of.
var brandOf = map[string]string{
	"favicon-master.svg":      "treerig-mark.svg",
	"favicon-master-dark.svg": "treerig-mark-dark.svg",
	"favicon-16.png":          "treerig-favicon-16.png",
	"favicon-16-dark.png":     "treerig-favicon-16-dark.png",
	"favicon-32.png":          "treerig-favicon-32.png",
	"favicon-32-dark.png":     "treerig-favicon-32-dark.png",
}

// TestFaviconsMatchBrand fails when an embedded favicon differs from the
// brand file it was copied from, when that file is missing, or when an icon
// the page serves has no brand file named here: the copies in assets/ must
// change together with docs/brand (regenerate with docs/brand/source/build.py,
// then copy).
func TestFaviconsMatchBrand(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "brand")
	if len(brandOf) != len(icons) {
		t.Fatalf("%d brand mappings for %d icons", len(brandOf), len(icons))
	}
	for _, name := range IconNames() {
		t.Run(name, func(t *testing.T) {
			brand, ok := brandOf[name]
			if !ok {
				t.Fatalf("no brand file named for %s", name)
			}
			want, err := os.ReadFile(filepath.Join(dir, brand))
			if err != nil {
				t.Fatalf("reading brand file: %v", err)
			}
			have, err := iconBytes(name)
			if err != nil {
				t.Fatalf("reading the embedded copy: %v", err)
			}
			if !bytes.Equal(have, want) {
				t.Errorf("assets copy differs from docs/brand/%s; copy the brand file over it", brand)
			}
		})
	}
}

// TestFaviconLinks pins the shape of the icon links, that each names a file
// Refresh writes, and the CSP that lets the page use them.
func TestFaviconLinks(t *testing.T) {
	html := RenderIndexHTML(renderTestSnapshot())
	linkRE := regexp.MustCompile(`<link rel="icon" type="([^"]+)" sizes="([^"]+)" media="\(prefers-color-scheme: ([a-z]+)\)" href="([^"]+)">`)

	t.Run("carries one relative icon link per file, in order", func(t *testing.T) {
		links := linkRE.FindAllStringSubmatch(html, -1)
		if len(links) != len(icons) || strings.Count(html, "<link ") != len(icons) {
			t.Fatalf("icon links = %d, want %d", len(links), len(icons))
		}
		// In each pair the dark link comes first and the light one last:
		// Safari ignores media and takes the last match, so it shows the
		// light icon; browsers that honour media match one link of the pair.
		want := [][4]string{
			{"image/png", "32x32", "dark", "favicon-32-dark.png"},
			{"image/png", "32x32", "light", "favicon-32.png"},
			{"image/png", "16x16", "dark", "favicon-16-dark.png"},
			{"image/png", "16x16", "light", "favicon-16.png"},
			{"image/svg+xml", "any", "dark", "favicon-master-dark.svg"},
			{"image/svg+xml", "any", "light", "favicon-master.svg"},
		}
		for i, link := range links {
			if [4]string(link[1:]) != want[i] {
				t.Errorf("link %d = %v, want %v", i, link[1:], want[i])
			}
		}
	})

	t.Run("every embedded favicon asset is linked, and nothing else is", func(t *testing.T) {
		matches, err := fs.Glob(iconFS, "assets/favicon-*")
		if err != nil {
			t.Fatal(err)
		}
		var embedded []string
		for _, m := range matches {
			embedded = append(embedded, strings.TrimPrefix(m, "assets/"))
		}
		slices.Sort(embedded)
		linked := slices.Sorted(slices.Values(IconNames()))
		if !slices.Equal(embedded, linked) {
			t.Errorf("embedded favicon assets = %v, linked = %v", embedded, linked)
		}
	})

	t.Run("embeds no data URI", func(t *testing.T) {
		if strings.Contains(html, "data:") {
			t.Error("the page carries a data: URI")
		}
	})

	t.Run("links only files Refresh writes, each of the type it declares", func(t *testing.T) {
		h := newServiceHarness(t, &dockerFake{}, nil, nil)
		if _, err := h.service.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		dir := filepath.Dir(h.service.opts.IndexFile)
		page, err := os.ReadFile(h.service.opts.IndexFile)
		if err != nil {
			t.Fatal(err)
		}
		links := linkRE.FindAllStringSubmatch(string(page), -1)
		if len(links) == 0 {
			t.Fatal("the written page has no icon links")
		}
		for _, link := range links {
			if strings.ContainsAny(link[4], `/:\`) {
				t.Errorf("href %q is not a plain file name beside index.html", link[4])
			}
			if _, err := os.Stat(filepath.Join(dir, link[4])); err != nil {
				t.Errorf("link to %s, which Refresh did not write: %v", link[4], err)
			}
			if got := mimeOfName(link[4]); got != link[1] {
				t.Errorf("%s is linked as %s, but its extension means %s", link[4], link[1], got)
			}
		}
	})

	t.Run("each icon is the size its link says", func(t *testing.T) {
		for _, ic := range icons {
			data, err := iconBytes(ic.name)
			if err != nil {
				t.Fatalf("%s: %v", ic.name, err)
			}
			switch ic.mime {
			case "image/png":
				cfg, err := png.DecodeConfig(bytes.NewReader(data))
				if err != nil {
					t.Fatalf("%s does not decode: %v", ic.name, err)
				}
				if got := sizeName(cfg.Width, cfg.Height); got != ic.sizes {
					t.Errorf("%s is %s, link says %s", ic.name, got, ic.sizes)
				}
			case "image/svg+xml":
				box := `viewBox="0 0 64 64"`
				if ic.sizes == "16x16" {
					box = `viewBox="0 0 16 16"`
				}
				if !strings.Contains(string(data), box) {
					t.Errorf("%s does not have %s", ic.name, box)
				}
			default:
				t.Errorf("%s has unexpected type %s", ic.name, ic.mime)
			}
		}
	})

	t.Run("the policy is img-src 'self' with no data: and nothing else new", func(t *testing.T) {
		// The whole policy, so any widening (an object-src, a remote or data:
		// img-src) fails here rather than passing quietly. Only the script
		// hash varies.
		policy := regexp.MustCompile(`content="(default-src[^"]*)"`).FindStringSubmatch(html)
		if policy == nil {
			t.Fatal("no Content-Security-Policy in the page")
		}
		hashed := regexp.MustCompile(`'sha256-[A-Za-z0-9+/=]+'`).ReplaceAllString(policy[1], "'sha256-HASH'")
		want := "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; script-src 'sha256-HASH'; connect-src 'self'; form-action 'none'; base-uri 'none'"
		if hashed != want {
			t.Errorf("policy = %q, want %q", hashed, want)
		}
	})
}

// sizeName is "WxH", the form a link's sizes attribute takes.
func sizeName(w, h int) string {
	return fmt.Sprintf("%dx%d", w, h)
}

// mimeOfName is the type a file's extension stands for, as a static server
// picks it.
func mimeOfName(name string) string {
	switch filepath.Ext(name) {
	case ".png":
		return "image/png"
	case ".svg":
		return "image/svg+xml"
	}
	return ""
}

// TestWriteIcons covers what Refresh and the router rely on: every icon is
// written at the served mode, a second call writes nothing, and a stale or
// wrong-mode file is put right.
func TestWriteIcons(t *testing.T) {
	t.Run("writes every icon at 0644, byte for byte", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteIcons(t, dir)
		for _, name := range IconNames() {
			want, err := iconBytes(name)
			if err != nil {
				t.Fatal(err)
			}
			have, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || !bytes.Equal(have, want) {
				t.Errorf("%s: read err %v, bytes match %v", name, err, bytes.Equal(have, want))
			}
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o644 {
				t.Errorf("%s mode = %o, want 0644", name, info.Mode().Perm())
			}
		}
	})

	t.Run("is idempotent: a second call leaves every file untouched", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteIcons(t, dir)
		before := inodes(t, dir)
		mustWriteIcons(t, dir)
		after := inodes(t, dir)
		for name, id := range before {
			if after[name] != id {
				t.Errorf("%s was rewritten (inode %v, then %v)", name, id, after[name])
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != len(icons) {
			t.Errorf("%d files in the directory, want %d (a leftover temp file?)", len(entries), len(icons))
		}
	})

	t.Run("replaces a file with other bytes or another mode", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteIcons(t, dir)
		stale, wrongMode := filepath.Join(dir, "favicon-16.png"), filepath.Join(dir, "favicon-master.svg")
		if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(wrongMode, 0o600); err != nil {
			t.Fatal(err)
		}
		mustWriteIcons(t, dir)
		want, err := iconBytes("favicon-16.png")
		if err != nil {
			t.Fatal(err)
		}
		have, err := os.ReadFile(stale)
		if err != nil || !bytes.Equal(have, want) {
			t.Errorf("the stale file was not replaced (read err %v)", err)
		}
		info, err := os.Stat(wrongMode)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("mode = %o, want 0644", info.Mode().Perm())
		}
	})

	t.Run("a failed write over an earlier copy is a warning; with no copy it is an error", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteIcons(t, dir)
		const kept, lost = "favicon-16.png", "favicon-32.png"
		if err := os.WriteFile(filepath.Join(dir, kept), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, lost)); err != nil {
			t.Fatal(err)
		}
		failing := func(path string, _ []byte, _ fs.FileMode) error {
			if filepath.Base(path) == kept || filepath.Base(path) == lost {
				return errors.New("disk on fire")
			}
			return nil
		}
		warning, err := writeIcons(dir, failing)
		if warning == nil || !strings.Contains(warning.Error(), kept) || !strings.Contains(warning.Error(), "disk on fire") || strings.Contains(warning.Error(), lost) {
			t.Errorf("warning = %v, want only %s, with the cause", warning, kept)
		}
		if err == nil || !strings.Contains(err.Error(), lost) || !strings.Contains(err.Error(), "disk on fire") || strings.Contains(err.Error(), kept) {
			t.Errorf("err = %v, want only %s, with the cause", err, lost)
		}
	})

	t.Run("a directory planted at an icon is an error, not a warning", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "favicon-32.png"), 0o755); err != nil {
			t.Fatal(err)
		}
		warning, err := WriteIcons(dir)
		if err == nil || warning != nil || !strings.Contains(err.Error(), "favicon-32.png") {
			t.Errorf("warning = %v, err = %v; want an error naming favicon-32.png", warning, err)
		}
		// The other icons were still written.
		if _, statErr := os.Stat(filepath.Join(dir, "favicon-16.png")); statErr != nil {
			t.Errorf("the other icons were not attempted: %v", statErr)
		}
	})

	t.Run("replaces a symlink rather than writing through it", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, []byte("untouched"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "favicon-32.png")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("no symlinks: %v", err)
		}
		mustWriteIcons(t, dir)
		have, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(have) != "untouched" {
			t.Errorf("the symlink target was written through: %q", have)
		}
		if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
			t.Errorf("the link was not replaced by a regular file: %v", err)
		}
	})
}

// TestEnsureIcons pins the policy Refresh and Router.write share: a tolerated
// failure is one warning with the shared message, a fatal one is returned and
// logs nothing, and success logs nothing.
func TestEnsureIcons(t *testing.T) {
	cases := []struct {
		name         string
		warning, err error
		wantErr      bool
		wantLog      []string
	}{
		{"success logs nothing", nil, nil, false, nil},
		{"a tolerated failure warns once and returns nil", errors.New("writing the icon favicon-16.png: disk on fire"), nil, false,
			[]string{"warn: Could not update the favicon files beside the checkout index; keeping the earlier ones: writing the icon favicon-16.png: disk on fire"}},
		{"a fatal failure is returned and not logged", nil, errors.New("writing the icon favicon-32.png: disk on fire"), true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &logger.Recording{}
			err := ensureIcons(t.TempDir(), log, func(string) (error, error) { return tc.warning, tc.err })
			if (err != nil) != tc.wantErr || (tc.wantErr && err != tc.err) {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
			if got := snapshotLines(log); !slices.Equal(got, tc.wantLog) && !(len(got) == 0 && len(tc.wantLog) == 0) {
				t.Errorf("logged %q, want %q", got, tc.wantLog)
			}
		})
	}
	t.Run("EnsureIcons writes the files and logs nothing on a clean directory", func(t *testing.T) {
		log := &logger.Recording{}
		dir := t.TempDir()
		if err := EnsureIcons(dir, log); err != nil || len(snapshotLines(log)) != 0 {
			t.Fatalf("err = %v, logged %q", err, snapshotLines(log))
		}
		if len(inodes(t, dir)) != len(icons) {
			t.Error("the icons were not written")
		}
	})
}

// mustWriteIcons is WriteIcons with any warning or error fatal.
func mustWriteIcons(t *testing.T, dir string) {
	t.Helper()
	if warning, err := WriteIcons(dir); warning != nil || err != nil {
		t.Fatalf("WriteIcons: warning %v, error %v", warning, err)
	}
}

// TestRefreshIconFailures pins what Refresh does when an icon cannot be
// written, and the order of its writes: icons first, then the page.
func TestRefreshIconFailures(t *testing.T) {
	t.Run("writes the icons before the page", func(t *testing.T) {
		h := newServiceHarness(t, &dockerFake{}, nil, nil)
		var pageWhenIconsWritten []bool
		h.service.writeIcons = func(dir string) (error, error) {
			_, err := os.Stat(h.service.opts.IndexFile)
			pageWhenIconsWritten = append(pageWhenIconsWritten, err == nil)
			return WriteIcons(dir)
		}
		for i := 0; i < 2; i++ {
			if _, err := h.service.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
		}
		// The first refresh starts with no page, so a page already there when
		// its icons are written means the page was written first.
		if len(pageWhenIconsWritten) != 2 || pageWhenIconsWritten[0] {
			t.Errorf("page existed before the first refresh's icons were written: %v", pageWhenIconsWritten)
		}
	})

	t.Run("a first write with an icon blocked writes no page and returns the error", func(t *testing.T) {
		h := newServiceHarness(t, &dockerFake{}, nil, nil)
		dir := filepath.Dir(h.service.opts.IndexFile)
		if err := os.Mkdir(filepath.Join(dir, "favicon-16.png"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := h.service.Refresh(context.Background())
		if err == nil || !strings.Contains(err.Error(), "favicon-16.png") {
			t.Fatalf("Refresh error = %v, want one naming favicon-16.png", err)
		}
		if _, statErr := os.Stat(h.service.opts.IndexFile); !os.IsNotExist(statErr) {
			t.Errorf("a page was written with an icon missing: %v", statErr)
		}
	})

	t.Run("a failed write over an earlier copy still refreshes the page, with a warning naming the icon", func(t *testing.T) {
		h := newServiceHarness(t, &dockerFake{}, nil, nil)
		dir := filepath.Dir(h.service.opts.IndexFile)
		if _, err := h.service.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		// An earlier copy with other bytes, and a write that fails for it.
		stale := filepath.Join(dir, "favicon-16.png")
		if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		h.service.writeIcons = func(dir string) (error, error) {
			return writeIcons(dir, func(path string, data []byte, mode fs.FileMode) error {
				if path == stale {
					return errors.New("read-only file system")
				}
				return files.AtomicWrite(path, data, mode)
			})
		}
		before, err := os.ReadFile(h.service.opts.IndexFile)
		if err != nil {
			t.Fatal(err)
		}
		h.service.opts.Now = func() time.Time { return time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC) }
		if _, err := h.service.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		after, err := os.ReadFile(h.service.opts.IndexFile)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(before, after) {
			t.Error("the page was not refreshed")
		}
		if !containsSubstring(snapshotLines(h.logger), "favicon-16.png") || !containsSubstring(snapshotLines(h.logger), "read-only file system") {
			t.Errorf("no warning naming the icon and the error: %v", snapshotLines(h.logger))
		}
	})
}

// TestRefreshWritesIcons covers Refresh's own part: the icons land beside the
// page, and a second refresh leaves them alone.
func TestRefreshWritesIcons(t *testing.T) {
	h := newServiceHarness(t, &dockerFake{}, nil, nil)
	dir := filepath.Dir(h.service.opts.IndexFile)
	if _, err := h.service.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	before := inodes(t, dir)
	for _, name := range IconNames() {
		if _, ok := before[name]; !ok {
			t.Errorf("Refresh did not write %s", name)
		}
	}
	if _, err := h.service.Refresh(context.Background()); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	after := inodes(t, dir)
	for _, name := range IconNames() {
		if before[name] != after[name] {
			t.Errorf("the second Refresh rewrote %s", name)
		}
	}
}

// inodes maps each file name in dir to its inode, so a test can tell a file
// left alone from one replaced by an atomic write (which renames a new file
// over it).
func inodes(t *testing.T, dir string) map[string]uint64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]uint64{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			ids[entry.Name()] = uint64(stat.Ino)
		}
	}
	return ids
}
