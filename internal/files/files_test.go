package files_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
)

// fileMode is the permission bits of a file or directory, for asserting on modes.
func fileMode(t *testing.T, file string) os.FileMode {
	t.Helper()
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat %s: %v", file, err)
	}
	return info.Mode().Perm()
}

// requireNoError fails t immediately if err is non-nil.
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// requireErrorContains fails t unless err is non-nil and its message contains substr.
func requireErrorContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected error containing %q, got %q", substr, err.Error())
	}
}

func TestExistenceProbes(t *testing.T) {
	t.Run("distinguish missing, empty, and populated files", func(t *testing.T) {
		root := t.TempDir()
		empty := filepath.Join(root, "empty")
		full := filepath.Join(root, "full")
		requireNoError(t, os.WriteFile(empty, nil, 0o600))
		requireNoError(t, os.WriteFile(full, []byte("contents"), 0o600))

		exists, err := files.PathExists(filepath.Join(root, "missing"))
		requireNoError(t, err)
		if exists {
			t.Fatalf("expected missing file to not exist")
		}
		exists, err = files.PathExists(empty)
		requireNoError(t, err)
		if !exists {
			t.Fatalf("expected empty file to exist")
		}
	})
}

func TestAtomicWrite(t *testing.T) {
	t.Run("creates the parent directory, writes owner-only, and leaves no temp file", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "nested", "deep", "record.json")

		requireNoError(t, files.AtomicWrite(destination, []byte("payload"), 0o600))

		contents, err := os.ReadFile(destination)
		requireNoError(t, err)
		if string(contents) != "payload" {
			t.Fatalf("expected payload, got %q", contents)
		}
		if mode := fileMode(t, destination); mode != 0o600 {
			t.Fatalf("expected mode 0600, got %o", mode)
		}
		if mode := fileMode(t, filepath.Join(root, "nested", "deep")); mode != 0o700 {
			t.Fatalf("expected mode 0700, got %o", mode)
		}
		entries, err := os.ReadDir(filepath.Dir(destination))
		requireNoError(t, err)
		if len(entries) != 1 || entries[0].Name() != "record.json" {
			t.Fatalf("expected only record.json, got %v", entries)
		}
	})

	t.Run("round-trips JSON", func(t *testing.T) {
		root := t.TempDir()
		file := filepath.Join(root, "state.json")

		requireNoError(t, files.AtomicJSON(file, map[string]any{"version": 2, "slug": "feature-login"}))

		contents, err := os.ReadFile(file)
		requireNoError(t, err)
		if !strings.Contains(string(contents), `"version": 2`) || !strings.Contains(string(contents), `"slug": "feature-login"`) {
			t.Fatalf("unexpected JSON contents: %s", contents)
		}
		if !strings.HasSuffix(string(contents), "\n") {
			t.Fatalf("expected a trailing newline")
		}
	})

	t.Run("writes the JavaScript line separators raw, unlike encoding/json's own default", func(t *testing.T) {
		root := t.TempDir()
		file := filepath.Join(root, "state.json")
		lineSeparator := string(rune(0x2028))
		paragraphSeparator := string(rune(0x2029))

		requireNoError(t, files.AtomicJSON(file, map[string]any{"branch": "feature/" + lineSeparator + "x" + paragraphSeparator}))

		contents, err := os.ReadFile(file)
		requireNoError(t, err)
		if !strings.Contains(string(contents), lineSeparator) || !strings.Contains(string(contents), paragraphSeparator) {
			t.Fatalf("expected the raw separators in the written bytes, got %q", contents)
		}
		var decoded map[string]string
		requireNoError(t, json.Unmarshal(contents, &decoded))
		if decoded["branch"] != "feature/"+lineSeparator+"x"+paragraphSeparator {
			t.Fatalf("round trip: got %q", decoded["branch"])
		}
	})
}

func TestRemoveWithin(t *testing.T) {
	t.Run("removes a nested target inside the root", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "instance")
		requireNoError(t, os.MkdirAll(filepath.Join(target, "db"), 0o700))
		requireNoError(t, os.WriteFile(filepath.Join(target, "db", "data"), []byte("x"), 0o600))

		requireNoError(t, files.RemoveWithin(root, target))

		exists, err := files.PathExists(target)
		requireNoError(t, err)
		if exists {
			t.Fatalf("expected target to be removed")
		}
	})

	t.Run("refuses the root itself, its parent, and anything outside it", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		requireNoError(t, os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o600))

		requireErrorContains(t, files.RemoveWithin(root, root), "Refusing to remove unsafe path")
		requireErrorContains(t, files.RemoveWithin(root, filepath.Dir(root)), "Refusing to remove unsafe path")
		requireErrorContains(t, files.RemoveWithin(root, filepath.Join(root, "..", filepath.Base(outside))), "Refusing to remove unsafe path")
		exists, err := files.PathExists(filepath.Join(outside, "keep"))
		requireNoError(t, err)
		if !exists {
			t.Fatalf("expected outside file to survive")
		}
	})
}

