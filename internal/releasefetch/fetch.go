package releasefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/semver"
)

// Size caps for what a release serves (docs/design/teardown.md, "Download
// bounds").
const (
	// MaxChecksums is the most checksums.txt may be.
	MaxChecksums int64 = 1 << 20
	// MaxBundle is the most checksums.txt.sigstore.json may be.
	MaxBundle int64 = 1 << 20
	// MaxArchive is the most a platform archive may be.
	MaxArchive int64 = 100 << 20
)

// DefaultRepository is the repository releases are downloaded from.
const DefaultRepository = "https://github.com/" + brand.Repository

// Source names where releases are published. The zero value is the treerig
// repository on GitHub; tests point Repository at an httptest server.
type Source struct {
	// Repository is the https URL of the repository, without a trailing
	// slash; empty means DefaultRepository.
	Repository string
}

// repository is the repository URL without a trailing slash.
func (s Source) repository() string {
	if s.Repository == "" {
		return DefaultRepository
	}
	return strings.TrimRight(s.Repository, "/")
}

// ReleaseBase is the URL every file of release is fetched from, ending in
// /releases/download/v<release>/, the shape the pin store requires of a source.
func (s Source) ReleaseBase(release string) string {
	return s.repository() + "/releases/download/v" + release + "/"
}

// LatestChecksumsURL is where `--latest` discovers the newest release that is
// not a pre-release.
func (s Source) LatestChecksumsURL() string {
	return s.repository() + "/releases/latest/download/checksums.txt"
}

// Fetcher gets the bytes behind an https URL.
type Fetcher interface {
	// Get returns the body of url. The URL must be https, and so must every
	// redirect it leads to. A body larger than limit bytes is an error
	// (*TooLargeError), never a truncated result; a status other than 200 is a
	// *StatusError.
	Get(ctx context.Context, url string, limit int64) ([]byte, error)
}

// StatusError is a response that was not 200.
type StatusError struct {
	URL    string
	Status int
}

// Error names the URL and the status.
func (e *StatusError) Error() string { return fmt.Sprintf("%s answered HTTP %d", e.URL, e.Status) }

// TooLargeError is a body larger than the cap for it.
type TooLargeError struct {
	URL   string
	Limit int64
}

// Error names the URL and the cap.
func (e *TooLargeError) Error() string {
	return fmt.Sprintf("%s is larger than %d bytes", e.URL, e.Limit)
}

// HTTP is the Fetcher over an *http.Client.
type HTTP struct {
	client *http.Client
}

// NewHTTP is a Fetcher that sends its requests through client. It does not
// trust the client to refuse a redirect to plain http: the copy it uses
// always does (RequireHTTPS).
func NewHTTP(client *http.Client) *HTTP {
	own := *client
	own.CheckRedirect = RequireHTTPS
	return &HTTP{client: &own}
}

// RequireHTTPS is an http.Client.CheckRedirect that refuses a redirect to any
// scheme but https and stops after 10 redirects, as the default does.
func RequireHTTPS(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing to follow a redirect to %s: only https is allowed", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// Get implements Fetcher.
func (h *HTTP) Get(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("refusing to fetch %s: only https URLs are allowed", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	// Asking for the identity encoding keeps net/http from decompressing a
	// body on its own, so the cap below counts the bytes that were served.
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{URL: rawURL, Status: resp.StatusCode}
	}
	if resp.ContentLength > limit {
		return nil, &TooLargeError{URL: rawURL, Limit: limit}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, &TooLargeError{URL: rawURL, Limit: limit}
	}
	return body, nil
}

// ErrNoStableRelease is what Latest reports when the repository has no
// release that is not a pre-release: releases/latest answers 404.
var ErrNoStableRelease = errors.New("no release that is not a pre-release")

// archiveName matches a platform archive's file name and captures its version.
var archiveName = regexp.MustCompile(`^` + regexp.QuoteMeta(brand.Name) + `_(.+)_(?:linux|darwin)_(?:amd64|arm64)\.tar\.gz$`)

// Latest is discovery only: it fetches releases/latest/download/checksums.txt,
// reads the version from the platform archives named in it, which must all
// agree and be a valid release, and discards the bytes. The caller then
// behaves as for an explicit --release and fetches everything again from the
// versioned URL, so a release published in between cannot mix two releases'
// files.
func Latest(ctx context.Context, fetcher Fetcher, source Source) (string, error) {
	latestURL := source.LatestChecksumsURL()
	body, err := fetcher.Get(ctx, latestURL, MaxChecksums)
	if err != nil {
		var status *StatusError
		if errors.As(err, &status) && status.Status == http.StatusNotFound {
			return "", ErrNoStableRelease
		}
		return "", err
	}
	version := ""
	for _, line := range strings.Split(string(body), "\n") {
		_, name, ok := strings.Cut(strings.TrimRight(line, "\r"), "  ")
		if !ok {
			continue
		}
		match := archiveName.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		if version != "" && match[1] != version {
			return "", fmt.Errorf("%s names archives of more than one version (%s and %s)", latestURL, version, match[1])
		}
		version = match[1]
	}
	if version == "" {
		return "", fmt.Errorf("%s names no platform archive", latestURL)
	}
	if !semver.IsRelease(version) {
		return "", fmt.Errorf("%s names a version that is not a release version: %q", latestURL, version)
	}
	return version, nil
}
