//go:build integration

package shared_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run/osrun"
	"github.com/sofired/treerig/internal/shared"
	"github.com/sofired/treerig/internal/text"
)

// realCheckout writes a shared file with the given volume source and env_file
// into a fresh checkout under its own parent directory, and returns the stack
// a command run in it builds, over the real `docker compose`, and the parent.
func realCheckout(t *testing.T, volume string, log logger.Logger, envFile string) (*shared.Stack, string) {
	t.Helper()
	if os.Getenv("TREERIG_TEST_SKIP_DOCKER") == "1" {
		t.Skip("Docker tests are skipped (TREERIG_TEST_SKIP_DOCKER=1)")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skipf("docker compose is not available: %v", err)
	}
	parent := registrytest.TempDir(t)
	dir := filepath.Join(parent, "acme", "docker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "services:\n  db:\n    image: postgres:17\n    env_file: " + envFile + "\n    volumes:\n      - " + volume + ":/data\n"
	for name, content := range map[string]string{"compose.shared.yaml": body, "db.env": "X=1\n", "../../outside.env": "Y=1\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stack := shared.New(shared.Options{
		RepositoryID: repository, LabelNamespace: ns, ControlHome: filepath.Join(parent, "control"),
		ComposeFile: filepath.Join(dir, "compose.shared.yaml"), ProjectName: "acme",
		WorktreeRoots: []string{filepath.Join(parent, "acme")}, Environ: os.Environ(),
		Runner: osrun.New(), Logger: log,
	})
	return stack, parent
}

// TestSharedFileHashAcrossCheckoutsWithRealCompose reads one shared file from
// two checkouts with the real Compose, which resolves paths itself.
func TestSharedFileHashAcrossCheckoutsWithRealCompose(t *testing.T) {
	t.Run("an absolute bind source gives both checkouts the same hash", func(t *testing.T) {
		var hashes []string
		for range 2 {
			stack, _ := realCheckout(t, "/srv/shared/db", logger.Silent{}, "./db.env")
			definition, err := stack.ReadDefinition(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			hash, err := definition.Hash("db")
			if err != nil {
				t.Fatal(err)
			}
			hashes = append(hashes, hash)
		}
		if hashes[0] != hashes[1] {
			t.Fatalf("hashes differ: %v", hashes)
		}
	})

	t.Run("refuses a relative bind source that climbs out of the checkout, in these words", func(t *testing.T) {
		stack, parent := realCheckout(t, "../../shared-data", logger.Silent{}, "./db.env")
		_, err := stack.ReadDefinition(context.Background())
		want := "docker/compose.shared.yaml reads differently in each checkout: services.db.volumes[0].source resolves to \"" +
			text.OneLine(parent+"/shared-data", 80) + "\" here, from a relative path. " +
			"Use an absolute path outside every worktree, a named volume, or a value that is the same in every checkout."
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v\nwant %s", err, want)
		}
	})

	t.Run("an env_file that climbs out of the directory skips the check with a warning", func(t *testing.T) {
		log := &recorder{}
		stack, _ := realCheckout(t, "../../shared-data", log, "../../outside.env")
		if _, err := stack.ReadDefinition(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(log.warnings) != 1 || !strings.Contains(log.warnings[0], "reads the same from every checkout (") ||
			!strings.Contains(log.warnings[0], "outside.env") {
			t.Fatalf("warnings = %q", log.warnings)
		}
	})
}

// TestSharedDefinitionIgnoresWorktreeURLVariablesWithRealCompose reads a
// shared file that references ${TREERIG_URL} and ${TREERIG_SERVICE_URL_WEB}
// with the real Compose, under inherited environments that differ only in
// those variables: the definition hash must be the same, because the shared
// definition must not vary by worktree (#177).
func TestSharedDefinitionIgnoresWorktreeURLVariablesWithRealCompose(t *testing.T) {
	if os.Getenv("TREERIG_TEST_SKIP_DOCKER") == "1" {
		t.Skip("Docker tests are skipped (TREERIG_TEST_SKIP_DOCKER=1)")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skipf("docker compose is not available: %v", err)
	}
	parent := registrytest.TempDir(t)
	dir := filepath.Join(parent, "acme", "docker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "services:\n  db:\n    image: postgres:17\n    environment:\n" +
		"      PUBLIC_URL: \"${TREERIG_URL:-unset}\"\n      WEB_URL: \"${TREERIG_SERVICE_URL_WEB:-unset}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.shared.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	hashUnder := func(inherited ...string) string {
		t.Helper()
		stack := shared.New(shared.Options{
			RepositoryID: repository, LabelNamespace: ns, ControlHome: filepath.Join(parent, "control"),
			ComposeFile: filepath.Join(dir, "compose.shared.yaml"), ProjectName: "acme",
			WorktreeRoots: []string{filepath.Join(parent, "acme")},
			Environ:       append(os.Environ(), inherited...), Runner: osrun.New(), Logger: logger.Silent{},
		})
		definition, err := stack.ReadDefinition(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		hash, err := definition.Hash("db")
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	t.Run("the hash is the same with and without inherited URL variables", func(t *testing.T) {
		bare := hashUnder("TREERIG_URL=", "TREERIG_SERVICE_URL_WEB=")
		withURLs := hashUnder("TREERIG_URL=http://one.acme.localhost:30080", "TREERIG_SERVICE_URL_WEB=http://one.acme.localhost:30080")
		other := hashUnder("TREERIG_URL=http://two.acme.localhost", "TREERIG_SERVICE_URL_WEB=http://api.two.acme.localhost")
		if bare != withURLs || bare != other {
			t.Fatalf("the shared definition varies with the caller's URL variables:\n%s\n%s\n%s", bare, withURLs, other)
		}
	})
}
