package registry_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/ports"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
)

const (
	wtC = "d3e4f506-3333-4444-8555-666677778888"
)

// smallRange is three ports, so a test can exhaust and wrap it.
var smallRange = layout.PortRange{Low: 30100, High: 30102}

// writePortsRecord writes a fixture record carrying ports.
func writePortsRecord(t *testing.T, controlHome, repositoryID, worktreeID, slug string, ports map[string]int) {
	t.Helper()
	if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: repositoryID, WorktreeID: worktreeID,
		Slug: slug, Domain: acmeDomain, Initialization: "complete", Ports: ports,
	}); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
}

// readPorts is the ports a record holds on disk, read through the store.
func readPorts(t *testing.T, store *registry.Store, worktreeID string) map[string]int {
	t.Helper()
	record, err := store.Find(worktreeID)
	if err != nil || record == nil {
		t.Fatalf("Find: %v, %v", record, err)
	}
	return record.Ports
}

func TestRecordPortsRoundTripThroughEveryWriter(t *testing.T) {
	ctx := context.Background()
	namespace := "dev.example"
	other := "dev.other"
	ports := map[string]int{"api": 30010, "web": 30011}
	allocate := func(_ []registry.RegisteredWorktree, preferred string) (string, error) {
		if preferred != "" {
			return preferred, nil
		}
		return "feature-login", nil
	}
	initialize := func() (registry.WorktreeInitialization, error) { return registry.Complete, nil }

	writers := []struct {
		name string
		run  func(t *testing.T, store *registry.Store)
	}{
		{"Ensure's fast path", func(t *testing.T, store *registry.Store) {
			if _, err := store.Ensure(ctx, wtA, acmeDomain, allocate, testWriter, initialize, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"Ensure's fast path reporting the recorded namespace", func(t *testing.T, store *registry.Store) {
			ensured, err := store.Ensure(ctx, wtA, acmeDomain, allocate, testWriter, initialize, &namespace)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(ensured.Ports, ports) {
				t.Fatalf("Ensure returned ports %v, want %v", ensured.Ports, ports)
			}
		}},
		{"Ensure's label rewrite", func(t *testing.T, store *registry.Store) {
			ensured, err := store.Ensure(ctx, wtA, acmeDomain, allocate, testWriter, initialize, &other)
			if err != nil {
				t.Fatal(err)
			}
			if ensured.LabelNamespace == nil || *ensured.LabelNamespace != other {
				t.Fatalf("the label rewrite did not happen: %+v", ensured)
			}
			if !reflect.DeepEqual(ensured.Ports, ports) {
				t.Fatalf("Ensure returned ports %v, want %v", ensured.Ports, ports)
			}
		}},
		{"Ensure's re-domain", func(t *testing.T, store *registry.Store) {
			if _, err := store.Ensure(ctx, wtA, "new.localhost", allocate, testWriter, initialize, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"MarkPending", func(t *testing.T, store *registry.Store) {
			if err := store.MarkPending(ctx, wtA, "feature-login", testWriter); err != nil {
				t.Fatal(err)
			}
		}},
		{"CompleteInitialization", func(t *testing.T, store *registry.Store) {
			if err := store.MarkPending(ctx, wtA, "feature-login", testWriter); err != nil {
				t.Fatal(err)
			}
			if err := store.CompleteInitialization(ctx, wtA, "feature-login", testWriter); err != nil {
				t.Fatal(err)
			}
		}},
		{"SyncPorts changing nothing", func(t *testing.T, store *registry.Store) {
			if _, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, registry.PortPlan{
				Services: []string{"api", "web"}, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{},
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"SyncPorts adding a service", func(t *testing.T, store *registry.Store) {
			result, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, registry.PortPlan{
				Services: []string{"api", "web", "worker"}, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Ports["api"] != 30010 || result.Ports["web"] != 30011 || result.Ports["worker"] == 0 {
				t.Fatalf("unexpected ports %v", result.Ports)
			}
			// The added service is the only difference; the caller below compares the rest.
			added := readPorts(t, store, wtA)
			delete(added, "worker")
			if !reflect.DeepEqual(added, ports) {
				t.Fatalf("ports after adding: %v", added)
			}
		}},
	}
	for _, writer := range writers {
		t.Run(writer.name, func(t *testing.T) {
			controlHome := claimedHome(t)
			if _, err := registrytest.WriteRecord(controlHome, registrytest.RecordJSON{
				Version: 1, RepositoryID: repoA, WorktreeID: wtA, Slug: "feature-login", Domain: acmeDomain,
				LabelNamespace: &namespace, Initialization: "complete", Ports: ports,
			}); err != nil {
				t.Fatal(err)
			}
			store := registry.NewStore(controlHome, repoA)
			writer.run(t, store)

			got := readPorts(t, store, wtA)
			if writer.name == "SyncPorts adding a service" {
				return // compared inside the writer
			}
			if !reflect.DeepEqual(got, ports) {
				t.Fatalf("ports after %s = %v, want %v", writer.name, got, ports)
			}
			requireRegistryFree(t, controlHome)
		})
	}

	t.Run("Ensure registering a new worktree writes a record without ports", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		if _, err := store.Ensure(ctx, wtA, acmeDomain, allocate, testWriter, initialize, nil); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(store.RecordPath(wtA))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "ports") {
			t.Fatalf("a record with no ports should not name the key:\n%s", data)
		}
	})
}

func TestRecordPortsShape(t *testing.T) {
	record := func(ports string) string {
		return `{"version":1,"repositoryId":"` + repoA + `","worktreeId":"` + wtA + `","slug":"x","domain":"d","initialization":"complete"` + ports + `}`
	}
	t.Run("a record without ports stays valid", func(t *testing.T) {
		controlHome := t.TempDir()
		dir := registry.RegistryDirectory(controlHome, repoA)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, wtA+".json"), []byte(record("")), 0o600); err != nil {
			t.Fatal(err)
		}
		found, err := registry.NewStore(controlHome, repoA).Find(wtA)
		if err != nil || found == nil || found.Ports != nil {
			t.Fatalf("Find = %+v, %v", found, err)
		}
	})

	for name, ports := range map[string]string{
		"a string port":        `,"ports":{"api":"30000"}`,
		"a fractional port":    `,"ports":{"api":30000.5}`,
		"port zero":            `,"ports":{"api":0}`,
		"a port above 65535":   `,"ports":{"api":65536}`,
		"an invalid service":   `,"ports":{"Api!":30000}`,
		"a list instead":       `,"ports":[30000]`,
		"a nested object":      `,"ports":{"api":{"port":1}}`,
		"an unknown extra key": `,"portz":{"api":30000}`,
	} {
		t.Run("reports "+name+" as an unrecognized record", func(t *testing.T) {
			controlHome := t.TempDir()
			dir := registry.RegistryDirectory(controlHome, repoA)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, wtA+".json"), []byte(record(ports)), 0o600); err != nil {
				t.Fatal(err)
			}
			_, problems, err := registry.NewStore(controlHome, repoA).Scan()
			if err != nil || len(problems) != 1 || !strings.Contains(problems[0].Reason, "unrecognized version or shape") {
				t.Fatalf("Scan problems = %+v, %v", problems, err)
			}
		})
	}

	t.Run("an empty ports object reads as none", func(t *testing.T) {
		controlHome := t.TempDir()
		dir := registry.RegistryDirectory(controlHome, repoA)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, wtA+".json"), []byte(record(`,"ports":{}`)), 0o600); err != nil {
			t.Fatal(err)
		}
		found, err := registry.NewStore(controlHome, repoA).Find(wtA)
		if err != nil || found == nil || found.Ports != nil {
			t.Fatalf("Find = %+v, %v", found, err)
		}
	})
}

