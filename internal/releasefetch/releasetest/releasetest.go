// Package releasetest serves a fake release over TLS for tests of `teardown pin
// --release` and `--latest`: the four platform archives of docs/design/release.md,
// a checksums.txt that lists them, and a bundle for cosign (whose verdict a
// test supplies, so the bytes are placeholders). It also builds archives,
// hostile ones included.
package releasetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/sofired/treerig/internal/releasefetch"
	"github.com/sofired/treerig/internal/teardown/teardowntest"
)

// Release is one release served by an httptest TLS server.
type Release struct {
	// Version is X.Y.Z, the release's version without a leading v.
	Version string
	// Server serves the release; Client trusts its certificate.
	Server *httptest.Server
	// Binaries are the treerig bytes in each platform's archive.
	Binaries map[string][]byte

	mu sync.Mutex
	// files are the served files, by name under /releases/download/v<Version>/.
	files map[string][]byte
	// status makes a file answer that status instead.
	status map[string]int
	// redirects makes a file answer 302 to that URL.
	redirects map[string]string
	// latest is the version releases/latest/download/checksums.txt serves the
	// checksums of; empty answers 404, as a repository with only pre-releases does.
	latest string
	// nextLatest, if set, replaces latest once it has been served: a release
	// published between discovery and the download.
	nextLatest string
	// requests are the paths requested, in order.
	requests []string
}

// New serves release version over TLS until the test ends. The server's URL is
// the repository: Source() is releasefetch.Source{Repository: that}, so the
// base URL ends /releases/download/v<version>/.
func New(t testing.TB, version string) *Release {
	t.Helper()
	r := &Release{
		Version:   version,
		Binaries:  map[string][]byte{},
		files:     map[string][]byte{},
		status:    map[string]int{},
		redirects: map[string]string{},
	}
	for _, platform := range releasefetch.Platforms {
		r.Binaries[platform] = teardowntest.Executable(platform, " treerig "+version+" for "+platform)
		r.files[releasefetch.ArchiveName(version, platform)] = Archive(
			Entry{Name: "treerig", Mode: 0o755, Body: r.Binaries[platform]},
			Entry{Name: "LICENSE", Mode: 0o644, Body: []byte("license")},
			Entry{Name: "completions/", Type: tar.TypeDir, Mode: 0o755},
			Entry{Name: "completions/treerig.bash", Mode: 0o644, Body: []byte("# completions")},
		)
	}
	r.files["checksums.txt.sigstore.json"] = []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)
	r.UpdateChecksums()
	r.Server = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Server.Close)
	return r
}

// FromDir serves a release GoReleaser built into dir (a snapshot's dist/):
// checksums.txt and the four platform archives of version, read as they are.
// Binaries is left empty, since the archives are not built here. The bundle is
// a placeholder until the test sets one.
func FromDir(t testing.TB, version, dir string) *Release {
	t.Helper()
	r := &Release{
		Version:   version,
		Binaries:  map[string][]byte{},
		files:     map[string][]byte{},
		status:    map[string]int{},
		redirects: map[string]string{},
	}
	names := []string{"checksums.txt"}
	for _, platform := range releasefetch.Platforms {
		names = append(names, releasefetch.ArchiveName(version, platform))
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		r.files[name] = data
	}
	r.files["checksums.txt.sigstore.json"] = []byte(`{}`)
	r.Server = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Server.Close)
	return r
}

// Source is the repository the release is served as.
func (r *Release) Source() releasefetch.Source {
	return releasefetch.Source{Repository: r.Server.URL}
}

// Fetcher is a releasefetch.Fetcher whose client trusts the server.
func (r *Release) Fetcher() releasefetch.Fetcher { return releasefetch.NewHTTP(r.Server.Client()) }

// Base is the URL every file of the release is served under.
func (r *Release) Base() string { return r.Source().ReleaseBase(r.Version) }

// Sum is the lowercase sha256 of data.
func Sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// UpdateChecksums rewrites checksums.txt from the archives now served, with
// the other assets a release lists (a package and the source archive) in
// between, as GoReleaser does.
func (r *Release) UpdateChecksums() {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lines []string
	for _, platform := range releasefetch.Platforms {
		name := releasefetch.ArchiveName(r.Version, platform)
		lines = append(lines, Sum(r.files[name])+"  "+name)
	}
	lines = append(lines,
		Sum([]byte("deb"))+"  treerig_"+r.Version+"_amd64.deb",
		Sum([]byte("source"))+"  treerig-"+r.Version+".tar.gz",
	)
	sort.Strings(lines)
	r.files["checksums.txt"] = []byte(strings.Join(lines, "\n") + "\n")
}

// Set replaces a served file. It does not touch checksums.txt.
func (r *Release) Set(name string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[name] = data
}

// File is a served file's bytes.
func (r *Release) File(name string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.files[name]
}

// Status makes name answer with status.
func (r *Release) Status(name string, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status[name] = status
}

// Redirect makes name answer 302 to target.
func (r *Release) Redirect(name, target string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.redirects[name] = target
}

// SetLatest makes releases/latest/download/checksums.txt serve the checksums
// of the release this server holds under version; "" makes it answer 404.
func (r *Release) SetLatest(version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latest = version
}

// PublishAfterLatest makes the next serving of releases/latest/download/checksums.txt
// be followed by a newer release becoming the latest, as a publish between
// discovery and the download would.
func (r *Release) PublishAfterLatest(next string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextLatest = next
}

// Requests are the paths requested so far, in order.
func (r *Release) Requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

// serve answers the two URL shapes a release has.
func (r *Release) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req.URL.Path)
	latest := "/releases/latest/download/checksums.txt"
	downloads := "/releases/download/v" + r.Version + "/"
	switch {
	case req.URL.Path == latest:
		if r.latest == "" {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(latestChecksums(r.latest))
		if r.nextLatest != "" {
			r.latest, r.nextLatest = r.nextLatest, ""
		}
	case strings.HasPrefix(req.URL.Path, downloads):
		name := strings.TrimPrefix(req.URL.Path, downloads)
		if target, ok := r.redirects[name]; ok {
			http.Redirect(w, req, target, http.StatusFound)
			return
		}
		if status, ok := r.status[name]; ok {
			http.Error(w, fmt.Sprintf("status %d", status), status)
			return
		}
		data, ok := r.files[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(data)
	default:
		http.NotFound(w, req)
	}
}

// latestChecksums is a checksums.txt that names the four archives of version.
func latestChecksums(version string) []byte {
	var b strings.Builder
	for _, platform := range releasefetch.Platforms {
		b.WriteString(Sum([]byte(platform+version)) + "  " + releasefetch.ArchiveName(version, platform) + "\n")
	}
	return []byte(b.String())
}

// Entry is one entry of an archive built by Archive.
type Entry struct {
	Name string
	// Type is a tar type flag; zero means a regular file.
	Type     byte
	Mode     int64
	Body     []byte
	Linkname string
}

// Archive is a tar.gz of entries, in order.
func Archive(entries ...Entry) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.Type
		if typ == 0 {
			typ = tar.TypeReg
		}
		header := &tar.Header{Name: e.Name, Typeflag: typ, Mode: e.Mode, Linkname: e.Linkname}
		if typ == tar.TypeReg {
			header.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(header); err != nil {
			panic(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write(e.Body); err != nil {
				panic(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	if err := gz.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
