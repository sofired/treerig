package backend

import (
	"os"
	"testing"

	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/worktree"
)

// TestOverrideCorpus drives this package's own overrideDocument with the
// exact inputs tools/oracle/override-corpus.ts recorded, decoded with this
// package's own ordered JSON decoder (the same one prepare uses on a real
// `docker compose config` document), and compares the encoded result byte
// for byte against what the TypeScript build's overrideDocument produced —
// or, for a refusal, its exact message.
func TestOverrideCorpus(t *testing.T) {
	data, err := os.ReadFile("testdata/override-corpus.json")
	if err != nil {
		t.Fatalf("reading testdata/override-corpus.json: %v (regenerate with tools/oracle/override-corpus.ts)", err)
	}
	root, err := decodeOrdered(data)
	if err != nil {
		t.Fatalf("decoding testdata/override-corpus.json: %v", err)
	}
	cases, ok := root.([]any)
	if !ok {
		t.Fatalf("expected a top-level array, got %T", root)
	}
	if len(cases) < 15 {
		t.Fatalf("only %d corpus cases, expected at least 15; regenerate testdata/override-corpus.json", len(cases))
	}

	for _, raw := range cases {
		obj, ok := raw.(*jsonKV)
		if !ok {
			t.Fatalf("expected an object case, got %T", raw)
		}
		name := mustString(t, obj, "name")
		t.Run(name, func(t *testing.T) {
			ref := decodeCorpusRef(t, obj)
			domain := mustString(t, obj, "domain")
			labelNamespace := mustString(t, obj, "labelNamespace")
			routerNetwork := mustString(t, obj, "routerNetwork")
			routes := decodeCorpusRoutes(t, obj)
			configValue, _ := obj.get("config")
			configObj, ok := configValue.(*jsonKV)
			if !ok {
				t.Fatalf("expected config to be an object, got %T", configValue)
			}
			services, err := orderedSection(configObj, "services", "compose.yaml")
			if err != nil {
				t.Fatalf("services: %v", err)
			}
			volumes, err := orderedSection(configObj, "volumes", "compose.yaml")
			if err != nil {
				t.Fatalf("volumes: %v", err)
			}
			networks, err := orderedSection(configObj, "networks", "compose.yaml")
			if err != nil {
				t.Fatalf("networks: %v", err)
			}

			rt := NewCompose(Options{
				ComposeFile: "/work/acme/docker/compose.yaml", ProjectRoot: "/work/acme",
				ConfigFile: "/work/acme/berth.toml", ProjectName: "acme",
				ControlHome: "/control", Domain: domain, LabelNamespace: labelNamespace,
				Routes: routes, RouterNetwork: routerNetwork, Logger: logger.Silent{},
			})

			document, buildErr := rt.overrideDocument(ref, services, volumes, networks)
			if wantError, hasError := obj.get("error"); hasError {
				wantMessage, _ := wantError.(string)
				if buildErr == nil {
					t.Fatalf("expected an error %q, got a document instead", wantMessage)
				}
				if buildErr.Error() != wantMessage {
					t.Fatalf("got error %q, want %q", buildErr.Error(), wantMessage)
				}
				return
			}
			if buildErr != nil {
				t.Fatalf("overrideDocument: %v", buildErr)
			}
			gotBytes, err := marshalOverrideDocument(document)
			if err != nil {
				t.Fatalf("marshalOverrideDocument: %v", err)
			}
			wantBytes := mustString(t, obj, "bytes")
			if string(gotBytes) != wantBytes {
				t.Fatalf("bytes mismatch:\n got: %q\nwant: %q", gotBytes, wantBytes)
			}
		})
	}
}

// mustString reads key from obj as a string, failing the test if it is
// missing or of another type.
func mustString(t *testing.T, obj *jsonKV, key string) string {
	t.Helper()
	value, ok := obj.get(key)
	if !ok {
		t.Fatalf("missing %q", key)
	}
	s, ok := value.(string)
	if !ok {
		t.Fatalf("%q is not a string: %T", key, value)
	}
	return s
}

// decodeCorpusRef decodes a corpus case's "ref" object into a worktree.Ref.
func decodeCorpusRef(t *testing.T, obj *jsonKV) worktree.Ref {
	t.Helper()
	value, ok := obj.get("ref")
	if !ok {
		t.Fatalf("missing ref")
	}
	refObj, ok := value.(*jsonKV)
	if !ok {
		t.Fatalf("ref is not an object: %T", value)
	}
	return worktree.Ref{
		RepositoryID: mustString(t, refObj, "repositoryId"),
		WorktreeID:   mustString(t, refObj, "worktreeId"),
		Slug:         mustString(t, refObj, "slug"),
	}
}

// decodeCorpusRoutes decodes a corpus case's "routes" array into RouteSpecs,
// returning nil when the case has none.
func decodeCorpusRoutes(t *testing.T, obj *jsonKV) []RouteSpec {
	t.Helper()
	value, ok := obj.get("routes")
	if !ok {
		return nil
	}
	array, ok := value.([]any)
	if !ok {
		t.Fatalf("routes is not an array: %T", value)
	}
	routes := make([]RouteSpec, 0, len(array))
	for _, item := range array {
		routeObj, ok := item.(*jsonKV)
		if !ok {
			t.Fatalf("route is not an object: %T", item)
		}
		route := RouteSpec{
			Service:   mustString(t, routeObj, "service"),
			Subdomain: mustString(t, routeObj, "subdomain"),
		}
		if pathValue, present := routeObj.get("path"); present {
			path, ok := pathValue.(string)
			if !ok {
				t.Fatalf("route.path is not a string: %T", pathValue)
			}
			route.Path = &path
		}
		if portValue, present := routeObj.get("port"); present {
			port, ok := numberToInt(portValue)
			if !ok {
				t.Fatalf("route.port is not a number: %v", portValue)
			}
			route.Port = &port
		}
		routes = append(routes, route)
	}
	return routes
}
