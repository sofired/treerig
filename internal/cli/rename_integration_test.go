//go:build integration

package cli_test

// `worktree rename`, through the real binary (#11): the argument contract,
// and the whole story the issue asks for — after a rename `up`, `down` and
// the overview work under the new slug, with nothing left under the old. The
// fake docker is the one `up` is tested with, which reports no containers, so
// the refusal that depends on them is pinned by internal/commands' own tests.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/router"
)

// renameRun runs the fixture's binary with args and returns its streams and exit code.
func renameRun(t *testing.T, f upFixture, binary string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := f.command(binary, args...)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatal(err)
	}
	return out.String(), errOut.String(), code
}

// routeFiles is the contents of every route file under the router's dynamic
// directory, by file name.
func routeFiles(t *testing.T, f upFixture) map[string]string {
	t.Helper()
	fragments, err := router.ListFragments(f.routerState)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, fragment := range fragments {
		data, err := os.ReadFile(fragment.Path)
		if err != nil {
			t.Fatal(err)
		}
		found[filepath.Base(fragment.Path)] = string(data)
	}
	return found
}

func TestWorktreeRenameThroughTheBinary(t *testing.T) {
	binary := buildBinary(t)

	t.Run("renames the registration and index, and up and down work at the new slug with nothing left under the old", func(t *testing.T) {
		f := newUpFixture(t)
		if _, stderr, code := renameRun(t, f, binary, "up"); code != 0 {
			t.Fatalf("up: exit %d\n%s", code, stderr)
		}
		if _, stderr, code := renameRun(t, f, binary, "down"); code != 0 {
			t.Fatalf("down: exit %d\n%s", code, stderr)
		}

		stdout, stderr, code := renameRun(t, f, binary, "worktree", "rename", "login")

		if code != 0 || stdout != "" ||
			!strings.Contains(stderr, "[treerig] Renamed feature-login.acme.localhost to login.acme.localhost. Run 'treerig up' to start it there.\n") {
			t.Fatalf("rename: exit %d\nstdout: %q\nstderr: %q", code, stdout, stderr)
		}
		list, _, _ := renameRun(t, f, binary, "list")
		var slugs []string
		for _, row := range strings.Split(strings.TrimSpace(list), "\n")[1:] {
			slugs = append(slugs, strings.Fields(strings.TrimLeft(row, "* "))[0])
		}
		if !slices.Equal(slugs, []string{"login"}) {
			t.Fatalf("list = %q, want one worktree, named login", list)
		}

		urls, stderr, code := renameRun(t, f, binary, "up")
		const want = "api   http://login.acme.localhost:18080\nmail  http://mail.login.acme.localhost:18080\n"
		if code != 0 || urls != want {
			t.Fatalf("up: exit %d\nstdout: %q, want %q\nstderr: %s", code, urls, want, stderr)
		}
		routes := routeFiles(t, f)
		if len(routes) != 2 {
			t.Fatalf("route files = %v", routes)
		}
		for name, body := range routes {
			if !strings.Contains(body, "login.acme.localhost") || strings.Contains(body, "feature-login") {
				t.Fatalf("%s = %s", name, body)
			}
		}
		page, err := os.ReadFile(f.routerState.IndexFile)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(page), "login.acme.localhost") || strings.Contains(string(page), "feature-login") {
			t.Fatalf("the index page still names the old slug or not the new one")
		}

		if _, stderr, code := renameRun(t, f, binary, "down"); code != 0 || !strings.Contains(stderr, "login.acme.localhost is not running. Nothing to stop.") {
			t.Fatalf("down: exit %d\n%s", code, stderr)
		}
		if routes := routeFiles(t, f); len(routes) != 0 {
			t.Fatalf("down left route files: %v", routes)
		}
		requireReleased(t, f.worktreeLock(t))
		requireReleased(t, f.routerState.LockPath)
	})

	t.Run("removes the route files a stopped worktree left behind, which would route the old host", func(t *testing.T) {
		f := newUpFixture(t)
		if _, stderr, code := renameRun(t, f, binary, "up"); code != 0 {
			t.Fatalf("up: exit %d\n%s", code, stderr)
		}
		// No `down`: the fake docker reports no containers, as after a manual stop.
		if len(routeFiles(t, f)) != 2 {
			t.Fatalf("route files before = %v", routeFiles(t, f))
		}
		if _, stderr, code := renameRun(t, f, binary, "worktree", "rename", "login"); code != 0 {
			t.Fatalf("rename: exit %d\n%s", code, stderr)
		}
		if routes := routeFiles(t, f); len(routes) != 0 {
			t.Fatalf("route files after = %v", routes)
		}
	})

	t.Run("refuses the reserved slug, a malformed one, and a missing argument, with exit 1 and nothing on stdout", func(t *testing.T) {
		f := newUpFixture(t)
		if _, stderr, code := renameRun(t, f, binary, "up"); code != 0 {
			t.Fatalf("up: exit %d\n%s", code, stderr)
		}
		for _, tc := range []struct {
			argv []string
			want string
		}{
			{[]string{"shared"}, `[treerig] ERROR: The slug "shared" is reserved: a repository's shared services are routed beneath it.`},
			{[]string{"Not A Label"}, `[treerig] ERROR: The slug "Not A Label" is not a valid hostname label`},
			{nil, `[treerig] ERROR: "treerig worktree rename" needs the new slug as its one argument.`},
			{[]string{"a", "b"}, `[treerig] ERROR: "treerig worktree rename" takes at most 1 argument, got 2: "a" "b"`},
		} {
			stdout, stderr, code := renameRun(t, f, binary, append([]string{"worktree", "rename"}, tc.argv...)...)
			if code != 1 || stdout != "" || !strings.HasPrefix(stderr, tc.want) {
				t.Fatalf("%v: exit %d\nstdout: %q\nstderr: %q\nwant prefix %q", tc.argv, code, stdout, stderr, tc.want)
			}
		}
	})

	t.Run("an unregistered checkout is told to run up first", func(t *testing.T) {
		f := newUpFixture(t)

		_, stderr, code := renameRun(t, f, binary, "worktree", "rename", "login")

		if code != 1 || !strings.HasPrefix(stderr, "[treerig] ERROR: This worktree is not registered. Run 'treerig up' first.") {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
	})
}
