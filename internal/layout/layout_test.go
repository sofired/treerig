package layout_test

import (
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/layout"
)

const (
	repository = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	worktree   = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"
)

// mapGetenv adapts a map to layout.Getenv, so tests can fix an environment
// without touching the process's real one.
func mapGetenv(values map[string]string) layout.Getenv {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func TestControlHomeResolution(t *testing.T) {
	t.Run("prefers an explicit override, then the environment, then the default", func(t *testing.T) {
		withEnv := mapGetenv(map[string]string{brand.HomeEnv: "/env/home"})

		got, err := layout.ResolveControlHome(withEnv, "/explicit")
		if err != nil || got != "/explicit" {
			t.Fatalf("got %q, %v", got, err)
		}

		got, err = layout.ResolveControlHome(withEnv, "")
		if err != nil || got != "/env/home" {
			t.Fatalf("got %q, %v", got, err)
		}

		got, err = layout.ResolveControlHome(mapGetenv(nil), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasSuffix(got, brand.DefaultHomeDir) {
			t.Fatalf("expected a path ending in %s, got %q", brand.DefaultHomeDir, got)
		}
	})
}

// collectPathFields walks a layout struct (including its embedded structs)
// and returns every string field's value, the way the TypeScript test's
// `Object.entries(layout)` sees every property regardless of which layer of
// the (spread-composed) layout object declared it.
func collectPathFields(v reflect.Value, out map[string]string) {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Struct:
			collectPathFields(fv, out)
		case reflect.String:
			out[field.Name] = fv.String()
		}
	}
}

func TestWorktreeLayout(t *testing.T) {
	t.Run("keeps every path inside the control home", func(t *testing.T) {
		l := layout.WorktreeLayout("/control", repository, worktree)
		paths := map[string]string{}
		collectPathFields(reflect.ValueOf(l), paths)
		if len(paths) == 0 {
			t.Fatalf("expected at least one path field")
		}
		for name, value := range paths {
			relative, err := filepath.Rel("/control", value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				t.Fatalf("%s = %q is outside the control home (relative: %q)", name, value, relative)
			}
		}
	})

	t.Run("separates locks from state so lock directories can be tiny and long-lived", func(t *testing.T) {
		l := layout.WorktreeLayout("/control", repository, worktree)
		if want := filepath.Join("/control", "locks", "worktrees", repository, worktree); l.LockPath != want {
			t.Fatalf("LockPath = %q, want %q", l.LockPath, want)
		}
		if want := l.LockPath + ".dev"; l.DevelopmentLockPath != want {
			t.Fatalf("DevelopmentLockPath = %q, want %q", l.DevelopmentLockPath, want)
		}
		if want := filepath.Join("/control", "repositories", repository, "instances", worktree); l.Home != want {
			t.Fatalf("Home = %q, want %q", l.Home, want)
		}
		if want := filepath.Join("/control", "repositories", repository, "worktrees", worktree+".json"); l.RecordPath != want {
			t.Fatalf("RecordPath = %q, want %q", l.RecordPath, want)
		}
		if want := filepath.Join("/control", "repositories", repository, "instances", ".deleting-"+worktree); l.QuarantinePath != want {
			t.Fatalf("QuarantinePath = %q, want %q", l.QuarantinePath, want)
		}
	})
}

func TestDerivedNames(t *testing.T) {
	t.Run("produces a stable, short, prefixed Compose project name", func(t *testing.T) {
		name := layout.ComposeProjectName(repository, worktree)
		if again := layout.ComposeProjectName(repository, worktree); name != again {
			t.Fatalf("expected a stable name, got %q then %q", name, again)
		}
		pattern := regexp.MustCompile("^" + brand.Name + "-[0-9a-f]{24}$")
		if !pattern.MatchString(name) {
			t.Fatalf("name %q does not match %s", name, pattern)
		}
		if other := layout.ComposeProjectName(repository, "other"); other == name {
			t.Fatalf("expected a different worktree to produce a different name")
		}
	})

	t.Run("routes a worktree under the project domain", func(t *testing.T) {
		if got := layout.WorktreeHost("feature-login", "acme.localhost"); got != "feature-login.acme.localhost" {
			t.Fatalf("got %q", got)
		}
	})
}
