package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/hooks"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/text"
	"github.com/sofired/treerig/internal/worktree"
)

// incompleteMarker is the file `backup` writes into a bundle before its hook
// runs and removes when the hook exits zero, so a bundle that a failed or
// interrupted backup left behind is never taken for a finished one:
// `restore` refuses a directory that holds it.
const incompleteMarker = ".incomplete"

// bundleTimestampLayout names a default bundle directory: UTC, sortable, and
// free of characters a file system or a shell objects to.
const bundleTimestampLayout = "20060102T150405Z"

// BackupOptions are `backup`'s arguments.
type BackupOptions struct {
	// Dir is the bundle directory; empty means a timestamped directory under
	// the worktree's home (see defaultBundleDir).
	Dir string
	// Now is the clock the default directory's name is taken from; nil means
	// time.Now.
	Now func() time.Time
}

// RestoreOptions are `restore`'s arguments.
type RestoreOptions struct {
	// Dir is the bundle directory to restore from; it must already exist.
	Dir string
}

// Backup is `backup`: the project's backup hook, run against this registered
// worktree with a bundle directory as its first argument.
func Backup(ctx context.Context, env *Env, opts BackupOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	return BackupWithContext(ctx, cctx, opts)
}

// Restore is `restore`: the project's restore hook, run against this
// registered worktree with an existing bundle directory as its first argument.
func Restore(ctx context.Context, env *Env, opts RestoreOptions) error {
	cctx, err := Context(env)
	if err != nil {
		return err
	}
	return RestoreWithContext(ctx, cctx, opts)
}

// BackupWithContext is `backup`, operating directly on a CommandContext.
//
// The bundle directory is created, or an empty existing one used; a
// directory that holds anything is refused, so a second backup can never be
// mixed into the first. With no directory given it is
// <worktree home>/backups/<UTC timestamp>: the worktree's home is already the
// private (0700) place treerig keeps that worktree's own state, and a bundle
// holds that worktree's data, so it inherits the same protection and needs no
// location of the user's choosing. The price is that `worktree remove`
// deletes the home, bundles included; a backup meant to outlive its
// worktree is given a directory of its own. The command says where the
// bundle went.
func BackupWithContext(ctx context.Context, cctx *CommandContext, opts BackupOptions) error {
	return runBundleHook(ctx, cctx, config.Backup, opts.Dir, opts.Now)
}

// RestoreWithContext is `restore`, operating directly on a CommandContext.
// The bundle directory must exist; what is in it is the hook's business.
func RestoreWithContext(ctx context.Context, cctx *CommandContext, opts RestoreOptions) error {
	if opts.Dir == "" {
		return clierr.Newf("Restore needs a bundle directory: %s restore <dir>.", cctx.CommandName)
	}
	return runBundleHook(ctx, cctx, config.Restore, opts.Dir, nil)
}

