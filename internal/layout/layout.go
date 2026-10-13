// Package layout derives the berth root's and control home's on-disk paths.
// Nothing here touches the filesystem; these are pure path derivations.
//
//	<home>/                              BERTH_HOME, the berth root (#80)
//	  settings.toml                     SettingsPath, machine-wide defaults (#79)
//	  router/                            RouterState, the shared router's own layout (stack file, route files, index page, state.json)
//	  control-home/                      the control home, unless BERTH_CONTROL_HOME overrides it
//	    locks/registry                    RegistryLockPath
//	    locks/worktrees/<repo>/<wt>       worktree operation lock
//	    locks/worktrees/<repo>/<wt>.dev   development-session lock
//	    repositories/<repo>/worktrees/    registry records, one JSON per worktree
//	    repositories/<repo>/instances/<wt>/   the worktree's home
//	    bin/<sha256>                      BinDir: `worktree remove`'s own content-addressed copies (#7)
//
// The shared router's own stack file, protocol record, dynamic and index
// directories, and lock live under the berth root rather than inside the
// control home (#12, #13, #80), so every control home on this machine that
// shares a berth root finds the same router state instead of taking turns
// recreating it. The router container itself is still one per Docker
// daemon regardless of berth root (RouterLayout), so a different berth root
// does not get its own isolated router — only its own copy of this state.
package layout

import (
	"crypto/sha256"
	"encoding/hex"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
)

// Control is the machine-wide part of the layout.
type Control struct {
	ControlHome      string
	RepositoriesDir  string
	LocksDir         string
	RegistryLockPath string
	// BinDir is the content-addressed store `worktree remove` installs a
	// copy of itself into before running, so deleting a checkout can never
	// delete the program doing the removal (#7) — the copy is exactly the
	// bytes that were invoked, nothing more. Entries are named by the
	// sha256 of their bytes; see internal/binstore.
	BinDir string
}

// Repository adds one repository's directories.
type Repository struct {
	Control
	RepositoryDir    string
	RegistryDir      string
	InstancesDir     string
	WorktreeLocksDir string
}

// Worktree adds one worktree's paths.
type Worktree struct {
	Repository
	RecordPath string
	Home       string
	// QuarantinePath is renamed-to during removal, so a crash mid-delete leaves a recognizable artifact.
	QuarantinePath      string
	LockPath            string
	DevelopmentLockPath string
}

// Getenv reads one environment variable; os.LookupEnv in production.
type Getenv func(key string) (string, bool)

// ResolveHome is the berth root: the override, else $BERTH_HOME, else
// ~/.berth, made absolute without resolving symlinks. It holds settings.toml
// (#79, SettingsPath), router/ (RouterState) and, unless BERTH_CONTROL_HOME
// overrides it, the control home (ResolveControlHome) (#80).
func ResolveHome(getenv Getenv, override string) (string, error) {
	candidate := override
	value, set := getenv(brand.HomeEnv)
	if candidate == "" && set {
		candidate = value
	}
	if candidate == "" && !set {
		home, err := HomeDir(getenv)
		if err != nil {
			return "", err
		}
		candidate = filepath.Join(home, brand.DefaultHomeDir)
	}
	// An empty BERTH_HOME resolves like Node's path.resolve(''): the cwd.
	return Resolve(candidate)
}

// ResolveControlHome is the control home: the override, else
// $BERTH_CONTROL_HOME, else <berth root>/control-home, made absolute without
// resolving symlinks. The berth root itself comes from ResolveHome, so an
// unset BERTH_CONTROL_HOME still honours BERTH_HOME (#80). Resolves the
// berth root itself for that fallback; a caller that already has one
// (typically already checked by files.AssertSafeHome) wants ControlHomeFrom
// instead, so the two can never resolve it to two different things.
func ResolveControlHome(getenv Getenv, override string) (string, error) {
	home, err := ResolveHome(getenv, "")
	if err != nil {
		return "", err
	}
	return ControlHomeFrom(home, getenv, override)
}

