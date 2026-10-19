package checkoutindex

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/logger"
)

// iconFS holds the brand's favicons, copied byte for byte from docs/brand (a
// test keeps the copies in step), so nothing reads docs/ at runtime. The
// index container serves them from the directory beside index.html, and the
// page links them by relative path. Safari before 26 shows neither an SVG
// favicon nor a data: URI one (WebKit bug 236616), so the files are real and
// same-origin, which also keeps the CSP at img-src 'self'.
//
// The 16 px files are pixel-built and only look right at 16 px (the PNGs are
// that same grid at 1x and 2x); the masters serve every larger size.
//
//go:embed assets/favicon-*.svg assets/favicon-*.png
var iconFS embed.FS

// icon is one favicon file and the link that offers it.
type icon struct {
	// name is the file's name, both under assets/ and beside index.html.
	name string
	// mime is its media type, and what the index container must serve it as.
	mime string
	// sizes is the link's sizes attribute: "16x16", "32x32" or "any".
	sizes string
	// scheme is "light" or "dark": the prefers-color-scheme the icon's link
	// names in its media attribute.
	scheme string
}

// icons is every favicon the page links, in link order. PNGs come first and
// are what Safari before 26 uses; the master SVGs follow for browsers that
// prefer them and for large sizes (the pixel-built 16 px SVGs are not linked:
// they are pixel for pixel the 16 px PNGs). The 32x32 PNG is for Retina tab
// strips.
//
// Every icon's link carries media="(prefers-color-scheme: ...)", and in each
// pair the dark link comes first and the light one after it. Chrome and Edge
// honour media: in light mode only the light links match, in dark mode only
// the dark ones. Safari ignores media on icon links and, as observed in Safari
// on a light tab bar, takes the last matching link, so the light one goes last
// and it shows the light icon. Firefox most likely ignores media there too
// (Mozilla bug 1603885 is open), so it gets the same. That is wrong on a dark
// tab bar, a known limit: those browsers offer links no way to choose. prefers-color-scheme
// resolves to light or dark only (MDN: light also covers "no active
// preference"), so no link without media is needed as a fallback, and one
// placed first would only compete with the dark link in a browser that picks
// by order. Chromium resolves prefers-color-scheme from the operating
// system, not from the browser's own theme, so a dark tab strip on a light
// system gets the light icon; links cannot fix that either.
var icons = []icon{
	{"favicon-32-dark.png", "image/png", "32x32", "dark"},
	{"favicon-32.png", "image/png", "32x32", "light"},
	{"favicon-16-dark.png", "image/png", "16x16", "dark"},
	{"favicon-16.png", "image/png", "16x16", "light"},
	{"favicon-master-dark.svg", "image/svg+xml", "any", "dark"},
	{"favicon-master.svg", "image/svg+xml", "any", "light"},
}

// IconNames is the name of every icon file Refresh writes beside the index
// page, in link order.
func IconNames() []string {
	names := make([]string, len(icons))
	for i, ic := range icons {
		names[i] = ic.name
	}
	return names
}

// iconBytes is the embedded content of one icon file.
func iconBytes(name string) ([]byte, error) {
	return iconFS.ReadFile("assets/" + name)
}

// iconMode is the mode the icon files are written with, the index page's own:
// readable by the cap-dropped index container.
const iconMode = 0o644

// WriteIcons writes every icon file into dir, the directory index.html lives
// in, so a page that links them never points at a missing file; callers write
// the icons before the page. A file that already holds the right bytes at the
// right mode is left alone, so a refresh that changes nothing writes nothing.
//
// A write that fails does not always leave the page without an icon. For each
// icon that could not be written, WriteIcons checks whether a regular file is
// already there (a copy from an earlier refresh): if so the failure goes in
// warning, and the page can still be written with the older icon; if not it
// goes in err, and the page must not be written. Both join every failure,
// each naming its icon. The other icons are still attempted either way.
//
// A write is replace-by-rename (files.AtomicWrite), so a symlink planted at an
// icon's path is replaced and never written through, and iconCurrent uses
// Lstat so a symlink is never taken for a current file. The router's
// assertion that the icon paths are safe (secureState) is defence in depth.
func WriteIcons(dir string) (warning, err error) {
	return writeIcons(dir, files.AtomicWrite)
}

// iconWarning is the one message a tolerated icon write failure is logged
// with, whoever writes the icons.
const iconWarning = "Could not update the favicon files beside the checkout index; keeping the earlier ones: %s"

// EnsureIcons is WriteIcons with the policy callers share: a failure over an
// icon an earlier write left is logged as a warning and the caller goes on to
// write its page; a failure with an icon missing is returned, and the caller
// must not write a page that links it.
func EnsureIcons(dir string, log logger.Logger) error {
	return ensureIcons(dir, log, WriteIcons)
}

// ensureIcons is EnsureIcons with the writer given, for Service's test hook.
func ensureIcons(dir string, log logger.Logger, write func(dir string) (warning, err error)) error {
	warning, err := write(dir)
	if err != nil {
		return err
	}
	if warning != nil {
		log.Warn(fmt.Sprintf(iconWarning, warning))
	}
	return nil
}

// writeIcons is WriteIcons with the file writer given, so a test can make a
// write fail where the filesystem would not.
func writeIcons(dir string, write func(path string, data []byte, mode fs.FileMode) error) (warning, err error) {
	var kept, lost []error
	for _, ic := range icons {
		want, readErr := iconBytes(ic.name)
		if readErr != nil {
			return nil, fmt.Errorf("reading the embedded icon %s: %w", ic.name, readErr)
		}
		path := filepath.Join(dir, ic.name)
		if iconCurrent(path, want) {
			continue
		}
		if writeErr := write(path, want, iconMode); writeErr != nil {
			failure := fmt.Errorf("writing the icon %s: %w", path, writeErr)
			if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() {
				kept = append(kept, failure)
			} else {
				lost = append(lost, failure)
			}
		}
	}
	return errors.Join(kept...), errors.Join(lost...)
}

// iconCurrent reports whether path is a regular file holding want at
// iconMode. Any doubt (a missing file, a symlink, a read error) is false,
// which makes WriteIcons write it again.
func iconCurrent(path string, want []byte) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != iconMode {
		return false
	}
	have, err := os.ReadFile(path)
	return err == nil && bytes.Equal(have, want)
}

// link is the page's <link rel="icon"> for ic. href is relative, so it
// resolves beside index.html wherever the page is served from.
func (ic icon) link() string {
	return fmt.Sprintf(`<link rel="icon" type="%s" sizes="%s" media="(prefers-color-scheme: %s)" href="%s">`,
		ic.mime, ic.sizes, ic.scheme, ic.name)
}

// renderFaviconLinks is the page's icon links, each on its own line of the
// head.
func renderFaviconLinks() string {
	links := make([]string, len(icons))
	for i, ic := range icons {
		links[i] = ic.link()
	}
	return strings.Join(links, "\n  ")
}
