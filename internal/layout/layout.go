// Package layout derives the control home's on-disk paths. Nothing here
// touches the filesystem; these are pure path derivations.
//
//	<controlHome>/
//	  locks/registry                    RegistryLockPath
//	  locks/router                      RouterLockPath
//	  locks/worktrees/<repo>/<wt>       worktree operation lock
//	  locks/worktrees/<repo>/<wt>.dev   development-session lock
//	  router/compose.yaml               the shared router's stack file
//	  router/protocol                   the protocol of the last router that ran here
//	  router/index/index.html           the shared checkout index
//	  repositories/<repo>/worktrees/    registry records, one JSON per worktree
//	  repositories/<repo>/instances/<wt>/   the worktree's home
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
	ControlHome        string
	RouterDir          string
	RouterComposeFile  string
	RouterProtocolFile string
	// RouterDynamicDir is bind-mounted read-only into the router; 0755, 0644 files, never secrets.
	RouterDynamicDir string
	// IndexDir is bind-mounted read-only into the index server; 0755, 0644 files, never secrets.
	IndexDir         string
	IndexFile        string
	RepositoriesDir  string
	LocksDir         string
	RegistryLockPath string
	RouterLockPath   string
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

// ResolveControlHome is the override, else $BERTH_HOME, else ~/.berth, made
// absolute without resolving symlinks.
func ResolveControlHome(getenv Getenv, override string) (string, error) {
	candidate := override
	if candidate == "" {
		if value, ok := getenv(brand.HomeEnv); ok {
			candidate = value
		}
	}
	if candidate == "" {
		if _, set := getenv(brand.HomeEnv); !set {
			home, err := HomeDir(getenv)
			if err != nil {
				return "", err
			}
			candidate = filepath.Join(home, brand.DefaultHomeDir)
		}
		// An empty BERTH_HOME resolves like Node's path.resolve(''): the cwd.
	}
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

// ResolveRouterPort is BERTH_ROUTER_PORT, or 80 when unset or empty.
func ResolveRouterPort(getenv Getenv) (int, error) {
	key := brand.EnvPrefix + "_ROUTER_PORT"
	raw, ok := getenv(key)
	if !ok || raw == "" {
		return 80, nil
	}
	port, err := parseJSInteger(raw)
	if err != nil || port < 1 || port > 65_535 {
		return 0, clierr.Newf("%s must be a port number between 1 and 65535, not \"%s\".", key, raw)
	}
	return port, nil
}

// parseJSInteger accepts what Number(raw) turns into an integer, the way the
// TypeScript build checks it: surrounding whitespace is allowed, and so are
// "80.0", "8e1" and the unsigned 0x/0o/0b forms.
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
	routerDir := filepath.Join(resolved, "router")
	indexDir := filepath.Join(routerDir, "index")
	return Control{
		ControlHome:        resolved,
		RouterDir:          routerDir,
		RouterComposeFile:  filepath.Join(routerDir, "compose.yaml"),
		RouterProtocolFile: filepath.Join(routerDir, "protocol"),
		RouterDynamicDir:   filepath.Join(routerDir, "dynamic"),
		IndexDir:           indexDir,
		IndexFile:          filepath.Join(indexDir, "index.html"),
		RepositoriesDir:    filepath.Join(resolved, "repositories"),
		LocksDir:           locksDir,
		RegistryLockPath:   filepath.Join(locksDir, "registry"),
		RouterLockPath:     filepath.Join(locksDir, "router"),
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

// ComposeProjectName is stable for the (repository, worktree) pair and cannot
// collide across repositories: berth-<first 24 hex of sha256("repo:wt")>.
func ComposeProjectName(repositoryID, worktreeID string) string {
	sum := sha256.Sum256([]byte(repositoryID + ":" + worktreeID))
	return brand.Name + "-" + hex.EncodeToString(sum[:])[:24]
}

// WorktreeHost is the routed hostname, <slug>.<domain>.
func WorktreeHost(slug, domain string) string { return slug + "." + domain }