// ControlHomeFrom is ResolveControlHome given the berth root already
// resolved: the override, else $BERTH_CONTROL_HOME, else
// <home>/control-home, made absolute without resolving symlinks.
func ControlHomeFrom(home string, getenv Getenv, override string) (string, error) {
	candidate := override
	value, set := getenv(brand.ControlHomeEnv)
	if candidate == "" && set {
		candidate = value
	}
	if candidate == "" && !set {
		candidate = filepath.Join(home, brand.ControlHomeDirName)
	}
	// An empty BERTH_CONTROL_HOME resolves like Node's path.resolve(''): the
	// cwd, the same convention ResolveHome applies to an empty BERTH_HOME.
	return Resolve(candidate)
}

// Resolve is Node's path.resolve for one argument: an absolute path is
// cleaned, a relative one is joined to the physical working directory. Go's
// os.Getwd prefers $PWD, which may be a symlinked spelling of the same place.
func Resolve(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	cwd, err := PhysicalWd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, path), nil
}

// PhysicalWd is the working directory as the kernel reports it, the way
// Node's process.cwd() does.
func PhysicalWd() (string, error) { return syscall.Getwd() }

// HomeDir is $HOME, falling back to the password database the way Node's
// os.homedir() does when HOME is unset.
func HomeDir(getenv Getenv) (string, error) {
	if home, ok := getenv("HOME"); ok && home != "" {
		return home, nil
	}
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	return current.HomeDir, nil
}

// DefaultRouterPort is the router's port when neither RouterPortEnv nor
// settings.toml (#79) sets one.
const DefaultRouterPort = 80

// RouterPortEnv overrides the router's port, ahead of everything else:
// settings.toml's [router] port (#79), then this package's own default of 80
// (EnvRouterPort). internal/settings.ResolveRouterPort is the precedence
// the rest of the build actually uses; this package stays a pure
// path-derivation one, so it resolves the environment variable alone,
// touching no filesystem.
const RouterPortEnv = brand.EnvPrefix + "_ROUTER_PORT"

// EnvRouterPort is RouterPortEnv, or DefaultRouterPort when unset or empty.
// It reads the environment alone; internal/settings.ResolveRouterPort is
// the full precedence, with settings.toml between the two.
func EnvRouterPort(getenv Getenv) (int, error) {
	raw, ok := getenv(RouterPortEnv)
	if !ok || raw == "" {
		return DefaultRouterPort, nil
	}
	port, err := parseJSInteger(raw)
	if err != nil || port < 1 || port > 65_535 {
		return 0, clierr.Newf("%s must be a port number between 1 and 65535, not \"%s\".", RouterPortEnv, raw)
	}
	return port, nil
}

