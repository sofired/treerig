package teardown_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sofired/berth/internal/binstore"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/teardown"
)

// fixedTime is the pinned_at every test pin carries.
const fixedTime = "2026-10-14T12:00:00Z"

// newControl is a control layout under a scratch directory with its control
// home established, and a scratch home for the adoption check. The teardown
// directory does not exist yet.
func newControl(t *testing.T) (layout.Control, string) {
	t.Helper()
	l := layout.ControlLayout(filepath.Join(registrytest.TempDir(t), "control"))
	home := registrytest.TempDir(t)
	if err := files.SecureControlDirectories(l, home); err != nil {
		t.Fatal(err)
	}
	return l, home
}

// sum is the hex sha256 of s, a stand-in digest.
func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// releasePin is a valid release pin for release, signed over checksums, whose
// archive digests cover all four platforms and whose binary digests cover the
// platforms in binaries (platform key to content).
func releasePin(release, checksums string, binaries map[string]string) teardown.Pin {
	pin := teardown.Pin{
		Version:         1,
		Release:         release,
		Source:          "https://github.com/sofired/berth/releases/download/v" + release + "/",
		Signer:          "https://github.com/sofired/berth/.github/workflows/release.yml@refs/tags/v" + release,
		ChecksumsSHA256: sum(checksums),
		PinnedAt:        fixedTime,
		Platforms:       map[string]teardown.Platform{},
	}
	for _, key := range teardown.ReleasePlatforms {
		entry := teardown.Platform{ArchiveSHA256: sum("archive " + key + " " + checksums)}
		if content, ok := binaries[key]; ok {
			entry.BinarySHA256 = binstore.Hash([]byte(content))
		}
		pin.Platforms[key] = entry
	}
	return pin
}

// filePin is a valid file pin labelled label for the linux_amd64 binary content.
func filePin(label, content string) teardown.Pin {
	return teardown.Pin{
		Version:   1,
		Release:   label,
		Source:    "file:/home/me/berth",
		PinnedAt:  fixedTime,
		Platforms: map[string]teardown.Platform{"linux_amd64": {BinarySHA256: binstore.Hash([]byte(content))}},
	}
}

// writeRaw writes data as pin.json, creating the teardown directory.
func writeRaw(t *testing.T, l layout.Control, data []byte) {
	t.Helper()
	if err := os.MkdirAll(l.TeardownDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.PinPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeMutated writes pin.json as the JSON of pin after mutate edited its
// generic form, so a test can break exactly one field or key.
func writeMutated(t *testing.T, l layout.Control, pin teardown.Pin, mutate func(map[string]any)) []byte {
	t.Helper()
	data, err := json.Marshal(pin)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatal(err)
	}
	mutate(generic)
	if data, err = json.Marshal(generic); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, l, data)
	return data
}

// age sets a file's modification time to now minus d.
func age(t *testing.T, path string, now time.Time, d time.Duration) {
	t.Helper()
	when := now.Add(-d)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// exists reports whether path exists, without following a final symlink.
func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatal(err)
	return false
}
