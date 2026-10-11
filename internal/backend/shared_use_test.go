package backend

// The labels that tell a sweep which of a worktree's containers use a shared
// service (docs/design/routing.md, decision 22), and the network_mode cases a
// shared container cannot be connected beside.

import (
	"encoding/json"
	"strings"
	"testing"
)

// sharedUseCompose is a *Compose whose web and worker services use mailpit
// and web alone uses objects.
func sharedUseCompose() *Compose {
	return NewCompose(Options{
		ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
		ConfigFile: "/work/acme/berth.toml", ProjectName: "acme",
		ControlHome: "/control", Domain: "acme.localhost", LabelNamespace: "dev.berth",
		SharedUse: map[string][]string{"web": {"objects", "mailpit"}, "worker": {"mailpit"}},
	})
}

// labelsOf is the labels the override gives a service.
func labelsOf(t *testing.T, document *jsonKV, service string) map[string]string {
	t.Helper()
	encoded, err := marshalOrdered(document)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Services map[string]struct {
			Labels   map[string]string `json:"labels"`
			Networks json.RawMessage   `json:"networks"`
			Ports    json.RawMessage   `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatal(err)
	}
	entry, ok := parsed.Services[service]
	if !ok {
		t.Fatalf("no %s in %s", service, encoded)
	}
	if entry.Networks != nil || entry.Ports != nil {
		t.Errorf("%s gained networks or ports: %s", service, encoded)
	}
	return entry.Labels
}

func TestOverrideStampsSharedUse(t *testing.T) {
	services, volumes, networks := decodeModel(t, `{"services":{"web":{},"worker":{},"db":{}}}`)
	document, err := sharedUseCompose().overrideDocument(networkFixtureRef, services, volumes, networks)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("labels each using service with a shared-use label per shared service, besides its ownership", func(t *testing.T) {
		web := labelsOf(t, document, "web")
		for _, name := range []string{"dev.berth.managed", "dev.berth.repository-uuid", "dev.berth.worktree-uuid", "dev.berth.shared-use.mailpit", "dev.berth.shared-use.objects"} {
			if web[name] == "" {
				t.Errorf("web lacks %s: %v", name, web)
			}
		}
		if web["dev.berth.shared-use.mailpit"] != "true" {
			t.Errorf("web = %v", web)
		}
		worker := labelsOf(t, document, "worker")
		if worker["dev.berth.shared-use.mailpit"] != "true" || worker["dev.berth.shared-use.objects"] != "" {
			t.Errorf("worker = %v", worker)
		}
	})

	t.Run("leaves a service that uses none with the ownership labels alone", func(t *testing.T) {
		db := labelsOf(t, document, "db")
		for name := range db {
			if strings.Contains(name, "shared") {
				t.Errorf("db carries %s", name)
			}
		}
		if len(db) != 3 {
			t.Errorf("db = %v", db)
		}
	})

	t.Run("is the same bytes every time, whatever order the services were listed in", func(t *testing.T) {
		again, err := sharedUseCompose().overrideDocument(networkFixtureRef, services, volumes, networks)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := marshalOverrideDocument(document)
		b, _ := marshalOverrideDocument(again)
		if string(a) != string(b) {
			t.Fatalf("two builds differ:\n%s\n%s", a, b)
		}
	})

	t.Run("a worktree that uses no shared service gets the override it always did", func(t *testing.T) {
		plain := NewCompose(Options{
			ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
			ConfigFile: "/work/acme/berth.toml", ProjectName: "acme", ControlHome: "/control", Domain: "acme.localhost", LabelNamespace: "dev.berth",
		})
		document, err := plain.overrideDocument(networkFixtureRef, services, volumes, networks)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := marshalOverrideDocument(document)
		if strings.Contains(string(encoded), "shared") {
			t.Fatalf("override:\n%s", encoded)
		}
	})

	t.Run("a used-by service the model lacks is skipped, not an error: an inactive profile", func(t *testing.T) {
		only, volumes, networks := decodeModel(t, `{"services":{"web":{}}}`)
		if _, err := sharedUseCompose().overrideDocument(networkFixtureRef, only, volumes, networks); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCannotJoinNetwork(t *testing.T) {
	for mode, want := range map[string]bool{
		"host": true, "none": true, "service:db": true, "container:abc": true,
		"bridge": false, "": false, "default": false, "my-net": false,
	} {
		if got := cannotJoinNetwork(mode); got != want {
			t.Errorf("cannotJoinNetwork(%q) = %v, want %v", mode, got, want)
		}
	}
}