func TestPathContainment(t *testing.T) {
	t.Run("accepts paths inside the root, including ones that do not exist yet", func(t *testing.T) {
		root := t.TempDir()
		requireNoError(t, files.AssertSafeDirectoryPath(root, root))
		requireNoError(t, files.AssertSafeDirectoryPath(root, filepath.Join(root, "not", "yet")))
	})

	t.Run("refuses paths outside the root", func(t *testing.T) {
		root := t.TempDir()
		requireErrorContains(t, files.AssertSafeDirectoryPath(root, filepath.Dir(root)), "Refusing directory outside the control home")
		requireErrorContains(t, files.AssertSafeDirectoryPath(root, filepath.Join(root, "..", "sibling")), "Refusing directory outside the control home")
	})

	t.Run("refuses a symlink anywhere in the chain", func(t *testing.T) {
		root := t.TempDir()
		elsewhere := t.TempDir()
		requireNoError(t, os.Symlink(elsewhere, filepath.Join(root, "linked")))

		requireErrorContains(t, files.AssertSafeDirectoryPath(root, filepath.Join(root, "linked", "child")), "Refusing symlinked or non-directory managed path")
	})

	t.Run("names a caller's own root in the outside-of-root refusal, for one that is not a control home", func(t *testing.T) {
		root := t.TempDir()
		requireErrorContains(t,
			files.AssertSafeDirectoryPathNamed(root, filepath.Dir(root), "the shared router's state directory"),
			"Refusing directory outside the shared router's state directory")
		requireErrorContains(t,
			files.AssertSafeFilePathNamed(root, filepath.Join(filepath.Dir(root), "sibling", "file"), "the shared router's state directory"),
			"Refusing directory outside the shared router's state directory")
		// The default (AssertSafeDirectoryPath/AssertSafeFilePath) still says
		// "the control home", byte-identical to what every existing caller —
		// and TypeScript's own equivalent — has always said.
		requireErrorContains(t, files.AssertSafeDirectoryPath(root, filepath.Dir(root)), "Refusing directory outside the control home")
		requireErrorContains(t, files.AssertSafeFilePath(root, filepath.Join(filepath.Dir(root), "sibling", "file")), "Refusing directory outside the control home")
	})
}

