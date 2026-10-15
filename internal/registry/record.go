package registry

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/stateformat"
)

// WorktreeInitialization is where a registered worktree's environment stands.
type WorktreeInitialization string

const (
	// Pending means registered, but never brought up (or a previous removal
	// left it that way deliberately, so the next `up` seeds again).
	Pending WorktreeInitialization = "pending"
	// Complete means at least one `up` finished.
	Complete WorktreeInitialization = "complete"
)

// RegisteredWorktree is one worktree's registry record.
type RegisteredWorktree struct {
	RepositoryID string
	WorktreeID   string
	// Slug is unique within Domain; the worktree routes as <slug>.<domain>.
	Slug   string
	Domain string
	// LabelNamespace is the Docker label namespace this worktree's resources
	// carry. Absent in records written before it was tracked.
	LabelNamespace *string
	// Ports is the loopback port allocated to each routed service, by
	// service name (#119). Absent in records written before ports existed,
	// and for a worktree that routes nothing. Read-only to callers: the
	// only writers are Store.SyncPorts and Store.ReleasePorts.
	Ports map[string]int
	// HostRoutes are the routes to processes running on the host (#6), by
	// name. Absent for a worktree that has none. Read-only to callers: the
	// only writer is Store.UpdateHostRoutes. The route files are derived
	// from them (docs/design/routing.md, decisions 9 and 11).
	HostRoutes []HostRoute
}

// WorktreeRegistration is a registered worktree together with where it
// currently lives.
type WorktreeRegistration struct {
	RegisteredWorktree
	Root   string
	Branch string
}

// EnsuredWorktree is what Store.Ensure returns: the record, plus where it
// was before if it moved.
type EnsuredWorktree struct {
	RegisteredWorktree
	PreviousHost *string
	// PreviousLabelNamespace is the namespace the record carried before this
	// call rewrote it.
	PreviousLabelNamespace *string
}

// EnsuredRegistration is what Registry.Ensure returns: a registration
// together with where it was before if it moved.
type EnsuredRegistration struct {
	WorktreeRegistration
	PreviousHost           *string
	PreviousLabelNamespace *string
}

// RegistryProblem is a registry file that could not be read as a record.
// Reported, never thrown.
type RegistryProblem struct {
	File   string
	Reason string
	// Newer is true when the file was written by a newer treerig than this
	// build (Reason then says to upgrade): it is intact, only unreadable
	// here, so a caller must not advise moving it aside.
	Newer bool
}

// HostnameConflicts maps a hostname to the two or more worktrees claiming it.
type HostnameConflicts map[string][]RegisteredWorktree

// RegistryScan is every registry on the machine, read once.
type RegistryScan struct {
	Records   []RegisteredWorktree
	Problems  []RegistryProblem
	Conflicts HostnameConflicts
}

// storedRecord is one record as the store reads it: the registration plus
// the initialization state that lives beside it in the same file.
type storedRecord struct {
	Initialization WorktreeInitialization
	Registration   RegisteredWorktree
}

// errInvalidJSON and errUnrecognizedShape are sentinels parseRecordJSON
// returns; callers turn them into a CliError naming the file.
var (
	errInvalidJSON       = errors.New("not valid JSON")
	errUnrecognizedShape = errors.New("unrecognized version or shape")
)

// newerFormat is what the record parsers return for a record whose version is
// above the one this build writes. found is the version as the record
// declares it. Callers turn it into stateformat.Newer, naming the file.
type newerFormat struct{ found string }

// Error satisfies error; callers never show this text, they show
// stateformat.Newer's.
func (e *newerFormat) Error() string { return "format " + e.found + " is newer than this build reads" }

// rawRecord is the on-disk shape, decoded strictly: an unknown key is a
// shape error.
type rawRecord struct {
	Version        *float64 `json:"version"`
	RepositoryID   *string  `json:"repositoryId"`
	WorktreeID     *string  `json:"worktreeId"`
	Slug           *string  `json:"slug"`
	Domain         *string  `json:"domain"`
	LabelNamespace *string  `json:"labelNamespace"`
	Initialization *string  `json:"initialization"`
	// Ports is decoded as numbers so a fractional or non-numeric port is a shape error.
	Ports map[string]int `json:"ports"`
	// HostRoutes are decoded strictly too: an unknown key in one is a shape error.
	HostRoutes []rawHostRoute `json:"hostRoutes"`
}

// parsedRecord is a validated record: the registration it holds, the one
// conversion every read path uses, and the initialization state that lives
// beside it in the same file.
type parsedRecord struct {
	RegisteredWorktree
	Initialization WorktreeInitialization
}

// parseRecordJSON validates data against the record schema: version 1 (a
// version above it is a *newerFormat, judged before the rest of the shape,
// which a newer build may have changed),
// repositoryId and worktreeId are UUIDs, slug matches the slug pattern,
// domain is non-empty, labelNamespace (if present) matches the label
// namespace pattern, initialization is "pending" or "complete", ports (if
// present) maps service names to ports between 1 and 65535, hostRoutes (if
// present) lists routes with distinct names (parseHostRoutes), and no other
// key is present.
func parseRecordJSON(data []byte) (parsedRecord, error) {
	if !json.Valid(data) {
		return parsedRecord{}, errInvalidJSON
	}
	if verdict, found := stateformat.JSON(data, stateformat.Registration); verdict == stateformat.FromNewer {
		return parsedRecord{}, &newerFormat{found: found}
	}
	var raw rawRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return parsedRecord{}, errUnrecognizedShape
	}
	ok := raw.Version != nil && *raw.Version == stateformat.Registration &&
		raw.RepositoryID != nil && uuidPattern.MatchString(*raw.RepositoryID) &&
		raw.WorktreeID != nil && uuidPattern.MatchString(*raw.WorktreeID) &&
		raw.Slug != nil && slugPattern.MatchString(*raw.Slug) &&
		raw.Domain != nil && *raw.Domain != "" &&
		raw.Initialization != nil && (*raw.Initialization == string(Pending) || *raw.Initialization == string(Complete)) &&
		(raw.LabelNamespace == nil || brand.LabelNamespacePattern.MatchString(*raw.LabelNamespace)) &&
		validPorts(raw.Ports)
	hostRoutes, hostRoutesOK := parseHostRoutes(raw.HostRoutes)
	if !ok || !hostRoutesOK {
		return parsedRecord{}, errUnrecognizedShape
	}
	return parsedRecord{
		RegisteredWorktree: RegisteredWorktree{
			RepositoryID:   *raw.RepositoryID,
			WorktreeID:     *raw.WorktreeID,
			Slug:           *raw.Slug,
			Domain:         *raw.Domain,
			LabelNamespace: raw.LabelNamespace,
			Ports:          nonEmptyPorts(raw.Ports),
			HostRoutes:     hostRoutes,
		},
		Initialization: WorktreeInitialization(*raw.Initialization),
	}, nil
}

// validPorts reports whether every key of ports is a service name and every
// value a TCP port. An absent map is valid.
func validPorts(ports map[string]int) bool {
	for service, port := range ports {
		if !slugPattern.MatchString(service) || port < 1 || port > 65_535 {
			return false
		}
	}
	return true
}

// nonEmptyPorts is ports, or nil when it holds nothing, so a record with
// "ports": {} and one without the key read the same.
func nonEmptyPorts(ports map[string]int) map[string]int {
	if len(ports) == 0 {
		return nil
	}
	return ports
}
