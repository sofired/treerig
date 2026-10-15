package teardown_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/teardown"
)

// binaryOf is the digest of content, as a pin records it.
func binaryOf(content string) string { return binstore.Hash([]byte(content)) }

func TestUpdate(t *testing.T) {
	t.Run("no pin: the incoming pin is written", func(t *testing.T) {
		incoming := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"})
		got := teardown.Update(teardown.Pin{}, false, incoming)
		if got.Kind != teardown.Written || got.Older || cmp.Diff(incoming, got.Pin) != "" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("the same checksums: the platform entry is merged, the others are kept and pinned_at is refreshed", func(t *testing.T) {
		existing := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "linux build"})
		incoming := releasePin("1.2.3", "c", map[string]string{"darwin_arm64": "mac build"})
		incoming.PinnedAt = "2026-10-15T08:30:00Z"
		got := teardown.Update(existing, true, incoming)
		if got.Kind != teardown.Merged || got.Older {
			t.Fatalf("got %+v", got)
		}
		want := existing
		want.PinnedAt = "2026-10-15T08:30:00Z"
		want.Platforms = cloneWith(existing.Platforms, "darwin_arm64", incoming.Platforms["darwin_arm64"])
		if diff := cmp.Diff(want, got.Pin); diff != "" {
			t.Errorf("merged pin differs: %s", diff)
		}
		if got.Pin.Platforms["linux_amd64"].BinarySHA256 != binaryOf("linux build") {
			t.Error("the other platform's binary was dropped")
		}
		if err := got.Pin.Validate(); err != nil {
			t.Errorf("the merged pin is invalid: %v", err)
		}
	})

	t.Run("a merge does not touch the existing pin it was given", func(t *testing.T) {
		existing := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "linux build"})
		incoming := releasePin("1.2.3", "c", map[string]string{"darwin_arm64": "mac build"})
		before := cloneWith(existing.Platforms, "x", teardown.Platform{})
		delete(before, "x")
		teardown.Update(existing, true, incoming)
		if diff := cmp.Diff(before, existing.Platforms); diff != "" {
			t.Errorf("existing changed: %s", diff)
		}
	})

	t.Run("a merge keeps the existing binary of a platform the incoming command only recorded an archive for", func(t *testing.T) {
		existing := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "linux build", "darwin_arm64": "mac build"})
		incoming := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "linux build"}) // darwin: archive only
		got := teardown.Update(existing, true, incoming)
		if got.Pin.Platforms["darwin_arm64"].BinarySHA256 != binaryOf("mac build") {
			t.Errorf("darwin_arm64 lost its binary: %+v", got.Pin.Platforms["darwin_arm64"])
		}
	})

	t.Run("a merge takes the incoming binary for a platform both installed", func(t *testing.T) {
		existing := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "first"})
		incoming := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "second"})
		got := teardown.Update(existing, true, incoming)
		if got.Pin.Platforms["linux_amd64"].BinarySHA256 != binaryOf("second") {
			t.Errorf("got %+v", got.Pin.Platforms["linux_amd64"])
		}
	})

	t.Run("a merge replaces an entry whose archive digest differs, whole", func(t *testing.T) {
		existing := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "first"})
		incoming := releasePin("1.2.3", "c", nil)
		changed := incoming.Platforms["linux_amd64"]
		changed.ArchiveSHA256 = sum("another archive")
		incoming.Platforms["linux_amd64"] = changed
		got := teardown.Update(existing, true, incoming)
		if got.Pin.Platforms["linux_amd64"] != changed {
			t.Errorf("got %+v, want %+v", got.Pin.Platforms["linux_amd64"], changed)
		}
	})

	t.Run("a merge keeps the existing identity fields", func(t *testing.T) {
		existing := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"})
		incoming := releasePin("1.2.3", "c", map[string]string{"darwin_arm64": "m"})
		incoming.Signer = "someone else"
		incoming.Source = "https://example.com/x/releases/download/v1.2.3/"
		got := teardown.Update(existing, true, incoming)
		if got.Pin.Signer != existing.Signer || got.Pin.Source != existing.Source || got.Pin.Release != existing.Release {
			t.Errorf("got %+v", got.Pin)
		}
	})

	t.Run("a merge that would leave no binary is refused by the writer", func(t *testing.T) {
		// The unreachable case: one checksums.txt, two archive digests for a
		// platform. If it were reached, the whole-entry replacement could drop
		// the only binary, and WritePin must refuse the result.
		existing := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"})
		incoming := releasePin("1.2.3", "c", nil)
		changed := incoming.Platforms["linux_amd64"]
		changed.ArchiveSHA256 = sum("another archive")
		incoming.Platforms["linux_amd64"] = changed
		merged := teardown.Update(existing, true, incoming)
		l, _ := newControl(t)
		err := teardown.WritePin(l, merged.Pin)
		if err == nil || !strings.Contains(err.Error(), "no platform names a binary") {
			t.Fatalf("WritePin = %v", err)
		}
		if exists(t, l.PinPath) {
			t.Error("pin.json was written")
		}
	})

	t.Run("another release is replaced", func(t *testing.T) {
		existing := releasePin("1.1.0", "old", map[string]string{"linux_amd64": "old"})
		incoming := releasePin("1.2.3", "new", map[string]string{"linux_amd64": "new"})
		got := teardown.Update(existing, true, incoming)
		if got.Kind != teardown.Replaced || got.Older || cmp.Diff(incoming, got.Pin) != "" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("the same release with other checksums is replaced, not merged", func(t *testing.T) {
		existing := releasePin("1.2.3", "one", map[string]string{"linux_amd64": "a"})
		incoming := releasePin("1.2.3", "two", map[string]string{"darwin_arm64": "b"})
		got := teardown.Update(existing, true, incoming)
		if got.Kind != teardown.Replaced || len(got.Pin.BinaryHashes()) != 1 || got.Pin.BinaryHashes()[0] != binaryOf("b") {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("another source with the same release and checksums merges: the checksums are what was verified", func(t *testing.T) {
		existing := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"})
		incoming := releasePin("1.2.3", "c", map[string]string{"darwin_arm64": "m"})
		incoming.Source = "https://mirror.example/treerig/releases/download/v1.2.3/"
		if got := teardown.Update(existing, true, incoming); got.Kind != teardown.Merged {
			t.Errorf("Kind = %v", got.Kind)
		}
	})

	t.Run("a downgrade is allowed and named", func(t *testing.T) {
		existing := releasePin("1.3.0", "new", map[string]string{"linux_amd64": "new"})
		incoming := releasePin("1.2.0", "old", map[string]string{"linux_amd64": "old"})
		got := teardown.Update(existing, true, incoming)
		if got.Kind != teardown.Replaced || !got.Older || cmp.Diff(incoming, got.Pin) != "" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("ranks releases by SemVer, not as text", func(t *testing.T) {
		for _, tc := range []struct {
			existing, incoming string
			older              bool
		}{
			{"1.9.0", "1.10.0", false},
			{"1.10.0", "1.9.0", true},
			{"1.0.0", "1.0.0-rc.1", true},
			{"1.0.0-rc.1", "1.0.0", false},
			{"1.0.0-beta.2", "1.0.0-beta.11", false},
			{"0.1.0-rc.1", "0.0.0-dev+0123456789ab", true},
		} {
			existing := releasePin(tc.existing, "a", map[string]string{"linux_amd64": "x"})
			incoming := releasePin(tc.incoming, "b", map[string]string{"linux_amd64": "y"})
			if got := teardown.Update(existing, true, incoming); got.Older != tc.older {
				t.Errorf("%s -> %s: Older = %v, want %v", tc.existing, tc.incoming, got.Older, tc.older)
			}
		}
	})

	t.Run("a file pin never merges, and file labels are not compared", func(t *testing.T) {
		file := filePin("my-build", "same bytes")
		again := filePin("my-build", "same bytes")
		again.PinnedAt = "2026-10-15T08:30:00Z"
		if got := teardown.Update(file, true, again); got.Kind != teardown.Replaced || got.Pin.PinnedAt != again.PinnedAt {
			t.Errorf("file over file: %+v", got)
		}
		release := releasePin("1.2.3", "c", map[string]string{"linux_amd64": "b"})
		if got := teardown.Update(file, true, release); got.Kind != teardown.Replaced || got.Older {
			t.Errorf("release over file: %+v", got)
		}
		if got := teardown.Update(release, true, filePin("0.0.1", "x")); got.Kind != teardown.Replaced || got.Older {
			t.Errorf("file over release: %+v", got)
		}
		if got := teardown.Update(filePin("2.0.0", "x"), true, filePin("1.0.0", "y")); got.Older {
			t.Errorf("file labels were compared: %+v", got)
		}
	})
}

// cloneWith is platforms plus key set to entry, without touching platforms.
func cloneWith(platforms map[string]teardown.Platform, key string, entry teardown.Platform) map[string]teardown.Platform {
	out := map[string]teardown.Platform{}
	for k, v := range platforms {
		out[k] = v
	}
	out[key] = entry
	return out
}