func TestSecureDirectoryCreation(t *testing.T) {
	t.Run("adopts an empty directory and marks it, adopts an older install, and refuses a stranger's directory", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "control")
		requireNoError(t, files.SecureControlDirectories(layout.ControlLayout(empty), t.TempDir()))
		exists, err := files.PathExists(filepath.Join(empty, files.ControlHomeMarker))
		requireNoError(t, err)
		if !exists {
			t.Fatalf("expected control home marker to exist")
		}
		requireNoError(t, files.SecureControlDirectories(layout.ControlLayout(empty), t.TempDir())) // and again, idempotently

		// An install from before the marker existed has at least two of the tool's directories.
		older := filepath.Join(t.TempDir(), "older")
		requireNoError(t, os.MkdirAll(filepath.Join(older, "repositories"), 0o700))
		requireNoError(t, os.MkdirAll(filepath.Join(older, "locks"), 0o700))
		requireNoError(t, files.SecureControlDirectories(layout.ControlLayout(older), t.TempDir()))

		// A directory a file manager merely looked at is still empty.
		browsed := filepath.Join(t.TempDir(), "browsed")
		requireNoError(t, os.MkdirAll(browsed, 0o700))
		requireNoError(t, os.WriteFile(filepath.Join(browsed, ".DS_Store"), nil, 0o600))
		requireNoError(t, files.SecureControlDirectories(layout.ControlLayout(browsed), t.TempDir()))

		// One generic directory name is not evidence: a code folder with `repositories/` is not ours.
		code := filepath.Join(t.TempDir(), "code")
		requireNoError(t, os.MkdirAll(filepath.Join(code, "repositories", "acme"), 0o700))
		requireErrorContains(t, files.SecureControlDirectories(layout.ControlLayout(code), t.TempDir()), "not empty (repositories) and was not created by berth")

		theirs := filepath.Join(t.TempDir(), "documents")
		// Node's default mkdir mode (0o777 masked by the process umask), not
		// this tool's 0o700: the assertion below checks our code never widens
		// or narrows a directory it refuses to adopt.
		requireNoError(t, os.MkdirAll(theirs, 0o755))
		requireNoError(t, os.WriteFile(filepath.Join(theirs, "thesis.txt"), []byte("years of work"), 0o600))
		resolvedTheirs, err := filepath.EvalSymlinks(theirs)
		requireNoError(t, err)
		err = files.SecureControlDirectories(layout.ControlLayout(theirs), t.TempDir())
		requireErrorContains(t, err, "Refusing to use "+resolvedTheirs+" as the control home: it is not empty (thesis.txt) and was not created by berth. "+
			"Move it aside, or set BERTH_CONTROL_HOME to an empty directory, or to one berth created earlier (it leaves a .berth-control-home marker file).")
		if mode := fileMode(t, theirs); mode == 0o700 {
			t.Fatalf("expected theirs not to be chmod'd to 0700")
		}
		exists, err = files.PathExists(filepath.Join(theirs, "thesis.txt"))
		requireNoError(t, err)
		if !exists {
			t.Fatalf("expected thesis.txt to survive")
		}
	})

	t.Run("refuses the home directory, anything above it, and the filesystem root outright, without telling the user to move them", func(t *testing.T) {
		home, err := os.UserHomeDir()
		requireNoError(t, err)
		requireErrorContains(t, files.AssertAdoptableControlHome(home, home),
			"it is your home directory. Set BERTH_CONTROL_HOME to an empty directory, or to one berth created earlier (it leaves a .berth-control-home marker file).")
		requireErrorContains(t, files.AssertAdoptableControlHome(filepath.Dir(home), home), "it contains your home directory")
		requireErrorContains(t, files.AssertAdoptableControlHome("/", home), "it is the filesystem root")
		// A directory *under* the home is of course fine, even before it exists.
		requireNoError(t, files.AssertAdoptableControlHome(filepath.Join(home, ".berth-does-not-exist-test"), home))
	})

	t.Run("AssertSafeHome refuses the home directory, anything above it, and the filesystem root, pointing at BERTH_HOME", func(t *testing.T) {
		home, err := os.UserHomeDir()
		requireNoError(t, err)
		requireErrorContains(t, files.AssertSafeHome(home, home),
			"it is your home directory. Point BERTH_HOME elsewhere.")
		requireErrorContains(t, files.AssertSafeHome(filepath.Dir(home), home), "it contains your home directory")
		requireErrorContains(t, files.AssertSafeHome("/", home), "it is the filesystem root")
		// A directory *under* the home is of course fine, even before it exists.
		requireNoError(t, files.AssertSafeHome(filepath.Join(home, ".berth-home-does-not-exist-test"), home))
	})

	// Review round 4, item 2: an empty userHome must be refused outright by
	// both assertions, not left to reach filepath.Rel deep inside — which
	// would fail with "Rel: can't make . relative to …", naming neither
	// BERTH_HOME nor BERTH_CONTROL_HOME. commands.checkedHome already
	// refuses this before either assertion is ever reached in production;
	// this pins the same refusal for any caller that reaches them some
	// other way.
	t.Run("both assertions refuse an empty userHome outright, rather than reaching filepath.Rel", func(t *testing.T) {
		const wantMessage = "Could not determine your home directory; set HOME."
		requireErrorContains(t, files.AssertSafeHome(t.TempDir(), ""), wantMessage)
		requireErrorContains(t, files.AssertAdoptableControlHome(t.TempDir(), ""), wantMessage)
	})

	t.Run("creates the control layout owner-only", func(t *testing.T) {
		controlHome := filepath.Join(t.TempDir(), "control")
		l := layout.ControlLayout(controlHome)

		requireNoError(t, files.SecureControlDirectories(l, t.TempDir()))

		for _, directory := range []string{l.ControlHome, l.RepositoriesDir, l.LocksDir} {
			if mode := fileMode(t, directory); mode != 0o700 {
				t.Fatalf("expected %s to be 0700, got %o", directory, mode)
			}
		}
		// The router's own directories are no longer created here: they live
		// under the berth root instead (internal/router, #12, #13, #80), not
		// under any control home. layout.Control no longer even names it
		// (docs/port/divergences.md row 9), so this checks the literal path
		// a TypeScript-era control home would have used.
		routerDir := filepath.Join(controlHome, "router")
		exists, err := files.PathExists(routerDir)
		requireNoError(t, err)
		if exists {
			t.Fatalf("expected %s not to be created", routerDir)
		}
	})

	t.Run("creates a worktree home owner-only and refuses while a removal is half-finished", func(t *testing.T) {
		controlHome := filepath.Join(t.TempDir(), "control")
		l := layout.WorktreeLayout(controlHome, "a08b4892-5111-49f1-a47a-5ab988c7f676", "2b41d9d4-ed88-49c1-be9c-5439bdf6a431")

		requireNoError(t, files.SecureWorktreeDirectories(l, t.TempDir()))
		if mode := fileMode(t, l.Home); mode != 0o700 {
			t.Fatalf("expected home to be 0700, got %o", mode)
		}
		if mode := fileMode(t, l.WorktreeLocksDir); mode != 0o700 {
			t.Fatalf("expected worktreeLocksDir to be 0700, got %o", mode)
		}

		requireNoError(t, os.MkdirAll(l.QuarantinePath, 0o700))
		requireErrorContains(t, files.SecureWorktreeDirectories(l, t.TempDir()), "was interrupted; retry the removal")
	})
}
