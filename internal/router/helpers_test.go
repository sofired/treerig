package router_test

import (
	"os"
	"sync"
	"testing"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/ports"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/router"
)

// Identifiers that satisfy the registry's own UUID check, unlike the lenient
// shape route files need.
const (
	regRepoA = "a1b2c3d4-1111-4222-8333-444455556666"
	regWtA   = "b1b2c3d4-1111-4222-8333-444455556666"
)

// registrytestRange is the port range tests allocate from.
func registrytestRange() layout.PortRange { return registrytest.PortRange }

// registryRangeContains reports whether port is in the test range.
func registryRangeContains(port int) bool { return registrytest.PortRange.Contains(port) }

// tinyRange is a range of three ports, so a test can exhaust it.
func tinyRange() layout.PortRange { return layout.PortRange{Low: 30900, High: 30902} }

// startOf is the first port ports.Pick tries for keys in the test range.
func startOf(keys ...string) int {
	return ports.Start(registrytest.PortRange.Size(), keys...)
}

// writeRegistration writes a registry record of worktreeID holding ports
// into controlHome, as `up` would have left it.
func writeRegistration(t *testing.T, controlHome, repositoryID, worktreeID string, held map[string]int) {
	t.Helper()
	if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: repositoryID, WorktreeID: worktreeID,
		Slug: "feature", Domain: "acme.localhost", Initialization: "complete", Ports: held,
	}); err != nil {
		t.Fatal(err)
	}
}

// corruptRegistration overwrites a registration with something no record
// reader accepts.
func corruptRegistration(t *testing.T, controlHome, repositoryID, worktreeID string) {
	t.Helper()
	path := layout.WorktreeLayout(controlHome, repositoryID, worktreeID).RecordPath
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeRegistrations is a router.Registrations that answers each lookup from
// a script, repeating the last answer, so a test can flip what a second read
// finds.
type fakeRegistrations struct {
	mu      sync.Mutex
	answers []router.Registration
	calls   int
	err     error
}

// newFakeRegistrations answers answers in order, then the last one forever.
func newFakeRegistrations(answers ...router.Registration) *fakeRegistrations {
	return &fakeRegistrations{answers: answers}
}

// Lookup is router.Registrations' Lookup.
func (f *fakeRegistrations) Lookup(string, string) (router.Registration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return router.Registration{}, f.err
	}
	index := min(f.calls, len(f.answers)-1)
	f.calls++
	return f.answers[index], nil
}

// lookups is how many successful lookups were made.
func (f *fakeRegistrations) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
