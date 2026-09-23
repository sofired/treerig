package registry

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/sofired/berth/internal/brand"
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

// rawRecord is the on-disk shape, decoded strictly: an unknown key is a
// shape error, exactly as the TypeScript build's `.strict()` schema treats it.
type rawRecord struct {
	Version        *float64 `json:"version"`
	RepositoryID   *string  `json:"repositoryId"`
	WorktreeID     *string  `json:"worktreeId"`
	Slug           *string  `json:"slug"`
	Domain         *string  `json:"domain"`
	LabelNamespace *string  `json:"labelNamespace"`
	Initialization *string  `json:"initialization"`
}

type parsedRecord struct {
	RepositoryID   string
	WorktreeID     string
	Slug           string
	Domain         string
	LabelNamespace *string
	Initialization WorktreeInitialization
}

// parseRecordJSON validates data against the record schema: version 1,
// repositoryId and worktreeId are UUIDs, slug matches the slug pattern,
// domain is non-empty, labelNamespace (if present) matches the label
// namespace pattern, initialization is "pending" or "complete", and no
// other key is present.
func parseRecordJSON(data []byte) (parsedRecord, error) {
	if !json.Valid(data) {
		return parsedRecord{}, errInvalidJSON
	}
	var raw rawRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return parsedRecord{}, errUnrecognizedShape
	}
	ok := raw.Version != nil && *raw.Version == 1 &&
		raw.RepositoryID != nil && uuidPattern.MatchString(*raw.RepositoryID) &&
		raw.WorktreeID != nil && uuidPattern.MatchString(*raw.WorktreeID) &&
		raw.Slug != nil && slugPattern.MatchString(*raw.Slug) &&
		raw.Domain != nil && *raw.Domain != "" &&
		raw.Initialization != nil && (*raw.Initialization == string(Pending) || *raw.Initialization == string(Complete)) &&
		(raw.LabelNamespace == nil || brand.LabelNamespacePattern.MatchString(*raw.LabelNamespace))
	if !ok {
		return parsedRecord{}, errUnrecognizedShape
	}
	return parsedRecord{
		RepositoryID:   *raw.RepositoryID,
		WorktreeID:     *raw.WorktreeID,
		Slug:           *raw.Slug,
		Domain:         *raw.Domain,
		LabelNamespace: raw.LabelNamespace,
		Initialization: WorktreeInitialization(*raw.Initialization),
	}, nil
}