func TestSyncPorts(t *testing.T) {
	ctx := context.Background()
	plan := func(probe ports.Probe, services ...string) registry.PortPlan {
		return registry.PortPlan{Services: services, Range: registrytest.PortRange, Probe: probe}
	}

	t.Run("allocates a port per service inside the range, the same ones again after the record is lost", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		store := registry.NewStore(controlHome, repoA)

		first, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, plan(registrytest.FreeProbe{}, "api", "web"))
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Ports) != 2 || len(first.Allocated) != 2 {
			t.Fatalf("result = %+v", first)
		}
		for service, port := range first.Ports {
			if !registrytest.PortRange.Contains(port) {
				t.Fatalf("%s: port %d is outside %v", service, port, registrytest.PortRange)
			}
		}
		if first.Ports["api"] == first.Ports["web"] {
			t.Fatalf("two services share port %d", first.Ports["api"])
		}
		requireRegistryFree(t, controlHome)

		// Lose the record's ports and allocate again: the hash start lands on the same ports.
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		second, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, plan(registrytest.FreeProbe{}, "api", "web"))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(second.Ports, first.Ports) {
			t.Fatalf("re-allocation gave %v, first was %v", second.Ports, first.Ports)
		}
	})

	t.Run("keeps a recorded port and writes nothing when nothing changes", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", map[string]int{"api": 30500})
		store := registry.NewStore(controlHome, repoA)
		before, err := os.ReadFile(store.RecordPath(wtA))
		if err != nil {
			t.Fatal(err)
		}
		// A busy probe and an unreadable record elsewhere prove neither is consulted.
		if err := os.MkdirAll(registry.RegistryDirectory(controlHome, repoB), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(registry.RegistryDirectory(controlHome, repoB), wtB+".json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		result, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter,
			plan(registrytest.FreeProbe{Busy: map[int]bool{30500: true}}, "api"))
		if err != nil {
			t.Fatal(err)
		}
		if result.Ports["api"] != 30500 || len(result.Allocated) != 0 || len(result.Released) != 0 {
			t.Fatalf("result = %+v", result)
		}
		after, err := os.ReadFile(store.RecordPath(wtA))
		if err != nil || string(after) != string(before) {
			t.Fatalf("the record changed:\n%s\nwas:\n%s", after, before)
		}
	})

	t.Run("skips ports in any registration of the control home, across repositories", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		probeStart, err := allocateOnce(t, controlHome, repoA, wtA, "feature-login", "api")
		if err != nil {
			t.Fatal(err)
		}
		// A second worktree, in another repository, already holds api's natural port.
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		writePortsRecord(t, controlHome, repoB, wtB, "other", map[string]int{"db": probeStart})

		store := registry.NewStore(controlHome, repoA)
		result, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, plan(registrytest.FreeProbe{}, "api"))
		if err != nil {
			t.Fatal(err)
		}
		want := next(probeStart)
		if result.Ports["api"] != want {
			t.Fatalf("api = %d, want %d (the port after %d, which another worktree holds)", result.Ports["api"], want, probeStart)
		}
	})

	t.Run("skips a port the probe reports busy and probes forward with wraparound", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		store := registry.NewStore(controlHome, repoA)
		small := registry.PortPlan{Services: []string{"api"}, Range: smallRange, Probe: registrytest.FreeProbe{}}
		natural, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, small)
		if err != nil {
			t.Fatal(err)
		}
		n := natural.Ports["api"]

		// Busy: the natural port, and (when it is the last) the wrap lands on the range's start.
		for busyCount := 1; busyCount <= 2; busyCount++ {
			writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
			busy := map[int]bool{}
			want := n
			for range busyCount {
				busy[want] = true
				want = smallNext(want)
			}
			small.Probe = registrytest.FreeProbe{Busy: busy}
			got, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, small)
			if err != nil {
				t.Fatal(err)
			}
			if got.Ports["api"] != want {
				t.Fatalf("with %d busy from %d: api = %d, want %d", busyCount, n, got.Ports["api"], want)
			}
		}
	})

	t.Run("refuses when every port in the range is taken or busy, changing nothing", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		store := registry.NewStore(controlHome, repoA)
		busy := registrytest.FreeProbe{Busy: map[int]bool{30100: true, 30101: true, 30102: true}}
		_, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter,
			registry.PortPlan{Services: []string{"api"}, Range: smallRange, Probe: busy})
		if err == nil || !strings.Contains(err.Error(), "No free port for api in 30100-30102") {
			t.Fatalf("err = %v", err)
		}
		if got := readPorts(t, store, wtA); got != nil {
			t.Fatalf("a refused allocation recorded %v", got)
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("refuses to allocate while a record cannot be read, naming it", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		dir := registry.RegistryDirectory(controlHome, repoB)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		bad := filepath.Join(dir, wtB+".json")
		if err := os.WriteFile(bad, []byte("{ not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		store := registry.NewStore(controlHome, repoA)
		_, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, plan(registrytest.FreeProbe{}, "api"))
		if err == nil || !strings.Contains(err.Error(), "Refusing to allocate a port while the registry has unreadable records") || !strings.Contains(err.Error(), bad) {
			t.Fatalf("err = %v", err)
		}
		if got := readPorts(t, store, wtA); got != nil {
			t.Fatalf("a refused allocation recorded %v", got)
		}
	})

	t.Run("keeps the port of a service that is no longer listed unless asked to release it", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", map[string]int{"api": 30500, "old": 30501})
		store := registry.NewStore(controlHome, repoA)
		result, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, plan(registrytest.FreeProbe{}, "api"))
		if err != nil || len(result.Released) != 0 || result.Ports["old"] != 30501 {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if got := readPorts(t, store, wtA); got["old"] != 30501 {
			t.Fatalf("recorded %v", got)
		}
	})

	t.Run("releases a service that is no longer listed when asked, without scanning", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", map[string]int{"api": 30500, "old": 30501})
		store := registry.NewStore(controlHome, repoA)
		released := plan(registrytest.FreeProbe{}, "api")
		released.Release = true
		result, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, released)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result.Released, map[string]int{"old": 30501}) || !reflect.DeepEqual(result.Ports, map[string]int{"api": 30500}) {
			t.Fatalf("result = %+v", result)
		}
		if got := readPorts(t, store, wtA); !reflect.DeepEqual(got, map[string]int{"api": 30500}) {
			t.Fatalf("recorded %v", got)
		}
	})

	t.Run("does not hand a just-released port to a service allocated in the same call", func(t *testing.T) {
		controlHome := claimedHome(t)
		natural, err := allocateOnce(t, controlHome, repoA, wtA, "feature-login", "api")
		if err != nil {
			t.Fatal(err)
		}
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", map[string]int{"old": natural})
		store := registry.NewStore(controlHome, repoA)
		released := plan(registrytest.FreeProbe{}, "api")
		released.Release = true
		result, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, released)
		if err != nil {
			t.Fatal(err)
		}
		if result.Ports["api"] == natural {
			t.Fatalf("api took %d, which old held until this call", natural)
		}
	})

	t.Run("skips the ports the caller reserves", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		natural, err := allocateOnce(t, controlHome, repoA, wtA, "feature-login", "api")
		if err != nil {
			t.Fatal(err)
		}
		store := registry.NewStore(controlHome, repoA)
		reserved := plan(registrytest.FreeProbe{}, "api")
		reserved.Reserved = []int{natural}
		result, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, reserved)
		if err != nil || result.Ports["api"] != next(natural) {
			t.Fatalf("api = %d, %v; want %d", result.Ports["api"], err, next(natural))
		}
	})

	t.Run("reallocates to a port other than the one held", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", map[string]int{"api": 30500, "web": 30501})
		store := registry.NewStore(controlHome, repoA)
		result, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, registry.PortPlan{
			Services: []string{"api", "web"}, Reallocate: []string{"api"}, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Ports["api"] == 30500 || result.Moved["api"] != 30500 || result.Ports["web"] != 30501 || len(result.Allocated) != 1 {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("refuses a registration another slug now owns", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "renamed", nil)
		store := registry.NewStore(controlHome, repoA)
		_, err := store.SyncPorts(ctx, wtA, "feature-login", testWriter, plan(registrytest.FreeProbe{}, "api"))
		if err == nil || !strings.Contains(err.Error(), "no longer owns registration") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("stops probing when the context is cancelled", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		store := registry.NewStore(controlHome, repoA)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := store.SyncPorts(cancelled, wtA, "feature-login", testWriter, plan(registrytest.FreeProbe{}, "api")); err == nil {
			t.Fatal("expected an error")
		}
	})
}

// allocateOnce allocates service's port in a scratch copy of the record and
// returns it, leaving the record with no ports: the port the hash picks
// first, to build a collision around.
func allocateOnce(t *testing.T, controlHome, repositoryID, worktreeID, slug, service string) (int, error) {
	t.Helper()
	writePortsRecord(t, controlHome, repositoryID, worktreeID, slug, nil)
	store := registry.NewStore(controlHome, repositoryID)
	result, err := store.SyncPorts(context.Background(), worktreeID, slug, testWriter, registry.PortPlan{
		Services: []string{service}, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{},
	})
	if err != nil {
		return 0, err
	}
	writePortsRecord(t, controlHome, repositoryID, worktreeID, slug, nil)
	return result.Ports[service], nil
}

// next is the port after port in registrytest.PortRange, wrapping.
func next(port int) int {
	if port == registrytest.PortRange.High {
		return registrytest.PortRange.Low
	}
	return port + 1
}

// smallNext is the port after port in smallRange, wrapping.
func smallNext(port int) int {
	if port == smallRange.High {
		return smallRange.Low
	}
	return port + 1
}

func TestReleasePorts(t *testing.T) {
	ctx := context.Background()
	t.Run("drops every port and keeps the rest of the record", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", map[string]int{"api": 30500})
		store := registry.NewStore(controlHome, repoA)
		if err := store.ReleasePorts(ctx, wtA, "feature-login", testWriter); err != nil {
			t.Fatal(err)
		}
		record, err := store.Find(wtA)
		if err != nil || record == nil || record.Ports != nil || record.Slug != "feature-login" {
			t.Fatalf("record = %+v, %v", record, err)
		}
		data, err := os.ReadFile(store.RecordPath(wtA))
		if err != nil || strings.Contains(string(data), "ports") {
			t.Fatalf("the ports key remains:\n%s", data)
		}
	})

	t.Run("is idempotent, and a missing record is success", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		store := registry.NewStore(controlHome, repoA)
		for range 2 {
			if err := store.ReleasePorts(ctx, wtA, "feature-login", testWriter); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.ReleasePorts(ctx, wtB, "other", testWriter); err != nil {
			t.Fatalf("a missing record: %v", err)
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("refuses a registration another slug now owns, keeping its ports", func(t *testing.T) {
		controlHome := claimedHome(t)
		writePortsRecord(t, controlHome, repoA, wtA, "renamed", map[string]int{"api": 30500})
		store := registry.NewStore(controlHome, repoA)
		if err := store.ReleasePorts(ctx, wtA, "feature-login", testWriter); err == nil {
			t.Fatal("expected an error")
		}
		if got := readPorts(t, store, wtA); got["api"] != 30500 {
			t.Fatalf("ports = %v", got)
		}
	})
}

func TestPortConflicts(t *testing.T) {
	a := registry.RegisteredWorktree{WorktreeID: wtA, Ports: map[string]int{"api": 30000, "web": 30001}}
	b := registry.RegisteredWorktree{WorktreeID: wtB, Ports: map[string]int{"api": 30001, "db": 30002}}
	c := registry.RegisteredWorktree{WorktreeID: wtC, Ports: map[string]int{"web": 30001}}
	conflicts := registry.PortConflicts([]registry.RegisteredWorktree{c, a, b})
	if len(conflicts) != 1 || conflicts[0].Port != 30001 || len(conflicts[0].Claims) != 3 {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	if got := conflicts[0].Claims[0].String(); got != "web of "+wtA {
		t.Fatalf("first claim = %q", got)
	}
	if got := registry.PortConflicts([]registry.RegisteredWorktree{a}); len(got) != 0 {
		t.Fatalf("one worktree, no conflict: %+v", got)
	}
}

// Compile-time proof that the fixture probe is a ports.Probe.
var _ ports.Probe = registrytest.FreeProbe{}

// jsonKeys is the top-level keys of a record, in file order.
func jsonKeys(t *testing.T, data []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, token.(string))
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestRecordKeyOrderWithPorts(t *testing.T) {
	controlHome := claimedHome(t)
	writePortsRecord(t, controlHome, repoA, wtA, "feature-login", map[string]int{"web": 30011, "api": 30010})
	store := registry.NewStore(controlHome, repoA)
	if err := store.MarkPending(context.Background(), wtA, "feature-login", testWriter); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.RecordPath(wtA))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(jsonKeys(t, data), ","); got != "version,repositoryId,worktreeId,slug,domain,initialization,ports" {
		t.Fatalf("keys = %s", got)
	}
	if !strings.Contains(string(data), `"api": 30010,`) {
		t.Fatalf("ports are not written in sorted order:\n%s", data)
	}
}

// collidingWorktrees mints n worktree IDs whose hash start for "api" in a
// range of size ports is one and the same, so concurrent allocations all want
// the same port first.
func collidingWorktrees(n, size int) []string {
	var ids []string
	want := -1
	for len(ids) < n {
		id := newTestUUID()
		start := ports.Start(size, id, "api")
		if want < 0 {
			want = start
		}
		if start == want {
			ids = append(ids, id)
		}
	}
	return ids
}

func TestSyncPortsConcurrently(t *testing.T) {
	const workers = 6
	smallRange := layout.PortRange{Low: 30200, High: 30200 + workers} // workers+1 ports
	controlHome := claimedHome(t)
	worktrees := collidingWorktrees(workers, smallRange.Size())
	repositories := []string{repoA, repoB}
	stores := make([]*registry.Store, workers)
	for i, id := range worktrees {
		repository := repositories[i%len(repositories)]
		writePortsRecord(t, controlHome, repository, id, "wt-"+strconv.Itoa(i), nil)
		stores[i] = registry.NewStore(controlHome, repository)
	}

	results := make([]registry.PortResult, workers)
	var group errgroup.Group
	for i := range worktrees {
		group.Go(func() error {
			result, err := stores[i].SyncPorts(context.Background(), worktrees[i], "wt-"+strconv.Itoa(i), testWriter, registry.PortPlan{
				Services: []string{"api"}, Range: smallRange, Probe: registrytest.FreeProbe{},
			})
			results[i] = result
			return err
		})
	}
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}

	seen := map[int]int{}
	for i, result := range results {
		port := result.Ports["api"]
		if !smallRange.Contains(port) {
			t.Fatalf("worker %d got %d, outside %v", i, port, smallRange)
		}
		if other, dup := seen[port]; dup {
			t.Fatalf("workers %d and %d both got port %d", other, i, port)
		}
		seen[port] = i
		if onDisk := readPorts(t, stores[i], worktrees[i]); !reflect.DeepEqual(onDisk, result.Ports) {
			t.Fatalf("worker %d: record on disk %v, call returned %v", i, onDisk, result.Ports)
		}
	}
	requireRegistryFree(t, controlHome)
}
