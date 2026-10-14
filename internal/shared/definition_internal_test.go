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

// TestFirstDifference pins what two reads of a shared file are said to differ
// in: equal outputs do not, and a type mismatch, a list of another length, a
// key only one read has and a changed leaf each do, at their path.
func TestFirstDifference(t *testing.T) {
	cases := []struct {
		name        string
		here, other string
		path, value string
		differs     bool
	}{
		{"equal", `{"a":{"b":[1,"x"]}}`, `{"a":{"b":[1,"x"]}}`, "", "", false},
		{"a changed leaf", `{"a":{"b":"/x"}}`, `{"a":{"b":"/y"}}`, "a.b", `"/x"`, true},
		{"a type mismatch", `{"a":{"b":"1"}}`, `{"a":{"b":1}}`, "a.b", `"1"`, true},
		{"an object against a string", `{"a":{"b":1}}`, `{"a":"s"}`, "a", `{"b":1}`, true},
		{"lists of another length", `{"a":["x","y"]}`, `{"a":["x"]}`, "a", `["x","y"]`, true},
		{"a key only the second read has", `{"a":1}`, `{"a":1,"b":2}`, "b", "nothing", true},
		{"a key only the first read has", `{"a":1,"b":2}`, `{"a":1}`, "b", "2", true},
		{"null against a value", `{"a":null}`, `{"a":"x"}`, "a", "nothing", true},
		{"two differences report the first key", `{"b":"1","a":"1"}`, `{"b":"2","a":"2"}`, "a", `"1"`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			found, differs, err := firstDifference(c.here, c.other)
			if err != nil {
				t.Fatal(err)
			}
			if differs != c.differs || found.path != c.path || (differs && found.describeHere() != c.value) {
				t.Fatalf("got differs=%v path=%q value=%s; want differs=%v path=%q value=%s", differs, found.path, found.describeHere(), c.differs, c.path, c.value)
			}
		})
	}

	t.Run("reports output it cannot decode", func(t *testing.T) {
		if _, _, err := firstDifference(`{`, `{}`); err == nil {
			t.Fatal("no error for the first read")
		}
		if _, _, err := firstDifference(`{}`, `{`); err == nil {
			t.Fatal("no error for the second read")
		}
	})
}

// TestCause pins that a cause is named only when substituting the scratch
// directory for the real one reproduces the second read.
func TestCause(t *testing.T) {
	const realDir, probe = "/work/acme/docker", "/scratch/a/b/c/d"
	const neutral = "a path or variable that depends on the checkout"
	cases := []struct {
		name, pwd, here, other, want string
	}{
		{"PWD itself", "/home/dev/acme", "/home/dev/acme", probe, "${PWD}"},
		{"PWD with a suffix", "/home/dev/acme", "/home/dev/acme/data", probe + "/data", "${PWD}"},
		{"a path in the project directory", "/home/dev", realDir + "/data", probe + "/data", "a relative path"},
		{"a path that climbs out of it", "/home/dev", "/work/shared-data", "/scratch/a/b/shared-data", "a relative path"},
		{"a command run from the project directory", realDir, realDir, probe, neutral},
		{"PWD an ancestor of the project directory, value the project directory", "/work", realDir, probe, "a relative path"},
		{"a command string", "/home/dev", "echo /work/acme", "echo /somewhere/else", neutral},
		{"no PWD, the scratch directory itself", "", realDir, probe, "a relative path"},
		{"no PWD, nothing to substitute", "", "alice", "bob", neutral},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := difference{path: "p", here: c.here, other: c.other}
			if got := d.cause(realDir, probe, c.pwd); got != c.want {
				t.Fatalf("cause = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("bounds a long value, keeping the quoting", func(t *testing.T) {
		long := strings.Repeat("k", 500)
		for name, value := range map[string]any{"a string": long, "a list": []any{long}} {
			got := difference{here: value}.describeHere()
			if len([]rune(got)) > describeLimit+4 || !strings.Contains(got, "…") {
				t.Fatalf("%s: describeHere is %d characters: %s", name, len(got), got)
			}
		}
		if got := (difference{here: "a\x1bb"}).describeHere(); strings.Contains(got, "\x1b") {
			t.Fatalf("describeHere = %q", got)
		}
	})

	t.Run("describes an empty value", func(t *testing.T) {
		if got := (difference{here: ""}).describeHere(); got != "an empty value" {
			t.Fatalf("describeHere = %q", got)
		}
	})
}
