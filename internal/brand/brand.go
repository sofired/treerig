// Package brand holds the product's names, and the distinction between its two
// kinds matters.
//
// PRESENTATION — what people see: the command name in help text, the log
// prefix, "run `X urls`" hints. This follows whatever the binary was installed
// or symlinked as, so a project that ships the tool as `duke` sees `duke`
// everywhere. Nothing is configured; see [CLIName].
//
// STORAGE IDENTITY — the config filename, the environment variables, the
// treerig root and control-home directories, the Docker label namespace, and
// the marker directory inside `.git`. These are on-disk contracts. They
// deliberately do NOT follow the binary name, because renaming a symlink
// must never orphan state.
package brand

import (
	"path/filepath"
	"regexp"
)

const (
	// Name is the lower-case product name; every storage identifier derives from it.
	Name = "treerig"
	// ConfigFile is the committed per-repository configuration, searched upward from cwd.
	ConfigFile = "treerig.toml"
	// ExampleFile is the annotated copy in this repository, which the missing-config error points at.
	ExampleFile = "treerig.example.toml"
	// SettingsFile is the machine-wide, per-treerig-root settings file (#79):
	// unlike ConfigFile, it is not committed to any repository, and it does
	// not follow TREERIG_CONTROL_HOME or any XDG variable — only the treerig
	// root itself. It holds the router's port and mode and the range of
	// loopback ports routed services are allocated from.
	SettingsFile = "settings.toml"
	// HomeEnv overrides the treerig root: the directory holding settings.toml
	// (#79), router/ and the control home (#80). It is not itself the
	// control home any more — ControlHomeEnv is.
	HomeEnv = "TREERIG_HOME"
	// ControlHomeEnv overrides the control home directly, taking precedence
	// over the one derived from HomeEnv (#80).
	ControlHomeEnv = "TREERIG_CONTROL_HOME"
	// DefaultHomeDir is the treerig root under $HOME when HomeEnv is unset.
	DefaultHomeDir = ".treerig"
	// ControlHomeDirName is the control home's directory name under the
	// treerig root, when ControlHomeEnv is unset.
	ControlHomeDirName = "control-home"
	// RouterDirName is the shared router's directory name under the treerig root.
	RouterDirName = "router"
	// MarkerDir is the directory inside a Git dir that holds the identity markers.
	MarkerDir = "treerig"
	// LabelNamespace is the Docker label namespace this tool owns. Every resource it
	// creates carries <namespace>.managed, <namespace>.repository-uuid and
	// <namespace>.worktree-uuid. Config may override it solely to adopt resources
	// a predecessor tool created.
	LabelNamespace = "dev.treerig"
	// EnvPrefix prefixes the environment variables handed to hooks, e.g. TREERIG_STAGE.
	EnvPrefix = "TREERIG"
)

// Version is what `--version` prints. It stays "0.0.0-dev" for a plain `go
// build`, including in tests, which is what the golden corpus pins. The
// release (GoReleaser, see .goreleaser.yaml) overrides it at build time with
// the tag, via
//
//	-ldflags "-X github.com/sofired/treerig/internal/brand.Version=<version>"
//
// which requires a var: -X cannot set a const.
var Version = "0.0.0-dev"

// LabelNamespacePattern is what a label namespace must look like: a reverse domain.
var LabelNamespacePattern = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)

// ManagedLabel marks a resource as this tool's.
func ManagedLabel(namespace string) string { return namespace + ".managed" }

// RepositoryLabel carries the owning repository's UUID.
func RepositoryLabel(namespace string) string { return namespace + ".repository-uuid" }

// WorktreeLabel carries the owning worktree's UUID.
func WorktreeLabel(namespace string) string { return namespace + ".worktree-uuid" }

// FormatLabel carries the format version of the labels and records treerig
// stamped on a resource ("1"), so a build that does not know a resource's
// format refuses to touch it rather than guess (docs/design/state.md). A
// storage name: it is on every resource treerig creates.
func FormatLabel(namespace string) string { return namespace + ".format" }

// SharedLabel marks a resource as part of a repository's shared-services
// stack (docs/design/routing.md, decision 17): it is the label that, with
// the managed and repository labels, finds the stack's containers, volumes
// and networks, and that no worktree's own resource ever carries. A storage
// name: changing it orphans every running shared service.
func SharedLabel(namespace string) string { return namespace + ".shared" }

// SharedDefinitionLabel carries the definition hash of the shared service a
// container runs, "v1:<sha256>" (decision 16): the only record of what the
// running instance was started from.
func SharedDefinitionLabel(namespace string) string { return namespace + ".shared-definition" }

// SharedDefinedByLabel carries the UUID of the worktree whose definition
// started a shared container, so a warning can say whose it is (decision 16).
func SharedDefinedByLabel(namespace string) string { return namespace + ".shared-defined-by" }

// SharedUseLabel is stamped "true" on a worktree's own container that uses
// the shared service called name (decision 22), so a sweep finds consumers by
// label without loading any worktree's configuration.
func SharedUseLabel(namespace, name string) string { return SharedUseLabelPrefix(namespace) + name }

// SharedUseLabelPrefix is what every SharedUseLabel starts with.
func SharedUseLabelPrefix(namespace string) string { return namespace + ".shared-use." }

// SharedProjectPrefix starts the name of a repository's shared Compose
// project: treerig-shared-<24 hex> (layout.SharedProjectName). A storage name.
const SharedProjectPrefix = Name + "-shared-"

// CLIName is the name this CLI was invoked as: the basename of argv[0], which
// is the symlink an installation is reached through. A binary built as `main`
// (or no argv[0] at all) leaves no meaningful name, so the product name is used.
func CLIName(argv0 string) string {
	if argv0 == "" {
		return Name
	}
	invoked := filepath.Base(argv0)
	if invoked == "." || invoked == string(filepath.Separator) || invoked == "main" {
		return Name
	}
	return invoked
}
