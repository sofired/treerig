package registrytest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/registry"
)

// RecordJSON is the on-disk shape of a registry record, exported for fixture
// construction: a raw write straight to disk, for records that test does
// not create via the (locked) write path — the same trick this package's
// WriteRecord plays.
type RecordJSON struct {
	Version        int     `json:"version"`
	RepositoryID   string  `json:"repositoryId"`
	WorktreeID     string  `json:"worktreeId"`
	Slug           string  `json:"slug"`
	Domain         string  `json:"domain"`
	LabelNamespace *string `json:"labelNamespace,omitempty"`
	Initialization string  `json:"initialization"`
	// Ports is the optional service → loopback port map (#119).
	Ports map[string]int `json:"ports,omitempty"`
	// HostRoutes are the optional routes to host processes (#6).
	HostRoutes []HostRouteJSON `json:"hostRoutes,omitempty"`
}

// HostRouteJSON is the on-disk shape of one host route, for fixtures.
type HostRouteJSON struct {
	Name      string  `json:"name"`
	Port      int     `json:"port"`
	Subdomain string  `json:"subdomain,omitempty"`
	Path      *string `json:"path,omitempty"`
}

// PortRange is the loopback port range tests allocate from. It is not the
// default range, so a test never takes a port from a developer's own
// worktrees, and it sits below the OS's ephemeral range.
var PortRange = layout.PortRange{Low: 30000, High: 30999}

// FreeProbe is a ports.Probe that reports every port free except the
// ones in Busy, so a test picks which ports are taken without binding any.
type FreeProbe struct {
	Busy map[int]bool
}

// Free is ports.Probe's Free.
func (p FreeProbe) Free(_ context.Context, port int) bool { return !p.Busy[port] }

// WriteRecord writes a fixture registry record directly to disk, bypassing
// the registry lock. This build has no Ensure (only `up` creates records), so
// tests that need a registration on disk — a record "from another
// repository", say — build one by hand here.
func WriteRecord(controlHome string, rec RecordJSON) (string, error) {
	file := filepath.Join(registry.RegistryDirectory(controlHome, rec.RepositoryID), rec.WorktreeID+".json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	// Written atomically, to a temporary name that does not end in .json: a
	// registry scan running at the same time (a test with workers) must never
	// read a half-written record, as it never can of a production write.
	return file, files.AtomicWrite(file, data, 0o600)
}

// Register creates worktree's identity markers (via IdentityService.Ensure,
// which needs no lock) and then writes a fixture record for it, as if `up`
// had registered it. Returns the identity so the caller can assert on its
// UUIDs.
func Register(
	ctx context.Context, controlHome string, identities *registry.IdentityService,
	worktree, domain, slug string, labelNamespace *string, initialization string,
) (*registry.WorktreeIdentity, error) {
	identity, err := identities.Ensure(ctx, worktree)
	if err != nil {
		return nil, err
	}
	_, err = WriteRecord(controlHome, RecordJSON{
		Version: 1, RepositoryID: identity.RepositoryID, WorktreeID: identity.WorktreeID,
		Slug: slug, Domain: domain, LabelNamespace: labelNamespace, Initialization: initialization,
	})
	if err != nil {
		return nil, err
	}
	return identity, nil
}
