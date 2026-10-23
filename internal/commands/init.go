package commands

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/run"
)

// composeFileNames are the file names Docker Compose itself looks for, in
// its own order of precedence.
var composeFileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// Init is `init`: it writes a starter treerig.toml at the root of the Git
// checkout it runs in, and refuses to replace one that is already there,
// including when another `init` creates it between the check and the write.
// When a Compose file sits at that root, the starter names it in [compose];
// otherwise it names compose.yaml and a warning says it still has to be
// created. The services in the Compose file are not read.
//
// It reads no treerig.toml, so --config has no meaning for it and is refused.
// Its notices go to stderr; it prints nothing on stdout, since there is no
// report to pipe.
func Init(ctx context.Context, env *Env) error {
	// The command line never gets here with a path: init declares no --config
	// and flagError explains why. This is the same refusal for a direct caller.
	if env.ConfigPath != "" {
		return clierr.Newf("'%s init' writes %s at the root of the Git repository you run it in; it does not take --config.",
			env.CommandName, brand.ConfigFile)
	}
	root, err := gitToplevel(ctx, env)
	if err != nil {
		return err
	}
	destination := filepath.Join(root, brand.ConfigFile)

	composeFile, found, err := findComposeFile(root)
	if err != nil {
		return err
	}
	if !found {
		composeFile = composeFileNames[0]
	}

	contents := config.Starter(config.StarterName(filepath.Base(root)), composeFile)
	if err := files.WriteNew(destination, contents, 0o644); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return clierr.Newf("%s already exists; not overwriting it. Edit it, or remove it first to start over.", destination)
		}
		// files.WriteNew's error already names the destination.
		return err
	}

	env.Logger.Info(fmt.Sprintf("Wrote %s.", destination))
	if !found {
		env.Logger.Warn(fmt.Sprintf("No Compose file at %s; [compose] file is %s, which you still need to create.", root, composeFile))
	}
	env.Logger.Info(fmt.Sprintf("Edit %s, then run '%s config validate'.", brand.ConfigFile, env.CommandName))
	return nil
}

// gitLocationVariables are the environment variables that tell Git which
// repository, work tree or index to act on instead of finding one from the
// working directory: GIT_DIR, GIT_WORK_TREE and GIT_COMMON_DIR name the
// repository and its work tree, GIT_INDEX_FILE and GIT_OBJECT_DIRECTORY
// redirect its index and objects, and GIT_IMPLICIT_WORK_TREE and GIT_PREFIX
// are what Git itself sets when it has already chosen one. A Git hook, or a
// tool run from one, inherits them, and then `rev-parse --show-toplevel`
// would answer for that repository, not the checkout init was run in. The list
// is the repository-location part of `git rev-parse --local-env-vars`. Left
// out on purpose: GIT_CEILING_DIRECTORIES and GIT_DISCOVERY_ACROSS_FILESYSTEM,
// which only limit how far up Git looks (they can make it find nothing, never
// another repository, and are the user's own policy); GIT_NAMESPACE, which
// affects refs; and the GIT_CONFIG* variables, which carry settings such as
// safe.directory that the user set for this very reason.
var gitLocationVariables = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY", "GIT_IMPLICIT_WORK_TREE", "GIT_PREFIX",
}

// withoutGitLocation is a copy of environ (NAME=value entries) without the
// gitLocationVariables; every other entry is kept, in order.
func withoutGitLocation(environ []string) []string {
	kept := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(gitLocationVariables, name) {
			kept = append(kept, entry)
		}
	}
	return kept
}

// gitToplevel is the root of the Git checkout that contains env.Cwd, as Git
// prints it. That is already a physical path, so it is only cleaned, not
// resolved again as registry.CanonicalPath does for identity. A directory
// outside any checkout is refused with a message that says where to run init
// instead, and every other Git failure (a bare repository, an unsafe
// directory, a corrupt repository) with the first line of what Git said, so a
// "dubious ownership" refusal never reads as "not a checkout". Git missing
// from PATH exits 127, the code a command that cannot start gets elsewhere.
// A repository that is a submodule is its own checkout: init writes there.
func gitToplevel(ctx context.Context, env *Env) (string, error) {
	result, err := env.Runner.Run(ctx, run.Spec{
		Command:      "git",
		Args:         registry.HardenedGitArgs(env.Cwd, "rev-parse", "--show-toplevel"),
		AllowFailure: true,
		Timeout:      30 * time.Second,
		OutputLimit:  64 * 1024,
		// Git's wording is localised; the "not a git repository" match below
		// is on the C locale's. The rest of the environment is kept.
		Env: append(withoutGitLocation(env.Environ), "LC_ALL=C"),
	})
	var startErr *run.StartError
	switch {
	case errors.As(err, &startErr) && startErr.Code == "ENOENT":
		return "", clierr.WithCode(fmt.Sprintf("Git was not found on PATH. Install Git, then run '%s init' again.", env.CommandName), startErr.ExitStatus())
	case err != nil:
		return "", err
	}
	// Git ends its answer with one newline; a path may hold other whitespace.
	root := strings.TrimSuffix(result.Stdout, "\n")
	if result.ExitCode == 0 && root != "" {
		return filepath.Clean(root), nil
	}
	said, _, _ := strings.Cut(strings.TrimSpace(result.Stderr), "\n")
	if strings.Contains(said, "not a git repository") {
		return "", clierr.Newf("%s is not inside a Git checkout (%s). Run '%s init' at the root of the repository you want to manage.",
			env.Cwd, said, env.CommandName)
	}
	if said == "" {
		said = fmt.Sprintf("git exited %d", result.ExitCode)
	}
	return "", clierr.Newf("Git could not find the root of the checkout containing %s: %s", env.Cwd, said)
}

// findComposeFile is the first of composeFileNames that is a file in root. A
// path that is not there is simply absent; any other failure to look is
// returned, since writing a starter that names the wrong file would be worse
// than stopping.
func findComposeFile(root string) (string, bool, error) {
	for _, name := range composeFileNames {
		info, err := os.Stat(filepath.Join(root, name))
		switch {
		case err == nil && info.Mode().IsRegular():
			return name, true, nil
		case err == nil, errors.Is(err, fs.ErrNotExist):
		default:
			return "", false, fmt.Errorf("looking for %s in %s: %w", name, root, err)
		}
	}
	return "", false, nil
}
