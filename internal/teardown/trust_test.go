package teardown_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/teardown"
)

// same is the boundary for a home the user database and the environment agree on.
func same(home string) teardown.HomeBoundary { return teardown.HomeFromUserDatabase(home, home) }

func TestHomeFromUserDatabase(t *testing.T) {
	home := registrytest.TempDir(t)
	other := registrytest.TempDir(t)
	if err := os.Mkdir(filepath.Join(home, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	l := layout.ControlLayout(filepath.Join(home, ".treerig", "control-home"))
	for name, tc := range map[string]struct {
		listed, claimed string
		exempt          bool
	}{
		"the listed home is the claimed one":    {home, home, true},
		"no listed home":                        {"", home, false},
		"no claimed home":                       {home, "", false},
		"the claimed home is another directory": {home, other, false},
		"the listed home does not exist":        {filepath.Join(home, "nope"), home, false},
	} {
		err := teardown.CheckLocation(l, teardown.HomeFromUserDatabase(tc.listed, tc.claimed))
		if exempt := err == nil; exempt != tc.exempt {
			t.Errorf("%s: err = %v, want exempt = %v", name, err, tc.exempt)
		}
	}
}

func TestCheckLocation(t *testing.T) {
	t.Run("accepts a control home that no work tree holds", func(t *testing.T) {
		l, home := newControl(t)
		if err := teardown.CheckLocation(l, same(home)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refuses a control home inside a checkout, whether .git is a directory or a file", func(t *testing.T) {
		for _, kind := range []string{"directory", "file"} {
			checkout := registrytest.TempDir(t)
			dot := filepath.Join(checkout, ".git")
			if kind == "directory" {
				if err := os.Mkdir(dot, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(dot, []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			l := layout.ControlLayout(filepath.Join(checkout, ".pin", "deep"))
			err := teardown.CheckLocation(l, same(registrytest.TempDir(t)))
			var inside *teardown.InsideWorkTreeError
			if !errors.As(err, &inside) || inside.Root != checkout || inside.Path != l.ControlHome {
				t.Fatalf(".git as a %s: err = %v", kind, err)
			}
		}
	})

	t.Run("sees through a symlink into a checkout", func(t *testing.T) {
		checkout := registrytest.TempDir(t)
		if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(checkout, "pin"), 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(registrytest.TempDir(t), "control")
		if err := os.Symlink(filepath.Join(checkout, "pin"), link); err != nil {
			t.Fatal(err)
		}
		var inside *teardown.InsideWorkTreeError
		if err := teardown.CheckLocation(layout.ControlLayout(link), same(registrytest.TempDir(t))); !errors.As(err, &inside) || inside.Root != checkout {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("walks the teardown directory too: a clean control home whose teardown is a symlink into a checkout", func(t *testing.T) {
		checkout := registrytest.TempDir(t)
		if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(checkout, "pin"), 0o700); err != nil {
			t.Fatal(err)
		}
		l := layout.ControlLayout(filepath.Join(registrytest.TempDir(t), "control-home"))
		if err := os.MkdirAll(l.ControlHome, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(checkout, "pin"), l.TeardownDir); err != nil {
			t.Fatal(err)
		}
		// The control home is clean; only the teardown directory resolves into the checkout.
		var inside *teardown.InsideWorkTreeError
		err := teardown.CheckLocation(l, teardown.HomeBoundary{})
		if !errors.As(err, &inside) || inside.Root != checkout {
			t.Fatalf("err = %v, want an InsideWorkTreeError with Root %s", err, checkout)
		}
	})

	t.Run("the exemption is the home it is given, compared as a file, and nothing a checkout can set", func(t *testing.T) {
		realHome := registrytest.TempDir(t)
		checkout := registrytest.TempDir(t)
		if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		// A control home in a checkout is refused whatever the real home is.
		inside := layout.ControlLayout(filepath.Join(checkout, ".c"))
		if err := teardown.CheckLocation(inside, same(realHome)); err == nil {
			t.Fatal("a control home inside a checkout was accepted")
		}
		// Another spelling of the real home is still the real home.
		if err := os.Mkdir(filepath.Join(realHome, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(registrytest.TempDir(t), "home-link")
		if err := os.Symlink(realHome, link); err != nil {
			t.Fatal(err)
		}
		dotfiles := layout.ControlLayout(filepath.Join(realHome, ".treerig", "control-home"))
		if err := teardown.CheckLocation(dotfiles, same(link)); err != nil {
			t.Fatalf("a symlinked spelling of the home did not exempt it: %v", err)
		}
	})

	t.Run("does not count a work tree rooted at the user's home or above it", func(t *testing.T) {
		home := registrytest.TempDir(t)
		if err := os.Mkdir(filepath.Join(home, ".git"), 0o700); err != nil { // a dotfiles repository
			t.Fatal(err)
		}
		l := layout.ControlLayout(filepath.Join(home, ".treerig", "control-home"))
		if err := teardown.CheckLocation(l, same(home)); err != nil {
			t.Fatalf("err = %v, want a dotfiles repository at the home directory tolerated", err)
		}
		if err := teardown.CheckLocation(l, teardown.HomeBoundary{}); err == nil {
			t.Fatal("with no home to stop at, the same layout was accepted")
		}
	})
}

func TestCheckOwnership(t *testing.T) {
	l, home := newControl(t)
	content := "the pinned bytes"
	pin := filePin("b", content)
	if _, err := teardown.Save(l, home, []byte(content), pin, time.Now()); err != nil {
		t.Fatal(err)
	}
	open := func(t *testing.T) *teardown.Binary {
		t.Helper()
		_, binary := teardown.OpenBinary(l, pin, "linux_amd64")
		if binary == nil {
			t.Fatal("not intact")
		}
		t.Cleanup(binary.Close)
		return binary
	}
	wantUntrusted := func(t *testing.T, err error, path, problemPrefix string) {
		t.Helper()
		var untrusted *teardown.UntrustedError
		if !errors.As(err, &untrusted) || untrusted.Path != path || len(untrusted.Problem) < len(problemPrefix) || untrusted.Problem[:len(problemPrefix)] != problemPrefix {
			t.Fatalf("err = %v, want %s %s...", err, path, problemPrefix)
		}
	}

	t.Run("accepts a store only its owner can write", func(t *testing.T) {
		if err := teardown.CheckOwnership(l, open(t)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refuses a group- or world-writable directory, pin or binary, naming it", func(t *testing.T) {
		binaryPath := teardown.BinaryPath(l, sum(content))
		for _, path := range []string{l.ControlHome, l.TeardownDir, l.PinPath, binaryPath} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			original := info.Mode().Perm()
			for _, bits := range []os.FileMode{0o020, 0o002} {
				if err := os.Chmod(path, original|bits); err != nil {
					t.Fatal(err)
				}
				wantUntrusted(t, teardown.CheckOwnership(l, open(t)), path, "can be written by its group or others (mode ")
			}
			if err := os.Chmod(path, original); err != nil {
				t.Fatal(err)
			}
		}
		if err := teardown.CheckOwnership(l, open(t)); err != nil {
			t.Fatalf("after restoring the modes: %v", err)
		}
	})

	t.Run("refuses anything owned by another user", func(t *testing.T) {
		defer teardown.SetEuidForTest(os.Geteuid() + 1)()
		wantUntrusted(t, teardown.CheckOwnership(l, open(t)), l.ControlHome, "is owned by uid ")
	})

	t.Run("refuses a symbolic link in the store, as one", func(t *testing.T) {
		linked := l
		linked.PinPath = filepath.Join(l.TeardownDir, "link.json")
		if err := os.Symlink(l.PinPath, linked.PinPath); err != nil {
			t.Fatal(err)
		}
		wantUntrusted(t, teardown.CheckOwnership(linked, open(t)), linked.PinPath, "is a symbolic link")
	})

	t.Run("refuses what is missing", func(t *testing.T) {
		missing := l
		missing.PinPath = filepath.Join(l.TeardownDir, "nope.json")
		wantUntrusted(t, teardown.CheckOwnership(missing, open(t)), missing.PinPath, "cannot be checked (")
	})
}

func TestMountNoExec(t *testing.T) {
	dir := registrytest.TempDir(t)
	// A temp directory is not a noexec mount on any machine the suite runs on;
	// what is pinned is that a path that does not exist yet is answered from its
	// nearest existing ancestor, and that the call does not error.
	if _, err := teardown.MountNoExec(filepath.Join(dir, "not", "yet")); err != nil {
		t.Fatal(err)
	}
}
