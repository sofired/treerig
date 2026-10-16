package commands_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/commands/commandstest"
	"github.com/sofired/treerig/internal/config"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/lock"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/procid"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/worktree"
)

// bundleWorld is a registered worktree with running services and both bundle
// hooks configured, and a recorder for the hooks `backup` and `restore` start.
type bundleWorld struct {
	*commandstest.World
	tc  *commandstest.TestContext
	ref worktree.Ref

	mu    sync.Mutex
	specs []run.Spec
	// during, when set, is called inside the hook, while the command still
	// holds whatever it holds; its result is the hook's.
	during func(ctx context.Context, spec run.Spec) (run.Result, error)
}

// newBundleWorld registers a worktree with services running; services names
// them, and none leaves the worktree down.
func newBundleWorld(t *testing.T, services ...string) *bundleWorld {
	t.Helper()
	w := commandstest.New(t, "feature/login", acme)
	bw := &bundleWorld{World: w}
	bw.ref = w.Register("", acme, "feature-login", brand.LabelNamespace, services...)
	bw.tc = w.Context("", acme, nil)
	bw.tc.Loaded.Config.Hooks = config.Hooks{Backup: ns("./hooks/backup --fast"), Restore: ns("./hooks/restore")}
	bw.tc.Environ = []string{"PATH=/usr/bin"}
	bw.tc.Cwd = registrytest.TempDir(t)
	fallback := w.Runner.Handler
	w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
		if spec.Label != "backup hook" && spec.Label != "restore hook" {
			return fallback(ctx, spec)
		}
		bw.mu.Lock()
		bw.specs = append(bw.specs, spec)
		during := bw.during
		bw.mu.Unlock()
		if during != nil {
			return during(ctx, spec)
		}
		return run.Result{}, nil
	}
	return bw
}

// started is every bundle hook the commands have asked the runner for.
func (bw *bundleWorld) started() []run.Spec {
	bw.mu.Lock()
	defer bw.mu.Unlock()
	return append([]run.Spec(nil), bw.specs...)
}

// operationLock is a Lock over the worktree's operation lock that gives up
// after a moment, for probing whether it is held.
func (bw *bundleWorld) operationLock() *lock.Lock {
	scope, _ := lock.WorktreeScopes(bw.ControlHome, bw.ref.RepositoryID, bw.ref.WorktreeID, bw.ref.Slug, procid.Markers{})
	return lock.New(scope, lock.Options{Logger: logger.Silent{}, Timeout: 300 * time.Millisecond, Watchdog: -1})
}

// fixedClock is a clock that always reads at.
func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