// parseJSInteger accepts what Number(raw) turns into an integer: surrounding
// whitespace is allowed, and so are "80.0", "8e1" and the unsigned 0x/0o/0b
// forms.
func parseJSInteger(raw string) (int, error) {
	trimmed := trimJSWhitespace(raw)
	if strings.Contains(trimmed, "_") || trimmed == "" {
		return 0, strconv.ErrSyntax
	}
	if len(trimmed) > 2 && trimmed[0] == '0' {
		base := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[trimmed[1]]
		if base != 0 {
			value, err := strconv.ParseUint(trimmed[2:], base, 32)
			return int(value), err
		}
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "inf") || strings.Contains(lower, "nan") || strings.HasPrefix(lower, "0x") {
		return 0, strconv.ErrSyntax
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || value != float64(int(value)) {
		return 0, strconv.ErrSyntax
	}
	return int(value), nil
}

// trimJSWhitespace trims what JavaScript's Number() treats as whitespace
// around a numeric string, the way String.prototype.trim() would.
func trimJSWhitespace(value string) string {
	start, end := 0, len(value)
	isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f' }
	for start < end && isSpace(value[start]) {
		start++
	}
	for end > start && isSpace(value[end-1]) {
		end--
	}
	return value[start:end]
}

// ControlLayout derives the machine-wide paths.
func ControlLayout(controlHome string) Control {
	resolved, err := Resolve(controlHome)
	if err != nil {
		resolved = filepath.Clean(controlHome)
	}
	locksDir := filepath.Join(resolved, "locks")
	return Control{
		ControlHome:      resolved,
		RepositoriesDir:  filepath.Join(resolved, "repositories"),
		LocksDir:         locksDir,
		RegistryLockPath: filepath.Join(locksDir, "registry"),
		BinDir:           filepath.Join(resolved, "bin"),
	}
}

// RepositoryLayout derives one repository's paths.
func RepositoryLayout(controlHome, repositoryID string) Repository {
	control := ControlLayout(controlHome)
	repositoryDir := filepath.Join(control.RepositoriesDir, repositoryID)
	return Repository{
		Control:          control,
		RepositoryDir:    repositoryDir,
		RegistryDir:      filepath.Join(repositoryDir, "worktrees"),
		InstancesDir:     filepath.Join(repositoryDir, "instances"),
		WorktreeLocksDir: filepath.Join(control.LocksDir, "worktrees", repositoryID),
	}
}

// WorktreeLayout derives one worktree's paths.
func WorktreeLayout(controlHome, repositoryID, worktreeID string) Worktree {
	repository := RepositoryLayout(controlHome, repositoryID)
	home := filepath.Join(repository.InstancesDir, worktreeID)
	return Worktree{
		Repository:          repository,
		RecordPath:          filepath.Join(repository.RegistryDir, worktreeID+".json"),
		Home:                home,
		QuarantinePath:      filepath.Join(repository.InstancesDir, ".deleting-"+worktreeID),
		LockPath:            filepath.Join(repository.WorktreeLocksDir, worktreeID),
		DevelopmentLockPath: filepath.Join(repository.WorktreeLocksDir, worktreeID+".dev"),
	}
}

// RouterLayout is the shared router's on-disk layout, outside the control
// home (#12, #13, #80): the stack file, the two directories the router
// bind-mounts, and the router lock's own target file. This layout is per
// berth root, so every control home under the same berth root reads and
// writes the same one instead of taking turns recreating it — but the
// router container itself is not: its Compose project name is fixed
// (router.ResolveProjectName), so it is one per Docker daemon regardless of
// berth root. A berth root with its own, different RouterLayout does not
// get its own router; it makes `up` or `router up` from that root recreate
// the one container against its own state instead, still on the same
// daemon (#80's own AGENTS.md note).
type RouterLayout struct {
	StateDir    string
	ComposeFile string
	// DynamicDir is bind-mounted read-only into the router; 0755, 0644 files, never secrets.
	DynamicDir string
	// IndexDir is bind-mounted read-only into the index server; 0755, 0644 files, never secrets.
	IndexDir  string
	IndexFile string
	LockPath  string
	// StateFile holds the router's own facts that no registration can: the
	// checkout index's loopback port and, in host mode, the ping port.
	StateFile string
}

// RouterStateDir is the directory the shared router's state and lock live
// under: `<berth root>/router` (#80). Before #80 this lived outside every
// control home, keyed off the OS's own per-account state directory
// (`$XDG_STATE_HOME` or `$HOME/.local/state` on Linux, `Library/Application
// Support` on macOS); it is simpler now that it is resolved the same way
// BERTH_HOME resolves a control home. A different BERTH_HOME still reads
// and writes a different RouterStateDir — but the router container itself
// stays one per Docker daemon (see RouterLayout), so a scratch BERTH_HOME
// used by hand does not get its own isolated router the way it gets its
// own control home. Resolves the berth root itself; a caller that already
// has one wants RouterStateDirFrom instead, so the two can never resolve it
// to two different things.
func RouterStateDir(getenv Getenv) (string, error) {
	home, err := ResolveHome(getenv, "")
	if err != nil {
		return "", err
	}
	return RouterStateDirFrom(home), nil
}

// RouterStateDirFrom is RouterStateDir given the berth root already resolved.
func RouterStateDirFrom(home string) string { return filepath.Join(home, brand.RouterDirName) }

// RouterState derives the shared router's paths. Resolves the berth root
// itself; a caller that already has one (typically already checked by
// files.AssertSafeHome) wants RouterStateFrom instead.
func RouterState(getenv Getenv) (RouterLayout, error) {
	home, err := ResolveHome(getenv, "")
	if err != nil {
		return RouterLayout{}, err
	}
	return RouterStateFrom(home), nil
}

// RouterStateFrom is RouterState given the berth root already resolved.
func RouterStateFrom(home string) RouterLayout {
	dir := RouterStateDirFrom(home)
	indexDir := filepath.Join(dir, "index")
	return RouterLayout{
		StateDir:    dir,
		ComposeFile: filepath.Join(dir, "compose.yaml"),
		DynamicDir:  filepath.Join(dir, "dynamic"),
		IndexDir:    indexDir,
		IndexFile:   filepath.Join(indexDir, "index.html"),
		LockPath:    filepath.Join(dir, "lock"),
		StateFile:   filepath.Join(dir, "state.json"),
	}
}

// SettingsPath is the machine-wide settings file's path (#79):
// `<berth root>/settings.toml`. It does not follow BERTH_CONTROL_HOME or any
// XDG variable, only the berth root itself — unlike RouterState, which
// shares that root for a different reason (#80), this file simply has
// nowhere else to live: it configures values (the router's port) that are
// resolved before any control home is. Resolves the berth root itself; a
// caller that already has one (typically already checked by checkedHome)
// wants SettingsPathFrom instead.
func SettingsPath(getenv Getenv) (string, error) {
	home, err := ResolveHome(getenv, "")
	if err != nil {
		return "", err
	}
	return SettingsPathFrom(home), nil
}

// SettingsPathFrom is SettingsPath given the berth root already resolved.
func SettingsPathFrom(home string) string {
	return filepath.Join(home, brand.SettingsFile)
}

// ComposeProjectName is stable for the (repository, worktree) pair and cannot
// collide across repositories: berth-<first 24 hex of sha256("repo:wt")>.
func ComposeProjectName(repositoryID, worktreeID string) string {
	sum := sha256.Sum256([]byte(repositoryID + ":" + worktreeID))
	return brand.Name + "-" + hex.EncodeToString(sum[:])[:24]
}

// SharedProjectName is the Compose project of a repository's shared
// services (docs/design/routing.md, decision 15): berth-shared-<first 24 hex
// of sha256("shared:" + repositoryID)>. Derived from the repository ID alone,
// so it is stable and uses a separate prefix from a worktree's
// ComposeProjectName. `shared down` can name it from any checkout without a
// berth.toml. A storage name.
func SharedProjectName(repositoryID string) string {
	sum := sha256.Sum256([]byte("shared:" + repositoryID))
	return brand.SharedProjectPrefix + hex.EncodeToString(sum[:])[:24]
}

// Shared is one repository's shared-services paths.
type Shared struct {
	Repository
	// Dir holds the repository's shared-service records, one JSON per
	// service, and the shared project's override file.
	Dir string
	// LockDir holds every repository's shared lock; LockPath is this one's.
	LockDir  string
	LockPath string
	// OverridePath is the Compose override berth writes for the shared
	// project: ownership labels, the definition label and the pinned ports.
	OverridePath string
}

// SharedLayout derives one repository's shared-services paths:
//
//	repositories/<repo>/shared/<name>.json   a service's pinned ports
//	repositories/<repo>/shared/berth.override.yaml
//	locks/shared/<repo>                      the shared lock (decision 18)
func SharedLayout(controlHome, repositoryID string) Shared {
	repository := RepositoryLayout(controlHome, repositoryID)
	dir := filepath.Join(repository.RepositoryDir, "shared")
	lockDir := filepath.Join(repository.LocksDir, "shared")
	return Shared{
		Repository:   repository,
		Dir:          dir,
		LockDir:      lockDir,
		LockPath:     filepath.Join(lockDir, repositoryID),
		OverridePath: filepath.Join(dir, brand.Name+".override.yaml"),
	}
}

// RecordPath is where the shared service called name keeps its pinned
// ports.
func (s Shared) RecordPath(name string) string {
	return filepath.Join(s.Dir, name+".json")
}

// WorktreeHost is the routed hostname, <slug>.<domain>.
func WorktreeHost(slug, domain string) string { return slug + "." + domain }
