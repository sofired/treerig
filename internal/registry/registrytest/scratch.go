package registrytest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
)

// WriteScratchSettings writes home's settings.toml with the test port range
// (PortRange) and any further TOML in extra, creating home if needed. A
// scratch berth root that runs `up` needs it: without it the run would take
// ports from the default range, which a developer's own worktrees use.
func WriteScratchSettings(t testing.TB, home, extra string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := "[ports]\nrange = \"" + PortRange.String() + "\"\n" + extra
	if err := os.WriteFile(layout.SettingsPathFrom(home), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ClaimControlHome marks dir as a control home berth created, so a scratch
// root whose berth root and control home are the same directory may hold a
// settings.toml beside its records: berth refuses to adopt a non-empty
// directory it did not create.
func ClaimControlHome(t testing.TB, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, files.ControlHomeMarker), []byte(files.ControlHomeFormat), 0o600); err != nil {
		t.Fatal(err)
	}
}