func TestBackup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	t.Run("runs the backup hook with the new directory as its last argument and TREERIG_STAGE=backup", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		dir := filepath.Join(bw.tc.Cwd, "out", "bundle")

		if err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir}); err != nil {
			t.Fatalf("Backup: %v", err)
		}
		specs := bw.started()
		if len(specs) != 1 {
			t.Fatalf("started %d hooks, want 1", len(specs))
		}
		spec := specs[0]
		if !strings.HasSuffix(spec.Command, "/hooks/backup") || strings.Join(spec.Args, "|") != "--fast|"+dir {
			t.Errorf("spec = %+v", spec)
		}
		if spec.Dir != bw.Root {
			t.Errorf("Dir = %q, want the project root %q", spec.Dir, bw.Root)
		}
		for k, v := range map[string]string{"TREERIG_STAGE": "backup", "TREERIG_WORKTREE_ID": bw.ref.WorktreeID, "TREERIG_ROOT": bw.Root} {
			if !containsEnv(spec.Env, k, v) {
				t.Errorf("want %s=%s in %v", k, v, spec.Env)
			}
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("bundle directory: %v, %v; want a directory with mode 0700", info, err)
		}
		if _, printed := bw.tc.Logger.Snapshot(); len(printed) != 0 {
			t.Errorf("printed %v on stdout", printed)
		}
		lines, _ := bw.tc.Logger.Snapshot()
		want := "info: Backed up feature-login.acme.localhost to " + dir + "."
		if !containsLine(lines, want) {
			t.Errorf("logged %q, want a line %q", lines, want)
		}
	})

	t.Run("defaults to a timestamped directory under the worktree's home", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		at := time.Date(2026, 10, 12, 15, 30, 45, 0, time.FixedZone("x", 3600))

		if err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Now: fixedClock(at)}); err != nil {
			t.Fatal(err)
		}
		home := layout.WorktreeLayout(bw.ControlHome, bw.ref.RepositoryID, bw.ref.WorktreeID).Home
		want := filepath.Join(home, "backups", "20261012T143045Z")
		args := bw.started()[0].Args
		if got := args[len(args)-1]; got != want {
			t.Errorf("bundle directory = %q, want %q", got, want)
		}
		if info, err := os.Stat(want); err != nil || !info.IsDir() {
			t.Errorf("default directory not created: %v", err)
		}
	})

	t.Run("takes a relative directory from the working directory, not the project root", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		if err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: "here/../bundle"}); err != nil {
			t.Fatal(err)
		}
		args := bw.started()[0].Args
		if want := filepath.Join(bw.tc.Cwd, "bundle"); args[len(args)-1] != want {
			t.Errorf("bundle directory = %q, want %q", args[len(args)-1], want)
		}
	})

	t.Run("uses an existing empty directory", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		dir := filepath.Join(bw.tc.Cwd, "empty")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir}); err != nil {
			t.Fatal(err)
		}
		if len(bw.started()) != 1 {
			t.Fatal("the hook did not run")
		}
	})

	t.Run("refuses a directory that holds anything, or is not a directory, without running the hook", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		full := filepath.Join(bw.tc.Cwd, "full")
		if err := os.Mkdir(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, "old.sql"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(bw.tc.Cwd, "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		for dir, want := range map[string]string{
			full: full + " is not empty; give 'treerig backup' a new or empty directory.",
			file: file + " is not a directory.",
		} {
			err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir})
			if err == nil || err.Error() != want || clierr.Code(err) != 1 {
				t.Errorf("err = %v, want %q", err, want)
			}
		}
		if n := len(bw.started()); n != 0 {
			t.Fatalf("ran %d hooks", n)
		}
	})

	t.Run("refuses when no backup hook is configured", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		bw.tc.Loaded.Config.Hooks.Backup = nil
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: filepath.Join(bw.tc.Cwd, "b")})
		want := "No backup hook is configured: add backup under [hooks] in treerig.toml."
		if err == nil || err.Error() != want || clierr.Code(err) != 1 {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if _, statErr := os.Stat(filepath.Join(bw.tc.Cwd, "b")); statErr == nil {
			t.Error("created the directory")
		}
	})

	t.Run("refuses a worktree that is not registered", func(t *testing.T) {
		w := commandstest.New(t, "feature/login", acme)
		tc := w.Context("", acme, nil)
		tc.Loaded.Config.Hooks = config.Hooks{Backup: ns("./hooks/backup")}
		err := commands.BackupWithContext(ctx, tc.CommandContext, commands.BackupOptions{Dir: "x"})
		want := "This worktree is not registered. Run 'treerig up' first."
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})

	t.Run("refuses a worktree with no running service, creating nothing", func(t *testing.T) {
		bw := newBundleWorld(t)
		dir := filepath.Join(bw.tc.Cwd, "b")
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir})
		want := "feature-login.acme.localhost has no running service, and the backup hook needs one. Run 'treerig up' first."
		if err == nil || err.Error() != want || clierr.Code(err) != 1 {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if _, statErr := os.Stat(dir); statErr == nil {
			t.Error("created the directory")
		}
		if n := len(bw.started()); n != 0 {
			t.Fatalf("ran %d hooks", n)
		}
	})

	t.Run("exits with the hook's own status; a bundle the hook began keeps its marker and a warning", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		bw.during = func(_ context.Context, spec run.Spec) (run.Result, error) {
			if err := os.WriteFile(filepath.Join(spec.Args[len(spec.Args)-1], "half.sql"), nil, 0o600); err != nil {
				t.Error(err)
			}
			return run.Result{ExitCode: 3}, nil
		}
		dir := filepath.Join(bw.tc.Cwd, "b")
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir})
		if err == nil || err.Error() != "The backup hook failed (exit 3)." || clierr.Code(err) != 3 {
			t.Fatalf("err = %v (code %d)", err, clierr.Code(err))
		}
		lines, _ := bw.tc.Logger.Snapshot()
		want := "warn: " + dir + " may hold a partial backup, marked .incomplete; 'treerig restore' refuses it. Delete it, or give the next backup another directory."
		if !containsLine(lines, want) {
			t.Errorf("logged %q, want %q", lines, want)
		}
		if _, err := os.Stat(filepath.Join(dir, ".incomplete")); err != nil {
			t.Errorf("marker: %v", err)
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "info: Backed up") {
				t.Errorf("claimed success: %q", line)
			}
		}
	})

	t.Run("removes the directory it created, and says nothing, when the hook failed before writing anything", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		bw.during = func(_ context.Context, spec run.Spec) (run.Result, error) {
			return run.Result{}, &run.StartError{Command: spec.Command, Code: "ENOENT", Detail: "does not exist (ENOENT).", ExitCode: 127}
		}
		dir := filepath.Join(bw.tc.Cwd, "made", "b")
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir})
		if clierr.Code(err) != 127 {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(dir); statErr == nil {
			t.Error("left the directory it created")
		}
		if lines, _ := bw.tc.Logger.Snapshot(); strings.Contains(strings.Join(lines, "\n"), "partial") {
			t.Errorf("logged %q", lines)
		}
	})

	t.Run("leaves an existing empty directory empty when the hook failed before writing anything", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		bw.during = func(context.Context, run.Spec) (run.Result, error) { return run.Result{ExitCode: 1}, nil }
		dir := filepath.Join(bw.tc.Cwd, "empty")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		_ = commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir})
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("entries = %v, %v; want the directory kept and empty", entries, err)
		}
	})

	t.Run("marks the bundle incomplete while the hook runs and removes the marker on success", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		dir := filepath.Join(bw.tc.Cwd, "b")
		var during bool
		bw.during = func(context.Context, run.Spec) (run.Result, error) {
			_, err := os.Stat(filepath.Join(dir, ".incomplete"))
			during = err == nil
			return run.Result{}, nil
		}
		if err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir}); err != nil {
			t.Fatal(err)
		}
		if !during {
			t.Error("no marker while the hook ran")
		}
		if _, err := os.Stat(filepath.Join(dir, ".incomplete")); err == nil {
			t.Error("marker left after success")
		}
	})

	t.Run("succeeds when the hook has already removed the marker itself", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		dir := filepath.Join(bw.tc.Cwd, "b")
		bw.during = func(_ context.Context, spec run.Spec) (run.Result, error) {
			if err := os.Remove(filepath.Join(spec.Args[len(spec.Args)-1], ".incomplete")); err != nil {
				t.Error(err)
			}
			return run.Result{}, nil
		}
		if err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir}); err != nil {
			t.Fatalf("Backup: %v", err)
		}
	})

	t.Run("fails with exit 1 when the marker cannot be removed after a successful hook", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		dir := filepath.Join(bw.tc.Cwd, "b")
		// A non-empty directory in the marker's place cannot be removed.
		bw.during = func(_ context.Context, spec run.Spec) (run.Result, error) {
			marker := filepath.Join(spec.Args[len(spec.Args)-1], ".incomplete")
			if err := os.Remove(marker); err != nil {
				t.Error(err)
			}
			if err := os.MkdirAll(filepath.Join(marker, "keep"), 0o700); err != nil {
				t.Error(err)
			}
			return run.Result{}, nil
		}
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir})
		want := "The backup hook succeeded, but .incomplete could not be removed from " + dir + ", so restore will refuse it"
		if err == nil || !strings.HasPrefix(err.Error(), want) || clierr.Code(err) != 1 {
			t.Fatalf("err = %v (code %d), want one starting %q", err, clierr.Code(err), want)
		}
		lines, _ := bw.tc.Logger.Snapshot()
		for _, line := range lines {
			if strings.HasPrefix(line, "info: Backed up") {
				t.Errorf("claimed success: %q", line)
			}
		}
	})

	t.Run("says after a default-location backup that worktree remove deletes it", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		if err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{}); err != nil {
			t.Fatal(err)
		}
		lines, _ := bw.tc.Logger.Snapshot()
		want := "info: That is inside the worktree's home, which 'treerig worktree remove' deletes; give 'treerig backup' a directory to keep a bundle longer."
		if !containsLine(lines, want) {
			t.Errorf("logged %q, want %q", lines, want)
		}
		explicit := newBundleWorld(t, "api")
		if err := commands.BackupWithContext(ctx, explicit.tc.CommandContext, commands.BackupOptions{Dir: filepath.Join(explicit.tc.Cwd, "b")}); err != nil {
			t.Fatal(err)
		}
		if lines, _ := explicit.tc.Logger.Snapshot(); containsLine(lines, want) {
			t.Error("an explicit directory was told it would be deleted")
		}
	})

	t.Run("warns about an existing directory other users can read", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		dir := filepath.Join(bw.tc.Cwd, "open")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir}); err != nil {
			t.Fatal(err)
		}
		lines, _ := bw.tc.Logger.Snapshot()
		want := "warn: " + dir + " is readable by other users (mode 0755), and a backup holds this worktree's data; consider chmod 700."
		if !containsLine(lines, want) {
			t.Errorf("logged %q, want %q", lines, want)
		}
	})

	t.Run("refuses, naming the directory, when it cannot be created", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can create a directory anywhere")
		}
		bw := newBundleWorld(t, "api")
		parent := filepath.Join(bw.tc.Cwd, "locked")
		if err := os.Mkdir(parent, 0o500); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(parent, "b")
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir})
		if err == nil || !strings.HasPrefix(err.Error(), "Could not create "+dir) {
			t.Fatalf("err = %v", err)
		}
		if n := len(bw.started()); n != 0 {
			t.Fatalf("ran %d hooks", n)
		}
	})

	t.Run("names the unfinished backup when refusing a directory that holds its marker", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		dir := filepath.Join(bw.tc.Cwd, "half")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".incomplete"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir})
		want := dir + " is not empty (it holds a .incomplete marker from an unfinished backup); give 'treerig backup' a new or empty directory."
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})

	t.Run("refuses a relative directory when there is no working directory to resolve it from", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		bw.tc.Cwd = ""
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: "rel"})
		if err == nil || !strings.Contains(err.Error(), `Cannot resolve the relative directory "rel"`) {
			t.Fatalf("err = %v", err)
		}
		if n := len(bw.started()); n != 0 {
			t.Fatalf("ran %d hooks", n)
		}
	})

	t.Run("wraps a failure to read the services' state with a next step", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		fallback := bw.Runner.Handler
		bw.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if spec.Label != "backup hook" && strings.Contains(strings.Join(spec.Args, " "), " ps ") {
				return run.Result{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"}, nil
			}
			return fallback(ctx, spec)
		}
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: filepath.Join(bw.tc.Cwd, "b")})
		want := "Could not read the state of feature-login.acme.localhost's services; is the Docker daemon running? 'treerig doctor' can explain"
		if err == nil || err.Error() != want || clierr.Code(err) != 1 {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})

	t.Run("gives the hook the domain and ports as they are once the lock is won", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		holder := bw.operationLock()
		if _, err := holder.Acquire(ctx, "treerig up"); err != nil {
			t.Fatal(err)
		}
		released := false
		defer func() {
			if !released {
				_ = holder.Release()
			}
		}()
		done := make(chan error, 1)
		go func() {
			done <- commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: filepath.Join(bw.tc.Cwd, "b")})
		}()
		for waiting := false; !waiting; {
			select {
			case err := <-done:
				t.Fatalf("backup returned before waiting for the lock: %v", err)
			case <-ctx.Done():
				t.Fatal("backup never waited for the lock")
			case <-time.After(20 * time.Millisecond):
			}
			lines, _ := bw.tc.Logger.Snapshot()
			for _, line := range lines {
				if strings.Contains(line, "treerig up") {
					waiting = true
				}
			}
		}
		record, err := bw.tc.Registry.Store(bw.ref.RepositoryID).Find(bw.ref.WorktreeID)
		if err != nil || record == nil {
			t.Fatalf("find: %v, %v", record, err)
		}
		if _, err := registrytest.WriteRecord(bw.ControlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID,
			Slug: record.Slug, Domain: "moved.localhost", LabelNamespace: record.LabelNamespace,
			Initialization: "complete", Ports: map[string]int{"api": 31234},
		}); err != nil {
			t.Fatal(err)
		}
		released = true
		if err := holder.Release(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		lines, _ := bw.tc.Logger.Snapshot()
		if want := "info: Backed up feature-login.moved.localhost to " + filepath.Join(bw.tc.Cwd, "b") + "."; !containsLine(lines, want) {
			t.Errorf("logged %q, want %q", lines, want)
		}
		env := bw.started()[0].Env
		for k, v := range map[string]string{
			"TREERIG_HOST": "feature-login.moved.localhost", "TREERIG_SERVICE_PORT_API": "31234",
			"TREERIG_SERVICE_URL_API": "http://feature-login.moved.localhost",
		} {
			if !containsEnv(env, k, v) {
				t.Errorf("want %s=%s in %v", k, v, env)
			}
		}
	})

	t.Run("gives the hook the worktree's Compose project and file", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		if err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: filepath.Join(bw.tc.Cwd, "b")}); err != nil {
			t.Fatal(err)
		}
		project := layout.ComposeProjectName(bw.ref.RepositoryID, bw.ref.WorktreeID)
		env := bw.started()[0].Env
		for k, v := range map[string]string{
			"COMPOSE_PROJECT_NAME": project, "TREERIG_COMPOSE_PROJECT": project,
			"COMPOSE_FILE": filepath.Join(bw.Root, "compose.yaml"),
		} {
			if !containsEnv(env, k, v) {
				t.Errorf("want %s=%s in %v", k, v, env)
			}
		}
	})

	t.Run("applies the backup hook's configured time limit, and exits 124 with the marker kept when it passes", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		limit := "45s"
		bw.tc.Loaded.Config.Hooks.Timeout = &config.HookTimeouts{Backup: &limit, Restore: ns("2m")}
		bw.during = func(_ context.Context, spec run.Spec) (run.Result, error) {
			if err := os.WriteFile(filepath.Join(spec.Args[len(spec.Args)-1], "half.sql"), nil, 0o600); err != nil {
				t.Error(err)
			}
			return run.Result{TimedOut: true}, nil
		}
		dir := filepath.Join(bw.tc.Cwd, "b")
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir})
		if want := "The backup hook timed out after 45s and was terminated."; err == nil || err.Error() != want || clierr.Code(err) != 124 {
			t.Fatalf("err = %v (code %d), want %q and 124", err, clierr.Code(err), want)
		}
		if got := bw.started()[0].Timeout; got != 45*time.Second {
			t.Errorf("Timeout = %v, want 45s", got)
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".incomplete")); statErr != nil {
			t.Errorf("a timed-out backup lost its marker: %v", statErr)
		}
	})

	t.Run("passes a start failure's 127 through", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		bw.during = func(_ context.Context, spec run.Spec) (run.Result, error) {
			return run.Result{}, &run.StartError{Command: spec.Command, Code: "ENOENT", Detail: "does not exist (ENOENT).", ExitCode: 127}
		}
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: filepath.Join(bw.tc.Cwd, "b")})
		if err == nil || !strings.HasPrefix(err.Error(), "The backup hook could not be started: `") || clierr.Code(err) != 127 {
			t.Fatalf("err = %v (code %d)", err, clierr.Code(err))
		}
	})

	t.Run("holds the operation lock while the hook runs, under its own name, and releases it after", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		var heldErr error
		bw.during = func(context.Context, run.Spec) (run.Result, error) {
			_, heldErr = bw.operationLock().Acquire(ctx, "probe")
			return run.Result{}, nil
		}
		if err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: filepath.Join(bw.tc.Cwd, "b")}); err != nil {
			t.Fatal(err)
		}
		if heldErr == nil || !strings.Contains(heldErr.Error(), `"treerig backup"`) {
			t.Errorf("a waiter is told %v, want it to name `treerig backup`", heldErr)
		}
		lk := bw.operationLock()
		if _, err := lk.Acquire(ctx, "probe"); err != nil {
			t.Fatalf("the lock is still held: %v", err)
		}
		_ = lk.Release()
	})

	t.Run("releases the lock when the hook is interrupted", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		interrupted, interrupt := context.WithCancelCause(ctx)
		bw.during = func(ctx context.Context, _ run.Spec) (run.Result, error) {
			interrupt(&clierr.Cancelled{Signal: 2})
			<-ctx.Done()
			return run.Result{}, context.Cause(ctx)
		}
		err := commands.BackupWithContext(interrupted, bw.tc.CommandContext, commands.BackupOptions{Dir: filepath.Join(bw.tc.Cwd, "b")})
		var cancelled *clierr.Cancelled
		if !errors.As(err, &cancelled) {
			t.Fatalf("err = %v, want the cancellation", err)
		}
		lk := bw.operationLock()
		if _, err := lk.Acquire(ctx, "probe"); err != nil {
			t.Fatalf("the lock is still held after an interrupt: %v", err)
		}
		_ = lk.Release()
	})

	t.Run("refuses inside its own worktree's hooks, before anything is created", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		inside(bw.tc, bw.ref.WorktreeID)
		dir := filepath.Join(bw.tc.Cwd, "b")
		err := commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: dir})
		requireNestedRefusal(t, err, layout.WorktreeHost("feature-login", acme))
		if _, statErr := os.Stat(dir); statErr == nil || len(bw.started()) != 0 {
			t.Error("created the directory or ran the hook")
		}
	})

	t.Run("refuses, running nothing, when the worktree was removed while it waited for the lock", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		holder := bw.operationLock()
		if _, err := holder.Acquire(ctx, "treerig worktree remove"); err != nil {
			t.Fatal(err)
		}
		released := false
		defer func() {
			if !released {
				_ = holder.Release()
			}
		}()
		done := make(chan error, 1)
		go func() {
			done <- commands.BackupWithContext(ctx, bw.tc.CommandContext, commands.BackupOptions{Dir: filepath.Join(bw.tc.Cwd, "b")})
		}()
		for waiting := false; !waiting; {
			select {
			case err := <-done:
				t.Fatalf("backup returned before waiting for the lock: %v", err)
			case <-ctx.Done():
				t.Fatal("backup never waited for the lock")
			case <-time.After(20 * time.Millisecond):
			}
			lines, _ := bw.tc.Logger.Snapshot()
			for _, line := range lines {
				if strings.Contains(line, "treerig worktree remove") {
					waiting = true
				}
			}
		}
		if _, err := bw.tc.Registry.Store(bw.ref.RepositoryID).Remove(ctx, bw.ref.WorktreeID, bw.ref.Slug,
			registry.Writer{Logger: logger.Silent{}, Invocation: "treerig worktree remove", UserHome: bw.tc.UserHome}); err != nil {
			t.Fatalf("remove: %v", err)
		}
		released = true
		if err := holder.Release(); err != nil {
			t.Fatal(err)
		}
		err := <-done
		want := "feature-login.acme.localhost was removed while 'treerig backup' was waiting for its lock; run 'treerig up' first."
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if n := len(bw.started()); n != 0 {
			t.Fatalf("ran %d hooks", n)
		}
	})
}

func TestRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	t.Run("runs the restore hook with the existing directory as its argument and TREERIG_STAGE=restore", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		dir := filepath.Join(bw.tc.Cwd, "bundle")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "db.sql"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := commands.RestoreWithContext(ctx, bw.tc.CommandContext, commands.RestoreOptions{Dir: "bundle"}); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		specs := bw.started()
		if len(specs) != 1 || !strings.HasSuffix(specs[0].Command, "/hooks/restore") || strings.Join(specs[0].Args, "|") != dir {
			t.Fatalf("specs = %+v", specs)
		}
		if !containsEnv(specs[0].Env, "TREERIG_STAGE", "restore") {
			t.Errorf("env = %v", specs[0].Env)
		}
		lines, _ := bw.tc.Logger.Snapshot()
		if !containsLine(lines, "info: Restored feature-login.acme.localhost from "+dir+".") {
			t.Errorf("logged %q", lines)
		}
	})

	t.Run("refuses a directory that does not exist or is a file, without running the hook", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		missing := filepath.Join(bw.tc.Cwd, "missing")
		file := filepath.Join(bw.tc.Cwd, "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		for dir, want := range map[string]string{
			missing: missing + " does not exist; restore needs an existing bundle directory.",
			file:    file + " is not a directory.",
		} {
			err := commands.RestoreWithContext(ctx, bw.tc.CommandContext, commands.RestoreOptions{Dir: dir})
			if err == nil || err.Error() != want || clierr.Code(err) != 1 {
				t.Errorf("err = %v, want %q", err, want)
			}
		}
		if _, err := os.Stat(missing); err == nil {
			t.Error("restore created the directory")
		}
		if n := len(bw.started()); n != 0 {
			t.Fatalf("ran %d hooks", n)
		}
	})

	t.Run("refuses a bundle a backup left incomplete", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		dir := filepath.Join(bw.tc.Cwd, "half")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".incomplete"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := commands.RestoreWithContext(ctx, bw.tc.CommandContext, commands.RestoreOptions{Dir: dir})
		want := dir + " was left incomplete by a backup that did not finish (it holds a .incomplete file). " +
			"Make another backup, or delete that file if you know the bundle is complete."
		if err == nil || err.Error() != want || clierr.Code(err) != 1 {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if n := len(bw.started()); n != 0 {
			t.Fatalf("ran %d hooks", n)
		}
	})

	t.Run("warns about an empty directory and runs the hook anyway", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		dir := filepath.Join(bw.tc.Cwd, "empty")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := commands.RestoreWithContext(ctx, bw.tc.CommandContext, commands.RestoreOptions{Dir: dir}); err != nil {
			t.Fatal(err)
		}
		lines, _ := bw.tc.Logger.Snapshot()
		if want := "warn: " + dir + " is empty, so there is nothing to restore; running the restore hook anyway."; !containsLine(lines, want) {
			t.Errorf("logged %q, want %q", lines, want)
		}
		if len(bw.started()) != 1 {
			t.Fatal("the hook did not run")
		}
	})

	t.Run("applies the restore hook's configured time limit", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		bw.tc.Loaded.Config.Hooks.Timeout = &config.HookTimeouts{Restore: ns("2m")}
		bw.during = func(context.Context, run.Spec) (run.Result, error) { return run.Result{TimedOut: true}, nil }
		err := commands.RestoreWithContext(ctx, bw.tc.CommandContext, commands.RestoreOptions{Dir: bw.tc.Cwd})
		if want := "The restore hook timed out after 2m and was terminated."; err == nil || err.Error() != want || clierr.Code(err) != 124 {
			t.Fatalf("err = %v (code %d), want %q and 124", err, clierr.Code(err), want)
		}
		if got := bw.started()[0].Timeout; got != 2*time.Minute {
			t.Errorf("Timeout = %v, want 2m", got)
		}
	})

	t.Run("refuses without a directory", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		err := commands.RestoreWithContext(ctx, bw.tc.CommandContext, commands.RestoreOptions{})
		want := "Restore needs a bundle directory: treerig restore <dir>."
		if err == nil || err.Error() != want || clierr.Code(err) != 1 {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})

	t.Run("refuses when no restore hook is configured", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		bw.tc.Loaded.Config.Hooks.Restore = nil
		err := commands.RestoreWithContext(ctx, bw.tc.CommandContext, commands.RestoreOptions{Dir: bw.tc.Cwd})
		want := "No restore hook is configured: add restore under [hooks] in treerig.toml."
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})

	t.Run("refuses a worktree with no running service", func(t *testing.T) {
		bw := newBundleWorld(t)
		err := commands.RestoreWithContext(ctx, bw.tc.CommandContext, commands.RestoreOptions{Dir: bw.tc.Cwd})
		want := "feature-login.acme.localhost has no running service, and the restore hook needs one. Run 'treerig up' first."
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})

	t.Run("exits with the hook's own status and holds the lock under its own name", func(t *testing.T) {
		bw := newBundleWorld(t, "api")
		var heldErr error
		bw.during = func(context.Context, run.Spec) (run.Result, error) {
			_, heldErr = bw.operationLock().Acquire(ctx, "probe")
			return run.Result{ExitCode: 4}, nil
		}
		err := commands.RestoreWithContext(ctx, bw.tc.CommandContext, commands.RestoreOptions{Dir: bw.tc.Cwd})
		if err == nil || err.Error() != "The restore hook failed (exit 4)." || clierr.Code(err) != 4 {
			t.Fatalf("err = %v (code %d)", err, clierr.Code(err))
		}
		if heldErr == nil || !strings.Contains(heldErr.Error(), `"treerig restore"`) {
			t.Errorf("a waiter is told %v, want it to name `treerig restore`", heldErr)
		}
	})
}

// containsLine reports whether lines holds want exactly.
func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}
