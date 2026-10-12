package shared

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/registry/registrytest"
)

// TestDecodeDefinition covers the forms of Compose's JSON that the stateful
// fake never prints.
func TestDecodeDefinition(t *testing.T) {
	t.Run("reads depends_on written as a plain list", func(t *testing.T) {
		d, err := decodeDefinition(`{"services":{"a":{"depends_on":["b"]},"b":{"depends_on":["c"]},"c":{}}}`, "f")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(d.Closure("a"), ","); got != "a,b,c" {
			t.Fatalf("closure = %s", got)
		}
	})

	t.Run("survives a dependency cycle and a dependency the file lacks", func(t *testing.T) {
		d, err := decodeDefinition(`{"services":{"a":{"depends_on":{"b":{},"ghost":{}}},"b":{"depends_on":{"a":{}}}}}`, "f")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(d.Closure("a"), ","); got != "a,b" {
			t.Fatalf("closure = %s", got)
		}
	})

	t.Run("keeps numbers as Compose printed them", func(t *testing.T) {
		a, _ := decodeDefinition(`{"services":{"a":{"cpus":1.0,"big":12345678901234567890}}}`, "f")
		b, _ := decodeDefinition(`{"services":{"a":{"cpus":1,"big":12345678901234567890}}}`, "f")
		ha, _ := a.Hash("a")
		hb, _ := b.Hash("a")
		if ha == hb {
			t.Fatal("1.0 and 1 hash alike")
		}
	})

	t.Run("does not escape HTML in the canonical form", func(t *testing.T) {
		d, _ := decodeDefinition(`{"services":{"a":{"command":"a<b&c>d"}}}`, "f")
		if _, err := d.Hash("a"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("rejects JSON it cannot read", func(t *testing.T) {
		if _, err := decodeDefinition(`{`, "f"); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("refuses a config or secret read from inside a worktree", func(t *testing.T) {
		d, _ := decodeDefinition(`{"services":{"a":{}},"configs":{"c":{"file":"/work/acme/c.conf"}},"secrets":{"s":{"file":"/srv/s"}}}`, "f")
		err := d.refuseUnsuitable("f", []string{"/work/acme"}, nil)
		if err == nil || !strings.Contains(err.Error(), "/work/acme/c.conf") {
			t.Fatalf("err = %v", err)
		}
		if err := d.refuseUnsuitable("f", []string{"/elsewhere"}, nil); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestInsideAnyFollowsLinks pins that a bind source reached through a
// symbolic link into a worktree is inside it.
func TestInsideAnyFollowsLinks(t *testing.T) {
	base := registrytest.TempDir(t)
	root := filepath.Join(base, "work")
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "alias")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, inside := insideAny([]string{root}, filepath.Join(link, "data")); !inside {
		t.Error("a path through a link into the worktree was taken for outside it")
	}
	if _, inside := insideAny([]string{link}, filepath.Join(root, "data")); !inside {
		t.Error("a worktree reached through a link did not contain its own directory")
	}
	if _, inside := insideAny([]string{root}, filepath.Join(base, "elsewhere", "data")); inside {
		t.Error("a sibling directory was taken for inside the worktree")
	}
	if _, inside := insideAny([]string{root}, ""); inside {
		t.Error("an empty source was taken for inside")
	}
}

// TestHashCoversWhatTheClosureReferences pins the inputs of the definition
// hash: the closure's services, and the top-level volumes, networks, configs
// and secrets they use — and nothing else.
func TestHashCoversWhatTheClosureReferences(t *testing.T) {
	const base = `{"services":{"a":{"image":"i","volumes":[{"type":"volume","source":"data","target":"/d"}],"networks":{"back":null},"configs":["c"],"secrets":[{"source":"s"}]},"b":{"image":"j"}},
		"volumes":{"data":{"name":"p_data","driver":"local"},"other":{"name":"p_other"}},
		"networks":{"back":{"name":"p_back"},"unused":{"name":"p_unused"}},
		"configs":{"c":{"content":"x"}},"secrets":{"s":{"environment":"S"}}}`
	hash := func(t *testing.T, document string) string {
		t.Helper()
		d, err := decodeDefinition(document, "f")
		if err != nil {
			t.Fatal(err)
		}
		h, err := d.Hash("a")
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	before := hash(t, base)
	for name, change := range map[string]string{
		"a referenced volume":  `"driver":"local"`,
		"a referenced network": `"name":"p_back"`,
		"a referenced config":  `"content":"x"`,
		"a referenced secret":  `"environment":"S"`,
	} {
		if hash(t, strings.Replace(base, change, change+`,"labels":{"changed":"1"}`, 1)) == before {
			t.Errorf("changing %s left the hash alone", name)
		}
	}
	for name, change := range map[string]string{
		"a volume nothing uses":  `"name":"p_other"`,
		"a network nothing uses": `"name":"p_unused"`,
		"a service outside it":   `"image":"j"`,
	} {
		if hash(t, strings.Replace(base, change, change+`,"labels":{"changed":"1"}`, 1)) != before {
			t.Errorf("changing %s moved the hash", name)
		}
	}
}

// TestClosureFollowsEverythingAServiceNeeds pins what pulls a service into
// another's closure besides depends_on.
func TestClosureFollowsEverythingAServiceNeeds(t *testing.T) {
	d, err := decodeDefinition(`{"services":{
		"a":{"links":["b:alias"],"volumes_from":["c:ro","container:x"],"network_mode":"service:d"},
		"b":{},"c":{},"d":{"depends_on":["e"]},"e":{},"f":{}}}`, "f")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(d.Closure("a"), ","); got != "a,b,c,d,e" {
		t.Fatalf("closure = %s", got)
	}
}

// TestAHashThatCannotBeComputedFailsTheStartAndWritesNothing pins that an
// unhashable definition (a value JSON cannot encode) is an error from the
// override and from Up, not a label with an empty hash, and that no override
// file is left behind.
func TestAHashThatCannotBeComputedFailsTheStartAndWritesNothing(t *testing.T) {
	bad := Definition{services: map[string]map[string]any{"a": {"image": "i", "broken": make(chan int)}}}
	control := registrytest.TempDir(t)
	stack := &Stack{o: Options{LabelNamespace: "dev.berth", RepositoryID: "repo", ControlHome: control}}

	if _, err := stack.overrideDocument(bad, nil, ""); err == nil {
		t.Fatal("overrideDocument printed a label for a hash it could not compute")
	}
	err := stack.Up(context.Background(), UpRequest{Definition: bad, Services: []string{"a"}})
	if err == nil || strings.Contains(err.Error(), "defines no service") {
		t.Fatalf("Up err = %v, want the hash failure", err)
	}
	var found []string
	if walkErr := filepath.WalkDir(control, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".yaml") {
			found = append(found, path)
		}
		return err
	}); walkErr != nil {
		t.Fatal(walkErr)
	}
	if len(found) != 0 {
		t.Fatalf("override files written: %v", found)
	}
}
