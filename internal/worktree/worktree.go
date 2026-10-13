// Package worktree holds what the control plane and the backend both say
// about a worktree: its reference, its services' states, what it owns, the
// environment hooks and Compose receive, and the URLs its declared services
// route to. It is a leaf, so the backend and the hooks runner can share it
// without importing each other.
package worktree

import (
	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/layout"
)

// Ref identifies a worktree to a backend.
type Ref struct {
	RepositoryID string
	WorktreeID   string
	// Slug is human-readable and unique within the project domain; it routes as <slug>.<domain>.
	Slug string
}

// ServiceState is running, starting, stopped, absent (declared but no
// container, the state after `down`), or unknown.
type ServiceState string

const (
	Running  ServiceState = "running"
	Starting ServiceState = "starting"
	Stopped  ServiceState = "stopped"
	Absent   ServiceState = "absent"
	Unknown  ServiceState = "unknown"
)

// ServiceStatus is one declared service's state.
type ServiceStatus struct {
	Service string
	State   ServiceState
	Detail  string
}

// OwnedResource is something the backend created for a worktree and can
// prove it owns. Reaping must refuse any resource whose ownership cannot be verified.
type OwnedResource struct {
	Kind    string
	ID      string
	Service string
}

// EnvironmentSource is what it takes to describe a worktree to the outside.
type EnvironmentSource struct {
	ProjectName string
	Domain      string
	ControlHome string
	// ConfigFile is the absolute path of the loaded config file.
	ConfigFile string
	// ProjectRoot is the directory containing it.
	ProjectRoot string
}

// ComposeProjectKey is the Environment key (BERTH_COMPOSE_PROJECT) that
// carries the worktree's Compose project name; the hook runner reads it back
// to set Compose's own COMPOSE_PROJECT_NAME from the same value.
const ComposeProjectKey = "COMPOSE_PROJECT"

// Environment is what a worktree looks like from outside the tool. Hooks
// receive it plus a stage; the Compose file receives exactly this, so
// ${BERTH_WORKTREE_HOME} means the same thing in a bind mount as in a seed
// script. One builder, so hooks and the Compose file never disagree.
//
// The keys are in a fixed order: PROJECT, REPOSITORY_ID, WORKTREE_ID, SLUG,
// HOST, WORKTREE_HOME, CONFIG, ROOT, COMPOSE_PROJECT. COMPOSE_PROJECT is the
// worktree's Compose project name, derived here once; the hook runner also
// hands it to Compose's own variable of that name.
func Environment(source EnvironmentSource, ref Ref) [][2]string {
	return [][2]string{
		{"PROJECT", source.ProjectName},
		{"REPOSITORY_ID", ref.RepositoryID},
		{"WORKTREE_ID", ref.WorktreeID},
		{"SLUG", ref.Slug},
		{"HOST", layout.WorktreeHost(ref.Slug, source.Domain)},
		// Deliberately not HOME: BERTH_HOME (the berth root) and
		// BERTH_CONTROL_HOME (the control-home override, #80) are the
		// user's own, and must pass through to hooks untouched.
		{"WORKTREE_HOME", layout.WorktreeLayout(source.ControlHome, ref.RepositoryID, ref.WorktreeID).Home},
		{"CONFIG", source.ConfigFile},
		{"ROOT", source.ProjectRoot},
		{ComposeProjectKey, layout.ComposeProjectName(ref.RepositoryID, ref.WorktreeID)},
	}
}

// Variables prefixes each key with BERTH_, the storage-contract name rather
// than the invoked CLI name, so hook scripts keep working however the binary
// is called.
func Variables(environment [][2]string) []string {
	variables := make([]string, len(environment))
	for i, pair := range environment {
		variables[i] = brand.EnvPrefix + "_" + pair[0] + "=" + pair[1]
	}
	return variables
}