// runBundleHook runs stage (backup or restore) for this registered worktree.
//
// It refuses, before taking any lock, when the stage is not configured or the
// worktree is not registered. It then holds the worktree's operation lock, as
// `up` does while its hooks run, and under it re-reads the registration (the
// hook is given the domain and ports as they are now, not as they were before
// a wait for the lock), checks that the worktree has a running service (the
// hooks talk to the services: a dump needs the database up, and a restore
// needs somewhere to load into) and prepares the bundle directory, so a
// refused command leaves nothing behind. The hook goes through
// hooks.RunHook, the runner `up` uses, so its environment, working
// directory, signal handling and exit status are the same: a failing hook's
// own exit code is this command's.
func runBundleHook(ctx context.Context, cctx *CommandContext, stage config.HookStage, dir string, now func() time.Time) error {
	cfg := cctx.Loaded.Config
	if !hooks.Configured(cfg.Hooks, stage) {
		return clierr.Newf("No %s hook is configured: add %s under [hooks] in %s.", stage, stage, brand.ConfigFile)
	}
	registration, err := cctx.requireRegistration(ctx)
	if err != nil {
		return err
	}
	ref := toRef(registration.RegisteredWorktree)
	host := layout.WorktreeHost(ref.Slug, registration.Domain)
	store := cctx.Registry.Store(ref.RepositoryID)
	runtime := cctx.RuntimeFor(cfg.LabelNamespace(), &registration.RegisteredWorktree, cctx.RouterPort)

	locking := *cctx
	locking.Invocation = strings.Join([]string{cctx.CommandName, string(stage)}, " ")
	var bundle string
	var inHome bool
	err = locking.mutate(ctx, ref, host, false, func(ctx context.Context) error {
		// The lock may have been held by someone else for minutes while this
		// waited: a `worktree remove` can have finished in between, and an
		// `up` can have moved the worktree's domain or ports.
		found, err := store.Find(ref.WorktreeID)
		if err != nil {
			return err
		}
		if found == nil {
			return clierr.Newf("%s was removed while '%s %s' was waiting for its lock; run '%s up' first.",
				host, cctx.CommandName, stage, cctx.CommandName)
		}
		host = layout.WorktreeHost(ref.Slug, found.Domain)
		statuses, err := runtime.Status(ctx, ref)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			return clierr.Wrap(fmt.Sprintf("Could not read the state of %s's services; is the Docker daemon running? '%s doctor' can explain", host, cctx.CommandName), err)
		}
		if !anyRunning(statuses) {
			return clierr.Newf("%s has no running service, and the %s hook needs one. Run '%s up' first.",
				host, stage, cctx.CommandName)
		}
		prepared, err := prepareBundle(cctx, stage, ref, dir, now)
		if err != nil {
			return err
		}
		bundle, inHome = prepared.path, prepared.inHome
		if stage == config.Backup {
			if err := os.WriteFile(filepath.Join(bundle, incompleteMarker), nil, 0o600); err != nil {
				prepared.discard()
				return clierr.Wrap(fmt.Sprintf("Could not mark %s as incomplete", bundle), err)
			}
		}
		hctx := hooks.HookContext{
			Source: worktree.EnvironmentSource{
				ProjectName: cfg.Project.Name,
				// The registered domain, not the config's: a hook addresses
				// the hostname that is actually routed.
				Domain:      found.Domain,
				ControlHome: cctx.ControlHome,
				ConfigFile:  cctx.Loaded.File,
				ProjectRoot: cctx.Loaded.Root,
			},
			Ref: ref, Hooks: cfg.Hooks, Logger: cctx.Logger, Progress: cctx.Progress, Runner: cctx.Runner, Environ: cctx.Environ, ComposeFile: cctx.ComposeFile(),
			// The same routes as `up`'s hooks and every Compose call: services
			// and host routes (#177).
			Routes: routesOf(cfg, ref.Slug, found.Domain, cctx.RouterPort, found.Ports, found.HostRoutes).All(),
			Shared: sharedAddrs(cctx, ref.RepositoryID),
		}
		_, hookErr := hooks.RunHook(ctx, stage, hctx, hooks.RunOptions{Argument: &bundle})
		if stage != config.Backup {
			return hookErr
		}
		if hookErr != nil {
			if prepared.discard() {
				return hookErr
			}
			cctx.Logger.Warn(fmt.Sprintf("%s may hold a partial backup, marked %s; '%s restore' refuses it. Delete it, or give the next backup another directory.",
				bundle, incompleteMarker, cctx.CommandName))
			return hookErr
		}
		if err := os.Remove(filepath.Join(bundle, incompleteMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return clierr.Wrap(fmt.Sprintf("The backup hook succeeded, but %s could not be removed from %s, so restore will refuse it", incompleteMarker, bundle), err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if stage == config.Backup {
		// The path alone is the result, so `dir=$(treerig backup)` gets it:
		// the hook's own output went to stderr.
		cctx.Logger.Output(text.Printable(bundle))
		cctx.Logger.Info(fmt.Sprintf("Backed up %s to %s.", host, bundle))
		if inHome {
			cctx.Logger.Info(fmt.Sprintf("That is inside the worktree's home, which '%s worktree remove' deletes; give '%s backup' a directory to keep a bundle longer.",
				cctx.CommandName, cctx.CommandName))
		}
	} else {
		cctx.Logger.Info(fmt.Sprintf("Restored %s from %s.", host, bundle))
	}
	return nil
}

// anyRunning reports whether at least one service is running.
func anyRunning(statuses []worktree.ServiceStatus) bool {
	for _, status := range statuses {
		if status.State == worktree.Running {
			return true
		}
	}
	return false
}

// preparedBundle is a bundle directory made ready for its hook.
type preparedBundle struct {
	// path is the absolute directory the hook is handed.
	path string
	// inHome reports that path is the default location under the worktree's
	// home, which `worktree remove` deletes.
	inHome bool
	// created reports that backup made path itself, so a failure that wrote
	// nothing can take it away again.
	created bool
}

// discard undoes a backup that wrote nothing: it removes the marker, and the
// directory too when treerig created it. It reports whether the directory is
// clean, that is, whether it held nothing of the hook's; a directory that
// holds anything keeps its marker.
func (b preparedBundle) discard() bool {
	entries, err := os.ReadDir(b.path)
	if err != nil {
		return false
	}
	switch {
	case len(entries) == 0:
	case len(entries) == 1 && entries[0].Name() == incompleteMarker:
		if os.Remove(filepath.Join(b.path, incompleteMarker)) != nil {
			return false
		}
	default:
		return false
	}
	if b.created {
		_ = os.Remove(b.path)
	}
	return true
}

// prepareBundle resolves the bundle directory the hook is handed and makes it
// ready for stage. A relative dir is taken from the working directory treerig
// was run from, not the project root the hook runs in, so the hook receives
// the directory the user meant. backup creates it (or accepts an empty one);
// restore requires it to exist already, to be a finished bundle, and says so
// when it is empty.
func prepareBundle(cctx *CommandContext, stage config.HookStage, ref worktree.Ref, dir string, now func() time.Time) (preparedBundle, error) {
	var bundle preparedBundle
	home := layout.WorktreeLayout(cctx.ControlHome, ref.RepositoryID, ref.WorktreeID)
	switch {
	case dir == "":
		if now == nil {
			now = time.Now
		}
		dir = filepath.Join(home.BackupsDir, now().UTC().Format(bundleTimestampLayout))
		bundle.inHome = true
	case !filepath.IsAbs(dir):
		if cctx.Cwd == "" {
			return bundle, clierr.Newf("Cannot resolve the relative directory %q: this process has no working directory to resolve it from.", dir)
		}
		dir = filepath.Join(cctx.Cwd, dir)
	}
	bundle.path = filepath.Clean(dir)

	if stage == config.Backup {
		// 0700: a bundle holds the worktree's data.
		if err := os.MkdirAll(filepath.Dir(bundle.path), 0o700); err != nil {
			return bundle, clierr.Wrap(fmt.Sprintf("Could not create %s", filepath.Dir(bundle.path)), err)
		}
		switch err := os.Mkdir(bundle.path, 0o700); {
		case err == nil:
			bundle.created = true
			return bundle, nil
		case !errors.Is(err, fs.ErrExist):
			return bundle, clierr.Wrap(fmt.Sprintf("Could not create %s", bundle.path), err)
		}
	}

	info, err := os.Stat(bundle.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return bundle, clierr.Newf("%s does not exist; %s needs an existing bundle directory.", bundle.path, stage)
	case err != nil:
		return bundle, clierr.Wrap(fmt.Sprintf("Could not read %s", bundle.path), err)
	case !info.IsDir():
		return bundle, clierr.Newf("%s is not a directory.", bundle.path)
	}
	empty, err := isEmptyDir(bundle.path)
	if err != nil {
		return bundle, clierr.Wrap(fmt.Sprintf("Could not read %s", bundle.path), err)
	}
	if stage == config.Backup {
		if !empty {
			unfinished := ""
			if _, err := os.Lstat(filepath.Join(bundle.path, incompleteMarker)); err == nil {
				unfinished = fmt.Sprintf(" (it holds a %s marker from an unfinished backup)", incompleteMarker)
			}
			return bundle, clierr.Newf("%s is not empty%s; give '%s backup' a new or empty directory.", bundle.path, unfinished, cctx.CommandName)
		}
		if info.Mode().Perm()&0o077 != 0 {
			cctx.Logger.Warn(fmt.Sprintf("%s is readable by other users (mode %04o), and a backup holds this worktree's data; consider chmod 700.",
				bundle.path, info.Mode().Perm()))
		}
		return bundle, nil
	}
	if _, err := os.Lstat(filepath.Join(bundle.path, incompleteMarker)); err == nil {
		return bundle, clierr.Newf("%s was left incomplete by a backup that did not finish (it holds a %s file). "+
			"Make another backup, or delete that file if you know the bundle is complete.", bundle.path, incompleteMarker)
	}
	if empty {
		cctx.Logger.Warn(fmt.Sprintf("%s is empty, so there is nothing to restore; running the restore hook anyway.", bundle.path))
	}
	return bundle, nil
}

// isEmptyDir reports whether the directory at path holds nothing.
func isEmptyDir(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}
