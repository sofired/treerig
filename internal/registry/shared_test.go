package registry_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/ports"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
)

// writeSharedRecord writes a fixture shared-service record by hand, in the
// shape recordForShared writes.
func writeSharedRecord(t *testing.T, controlHome, repositoryID, name string, hostPorts map[int]int) {
	t.Helper()
	var entries []string
	for container, host := range hostPorts {
		entries = append(entries, `"`+strconv.Itoa(container)+`": `+strconv.Itoa(host))
	}
	writeSharedFile(t, controlHome, repositoryID, name, `{"version":1,"name":"`+name+`","domain":"acme.localhost","ports":{`+strings.Join(entries, ",")+`}}`)
}

// writeSharedFile writes raw as one shared-service record file.
func writeSharedFile(t *testing.T, controlHome, repositoryID, name, raw string) string {
	t.Helper()
	l := layout.SharedLayout(controlHome, repositoryID)
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := l.RecordPath(name)
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// syncShared runs SyncSharedPorts for name with the test range and a probe
// that reports every port free but busy.
func syncShared(t *testing.T, store *registry.Store, name string, declared []int, mutate func(*registry.SharedPortPlan)) registry.SharedPortResult {
	t.Helper()
	plan := registry.SharedPortPlan{Name: name, Domain: "acme.localhost", Ports: declared, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{}}
	if mutate != nil {
		mutate(&plan)
	}
	result, err := store.SyncSharedPorts(context.Background(), testWriter, plan)
	if err != nil {
		t.Fatalf("SyncSharedPorts: %v", err)
	}
	return result
}

func TestSyncSharedPorts(t *testing.T) {
	t.Run("pins a host port per declared container port, inside the range and distinct", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		result := syncShared(t, store, "mailpit", []int{8025, 1025, 1110}, nil)
		if len(result.Ports) != 3 || len(result.Allocated) != 3 {
			t.Fatalf("result = %+v", result)
		}
		seen := map[int]bool{}
		for container, host := range result.Ports {
			if !registrytest.PortRange.Contains(host) || seen[host] {
				t.Fatalf("container port %d got host port %d (range %v, seen %v)", container, host, registrytest.PortRange, seen)
			}
			seen[host] = true
		}
		requireRegistryFree(t, controlHome)

		record, err := store.FindShared("mailpit")
		if err != nil || record == nil {
			t.Fatalf("FindShared = %+v, %v", record, err)
		}
		if !reflect.DeepEqual(record.Ports, result.Ports) || record.Domain != "acme.localhost" || record.RepositoryID != repoA || record.Name != "mailpit" {
			t.Fatalf("record = %+v, result = %v", record, result.Ports)
		}
	})

	t.Run("keys the starting port by repository, service and container port, so a lost record lands on the same ports", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		first := syncShared(t, store, "mailpit", []int{8025, 1025}, nil)
		if err := os.Remove(layout.SharedLayout(controlHome, repoA).RecordPath("mailpit")); err != nil {
			t.Fatal(err)
		}
		again := syncShared(t, store, "mailpit", []int{8025, 1025}, nil)
		if !reflect.DeepEqual(first.Ports, again.Ports) {
			t.Fatalf("re-allocation gave %v, first was %v", again.Ports, first.Ports)
		}
		// Pick starts at ports.Start(size, repository, "shared:<name>:<container port>").
		start := registrytest.PortRange.Low + ports.Start(registrytest.PortRange.Size(), repoA, "shared:mailpit:8025")
		if first.Ports[8025] != start {
			t.Fatalf("8025 -> %d, want the hash start %d", first.Ports[8025], start)
		}
		other := registry.NewStore(controlHome, repoB)
		if elsewhere := syncShared(t, other, "mailpit", []int{8025}, nil); elsewhere.Ports[8025] == first.Ports[8025] {
			t.Fatalf("two repositories' mailpit share host port %d", first.Ports[8025])
		}
	})

	t.Run("writes nothing, and does not scan, when every declared port already has one", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		writeSharedRecord(t, controlHome, repoA, "mailpit", map[int]int{8025: 30500, 1025: 30501})
		before, err := os.ReadFile(layout.SharedLayout(controlHome, repoA).RecordPath("mailpit"))
		if err != nil {
			t.Fatal(err)
		}
		// An unreadable record elsewhere and a busy probe prove neither is consulted.
		writeSharedFile(t, controlHome, repoB, "other", "{ not json")
		result := syncShared(t, store, "mailpit", []int{8025, 1025}, func(p *registry.SharedPortPlan) {
			p.Probe = registrytest.FreeProbe{Busy: map[int]bool{30500: true}}
		})
		if result.Ports[8025] != 30500 || result.Ports[1025] != 30501 || len(result.Allocated) != 0 {
			t.Fatalf("result = %+v", result)
		}
		after, _ := os.ReadFile(layout.SharedLayout(controlHome, repoA).RecordPath("mailpit"))
		if string(after) != string(before) {
			t.Fatalf("the record changed:\n%s\nwas:\n%s", after, before)
		}
	})

	t.Run("writes no record for a service that declares nothing", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		result := syncShared(t, store, "worker", nil, nil)
		if len(result.Ports) != 0 {
			t.Fatalf("result = %+v", result)
		}
		if record, err := store.FindShared("worker"); err != nil || record != nil {
			t.Fatalf("FindShared = %+v, %v", record, err)
		}
	})

	t.Run("keeps a port that is no longer declared unless asked to prune", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		writeSharedRecord(t, controlHome, repoA, "mailpit", map[int]int{8025: 30500, 1025: 30501})
		kept := syncShared(t, store, "mailpit", []int{8025}, nil)
		if kept.Ports[1025] != 30501 || len(kept.Released) != 0 {
			t.Fatalf("kept = %+v", kept)
		}
		pruned := syncShared(t, store, "mailpit", []int{8025}, func(p *registry.SharedPortPlan) { p.Prune = true })
		if !reflect.DeepEqual(pruned.Released, map[int]int{1025: 30501}) || !reflect.DeepEqual(pruned.Ports, map[int]int{8025: 30500}) {
			t.Fatalf("pruned = %+v", pruned)
		}
		if record, _ := store.FindShared("mailpit"); !reflect.DeepEqual(record.Ports, map[int]int{8025: 30500}) {
			t.Fatalf("record = %+v", record)
		}
	})

	t.Run("a port pruned in the same call is not handed to a new one", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		natural := registrytest.PortRange.Low + ports.Start(registrytest.PortRange.Size(), repoA, "shared:mailpit:2525")
		writeSharedRecord(t, controlHome, repoA, "mailpit", map[int]int{1025: natural})
		result := syncShared(t, store, "mailpit", []int{2525}, func(p *registry.SharedPortPlan) { p.Prune = true })
		if result.Ports[2525] == natural {
			t.Fatalf("2525 took %d, which 1025 held until this call", natural)
		}
	})

	t.Run("reallocates to a port other than the one held, keeping the others", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		writeSharedRecord(t, controlHome, repoA, "mailpit", map[int]int{8025: 30500, 1025: 30501})
		result := syncShared(t, store, "mailpit", []int{8025, 1025}, func(p *registry.SharedPortPlan) { p.Reallocate = []int{8025} })
		if result.Ports[8025] == 30500 || result.Moved[8025] != 30500 || result.Ports[1025] != 30501 || len(result.Allocated) != 1 {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("keeps the domain it was first allocated under", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		syncShared(t, store, "mailpit", []int{8025}, nil)
		syncShared(t, store, "mailpit", []int{8025, 1025}, func(p *registry.SharedPortPlan) { p.Domain = "renamed.localhost" })
		record, err := store.FindShared("mailpit")
		if err != nil || record.Domain != "acme.localhost" {
			t.Fatalf("record = %+v, %v", record, err)
		}
	})

	t.Run("skips ports any registration holds, and a worktree skips what a shared service holds", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		natural := registrytest.PortRange.Low + ports.Start(registrytest.PortRange.Size(), repoA, "shared:mailpit:8025")
		writePortsRecord(t, controlHome, repoB, wtB, "other", map[string]int{"db": natural})
		result := syncShared(t, store, "mailpit", []int{8025}, nil)
		if result.Ports[8025] != next(natural) {
			t.Fatalf("8025 -> %d, want %d (after the worktree's %d)", result.Ports[8025], next(natural), natural)
		}

		// And the other way: a worktree whose natural port a shared service holds.
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		worktreeNatural := registrytest.PortRange.Low + ports.Start(registrytest.PortRange.Size(), wtA, "api")
		writeSharedRecord(t, controlHome, repoA, "db", map[int]int{5432: worktreeNatural})
		got, err := store.SyncPorts(context.Background(), wtA, "feature-login", testWriter, registry.PortPlan{
			Services: []string{"api"}, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Ports["api"] != next(worktreeNatural) {
			t.Fatalf("api -> %d, want %d (after the shared service's %d)", got.Ports["api"], next(worktreeNatural), worktreeNatural)
		}
	})

	t.Run("refuses while a shared record cannot be read, naming it, and records nothing", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		bad := writeSharedFile(t, controlHome, repoB, "other", "{ not json")
		_, err := store.SyncSharedPorts(context.Background(), testWriter, registry.SharedPortPlan{
			Name: "mailpit", Domain: "acme.localhost", Ports: []int{8025}, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{},
		})
		if err == nil || !strings.Contains(err.Error(), "Refusing to allocate a port while the registry has unreadable records") || !strings.Contains(err.Error(), bad) {
			t.Fatalf("err = %v", err)
		}
		if record, _ := store.FindShared("mailpit"); record != nil {
			t.Fatalf("a refused allocation recorded %+v", record)
		}
		// A worktree's allocation refuses on it too: it may hold a port.
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", nil)
		_, err = store.SyncPorts(context.Background(), wtA, "feature-login", testWriter, registry.PortPlan{
			Services: []string{"api"}, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{},
		})
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Fatalf("worktree allocation err = %v", err)
		}
	})

	t.Run("refuses a name that is not a shared service's, and writes nothing", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		for _, name := range []string{"../x", "a/b", "", "UPPER"} {
			_, err := store.SyncSharedPorts(context.Background(), testWriter, registry.SharedPortPlan{
				Name: name, Domain: "acme.localhost", Ports: []int{8025}, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{},
			})
			if err == nil {
				t.Errorf("%q: no error", name)
			}
		}
		var written []string
		if err := filepath.WalkDir(controlHome, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && filepath.Base(path) != ".berth-control-home" {
				written = append(written, path)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(written) != 0 {
			t.Fatalf("files written: %v", written)
		}
	})

	t.Run("names the container port when the range is exhausted", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		_, err := store.SyncSharedPorts(context.Background(), testWriter, registry.SharedPortPlan{
			Name: "mailpit", Domain: "acme.localhost", Ports: []int{8025}, Range: smallRange,
			Probe: registrytest.FreeProbe{Busy: map[int]bool{30100: true, 30101: true, 30102: true}},
		})
		if err == nil || !strings.Contains(err.Error(), "No free port for mailpit:8025 in 30100-30102") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("serialises concurrent allocations across worktrees and shared services", func(t *testing.T) {
		const workers = 6
		small := layout.PortRange{Low: 30300, High: 30300 + workers}
		controlHome := claimedHome(t)
		var group sync.WaitGroup
		results := make([]int, workers)
		errs := make([]error, workers)
		for i := range workers {
			group.Add(1)
			go func() {
				defer group.Done()
				if i%2 == 0 {
					id := newTestUUID()
					if err := tryWritePortsRecord(controlHome, repoA, id, "wt-"+strconv.Itoa(i), nil); err != nil {
						errs[i] = err
						return
					}
					result, err := registry.NewStore(controlHome, repoA).SyncPorts(context.Background(), id, "wt-"+strconv.Itoa(i), testWriter, registry.PortPlan{
						Services: []string{"api"}, Range: small, Probe: registrytest.FreeProbe{},
					})
					results[i], errs[i] = result.Ports["api"], err
					return
				}
				result, err := registry.NewStore(controlHome, repoB).SyncSharedPorts(context.Background(), testWriter, registry.SharedPortPlan{
					Name: "svc" + strconv.Itoa(i), Domain: "acme.localhost", Ports: []int{1025}, Range: small, Probe: registrytest.FreeProbe{},
				})
				results[i], errs[i] = result.Ports[1025], err
			}()
		}
		group.Wait()
		seen := map[int]int{}
		for i, port := range results {
			if errs[i] != nil {
				t.Fatalf("worker %d: %v", i, errs[i])
			}
			if other, dup := seen[port]; dup {
				t.Fatalf("workers %d and %d both got port %d", other, i, port)
			}
			seen[port] = i
		}
		requireRegistryFree(t, controlHome)
	})
}

func TestRemoveSharedRecord(t *testing.T) {
	t.Run("deletes the record, and is success when there is none", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		writeSharedRecord(t, controlHome, repoA, "mailpit", map[int]int{8025: 30500})
		for range 2 {
			if err := store.RemoveSharedRecord(context.Background(), testWriter, "mailpit"); err != nil {
				t.Fatal(err)
			}
		}
		if record, err := store.FindShared("mailpit"); err != nil || record != nil {
			t.Fatalf("FindShared = %+v, %v", record, err)
		}
		requireRegistryFree(t, controlHome)
	})

	t.Run("leaves a sibling and a worktree registration alone", func(t *testing.T) {
		controlHome := claimedHome(t)
		store := registry.NewStore(controlHome, repoA)
		writeSharedRecord(t, controlHome, repoA, "mailpit", map[int]int{8025: 30500})
		writeSharedRecord(t, controlHome, repoA, "db", map[int]int{5432: 30501})
		writePortsRecord(t, controlHome, repoA, wtA, "feature-login", map[string]int{"api": 30502})
		if err := store.RemoveSharedRecord(context.Background(), testWriter, "mailpit"); err != nil {
			t.Fatal(err)
		}
		if record, _ := store.FindShared("db"); record == nil {
			t.Error("a sibling's record went with it")
		}
		if got := readPorts(t, store, wtA); got["api"] != 30502 {
			t.Errorf("the worktree's ports = %v", got)
		}
	})
}

func TestSharedRecordShape(t *testing.T) {
	valid := `{"version":1,"name":"mailpit","domain":"acme.localhost","ports":{"1025":30001,"8025":30000}}`
	t.Run("reads a valid record, ignoring the override file beside it", func(t *testing.T) {
		controlHome := t.TempDir()
		writeSharedFile(t, controlHome, repoA, "mailpit", valid)
		l := layout.SharedLayout(controlHome, repoA)
		if err := os.WriteFile(l.OverridePath, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		records, problems, err := registry.NewStore(controlHome, repoA).SharedRecords()
		if err != nil || len(problems) != 0 || len(records) != 1 {
			t.Fatalf("records = %+v, problems = %+v, err = %v", records, problems, err)
		}
		if want := (registry.SharedRecord{RepositoryID: repoA, Name: "mailpit", Domain: "acme.localhost", Ports: map[int]int{1025: 30001, 8025: 30000}}); !reflect.DeepEqual(records[0], want) {
			t.Fatalf("record = %+v, want %+v", records[0], want)
		}
		if records[0].HostPort(8025) != 30000 || records[0].HostPort(9) != 0 {
			t.Fatalf("HostPort misreads %+v", records[0])
		}
	})

	for name, raw := range map[string]string{
		"a version other than 1":          `{"version":2,"name":"mailpit","domain":"d","ports":{}}`,
		"no version":                      `{"name":"mailpit","domain":"d","ports":{}}`,
		"a name that is not a slug":       `{"version":1,"name":"Mail!","domain":"d","ports":{}}`,
		"a name other than the file's":    `{"version":1,"name":"other","domain":"d","ports":{}}`,
		"an empty domain":                 `{"version":1,"name":"mailpit","domain":"","ports":{}}`,
		"no ports":                        `{"version":1,"name":"mailpit","domain":"d"}`,
		"null ports":                      `{"version":1,"name":"mailpit","domain":"d","ports":null}`,
		"a string host port":              `{"version":1,"name":"mailpit","domain":"d","ports":{"1025":"30000"}}`,
		"a fractional host port":          `{"version":1,"name":"mailpit","domain":"d","ports":{"1025":30000.5}}`,
		"host port zero":                  `{"version":1,"name":"mailpit","domain":"d","ports":{"1025":0}}`,
		"a host port above 65535":         `{"version":1,"name":"mailpit","domain":"d","ports":{"1025":65536}}`,
		"a container port of zero":        `{"version":1,"name":"mailpit","domain":"d","ports":{"0":30000}}`,
		"a container port with a sign":    `{"version":1,"name":"mailpit","domain":"d","ports":{"+25":30000}}`,
		"a container port that is a word": `{"version":1,"name":"mailpit","domain":"d","ports":{"smtp":30000}}`,
		"one host port used twice":        `{"version":1,"name":"mailpit","domain":"d","ports":{"25":30000,"26":30000}}`,
		"an unknown key":                  `{"version":1,"name":"mailpit","domain":"d","ports":{},"extra":1}`,
		"a list of ports":                 `{"version":1,"name":"mailpit","domain":"d","ports":[30000]}`,
		"not JSON":                        `{ nope`,
	} {
		t.Run("reports "+name+" as a problem, not a record", func(t *testing.T) {
			controlHome := t.TempDir()
			writeSharedFile(t, controlHome, repoA, "mailpit", raw)
			records, problems, err := registry.NewStore(controlHome, repoA).SharedRecords()
			if err != nil || len(records) != 0 || len(problems) != 1 {
				t.Fatalf("records = %+v, problems = %+v, err = %v", records, problems, err)
			}
			if !strings.Contains(problems[0].Reason, "Shared-service record") {
				t.Fatalf("reason = %q", problems[0].Reason)
			}
		})
	}

	t.Run("reports a symlink as a problem and never follows it", func(t *testing.T) {
		controlHome := t.TempDir()
		real := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(real, []byte(valid), 0o600); err != nil {
			t.Fatal(err)
		}
		l := layout.SharedLayout(controlHome, repoA)
		if err := os.MkdirAll(l.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, l.RecordPath("mailpit")); err != nil {
			t.Fatal(err)
		}
		_, problems, err := registry.NewStore(controlHome, repoA).SharedRecords()
		if err != nil || len(problems) != 1 || !strings.Contains(problems[0].Reason, "not a symbolic link") {
			t.Fatalf("problems = %+v, err = %v", problems, err)
		}
	})

	t.Run("scans every repository, skipping directories that are not repositories", func(t *testing.T) {
		controlHome := t.TempDir()
		writeSharedRecord(t, controlHome, repoA, "mailpit", map[int]int{8025: 30000})
		writeSharedRecord(t, controlHome, repoB, "db", map[int]int{5432: 30001})
		if err := os.MkdirAll(filepath.Join(layout.ControlLayout(controlHome).RepositoriesDir, "not-a-uuid", "shared"), 0o700); err != nil {
			t.Fatal(err)
		}
		records, problems, err := registry.ScanSharedRecords(controlHome)
		if err != nil || len(problems) != 0 || len(records) != 2 {
			t.Fatalf("records = %+v, problems = %+v, err = %v", records, problems, err)
		}
	})

	t.Run("a control home with no repositories has none", func(t *testing.T) {
		records, problems, err := registry.ScanSharedRecords(t.TempDir())
		if err != nil || records != nil || problems != nil {
			t.Fatalf("records = %+v, problems = %+v, err = %v", records, problems, err)
		}
	})
}

func TestSharedRecordKeyOrder(t *testing.T) {
	controlHome := claimedHome(t)
	store := registry.NewStore(controlHome, repoA)
	syncShared(t, store, "mailpit", []int{8025, 1025}, nil)
	data, err := os.ReadFile(layout.SharedLayout(controlHome, repoA).RecordPath("mailpit"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(jsonKeys(t, data), ","); got != "version,name,domain,ports" {
		t.Fatalf("keys = %s\n%s", got, data)
	}
}

func TestSharedRoutedPort(t *testing.T) {
	routed := func(port int) func(*registry.SharedPortPlan) {
		return func(plan *registry.SharedPortPlan) { plan.Routed = &port }
	}

	t.Run("records the web UI's container port with the pins, and the record names its host", func(t *testing.T) {
		store := registry.NewStore(claimedHome(t), repoA)
		result := syncShared(t, store, "mailpit", []int{8025, 1025}, routed(8025))
		record, err := store.FindShared("mailpit")
		if err != nil || record == nil || record.Routed != 8025 {
			t.Fatalf("record = %+v, %v", record, err)
		}
		if record.RoutedHostPort() != result.Ports[8025] || record.Host() != "mailpit.shared.acme.localhost" {
			t.Errorf("RoutedHostPort = %d, Host = %q", record.RoutedHostPort(), record.Host())
		}
	})

	t.Run("a service with no web UI has no host", func(t *testing.T) {
		store := registry.NewStore(claimedHome(t), repoA)
		syncShared(t, store, "smtp", []int{1025}, routed(0))
		record, _ := store.FindShared("smtp")
		if record.Routed != 0 || record.Host() != "" || record.RoutedHostPort() != 0 {
			t.Errorf("record = %+v", record)
		}
	})

	t.Run("a nil Routed leaves it as it is, and 0 clears it", func(t *testing.T) {
		store := registry.NewStore(claimedHome(t), repoA)
		syncShared(t, store, "mailpit", []int{8025}, routed(8025))
		syncShared(t, store, "mailpit", []int{8025}, nil)
		if record, _ := store.FindShared("mailpit"); record.Routed != 8025 {
			t.Fatalf("routed = %d after a plan that said nothing", record.Routed)
		}
		syncShared(t, store, "mailpit", []int{8025}, routed(0))
		if record, _ := store.FindShared("mailpit"); record.Routed != 0 {
			t.Fatalf("routed = %d after clearing", record.Routed)
		}
	})

	t.Run("changing only the routed port rewrites the record and moves no host port", func(t *testing.T) {
		store := registry.NewStore(claimedHome(t), repoA)
		first := syncShared(t, store, "mailpit", []int{8025, 1025}, routed(0))
		second := syncShared(t, store, "mailpit", nil, routed(8025))
		if len(second.Allocated) != 0 || !reflect.DeepEqual(second.Ports, first.Ports) {
			t.Fatalf("second = %+v, first = %+v", second, first)
		}
		if record, _ := store.FindShared("mailpit"); record.Routed != 8025 {
			t.Errorf("record = %+v", record)
		}
	})

	t.Run("pruning the routed port's pin clears it", func(t *testing.T) {
		store := registry.NewStore(claimedHome(t), repoA)
		syncShared(t, store, "mailpit", []int{8025, 1025}, routed(8025))
		syncShared(t, store, "mailpit", []int{1025}, func(plan *registry.SharedPortPlan) { plan.Prune = true })
		if record, _ := store.FindShared("mailpit"); record.Routed != 0 {
			t.Errorf("record = %+v", record)
		}
	})

	t.Run("refuses a routed port the service does not pin", func(t *testing.T) {
		store := registry.NewStore(claimedHome(t), repoA)
		port := 9999
		_, err := store.SyncSharedPorts(context.Background(), testWriter, registry.SharedPortPlan{
			Name: "mailpit", Domain: "acme.localhost", Ports: []int{1025}, Routed: &port, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{}})
		if err == nil || !strings.Contains(err.Error(), "container port 9999 is not one it declares") {
			t.Fatalf("err = %v", err)
		}
		if record, _ := store.FindShared("mailpit"); record != nil {
			t.Errorf("a record was written: %+v", record)
		}
	})

	t.Run("a record whose routed port is not pinned is a shape error", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeSharedFile(t, controlHome, repoA, "mailpit", `{"version":1,"name":"mailpit","domain":"acme.localhost","ports":{"1025":30000},"routed":8025}`)
		if _, err := registry.NewStore(controlHome, repoA).FindShared("mailpit"); err == nil {
			t.Fatal("a routed port that is not pinned was accepted")
		}
	})
}

func TestSharedRoutedConflict(t *testing.T) {
	routed := func(port int) func(*registry.SharedPortPlan) {
		return func(plan *registry.SharedPortPlan) { plan.Routed = &port }
	}
	conflict := func(t *testing.T, store *registry.Store, mutate func(*registry.SharedPortPlan)) error {
		t.Helper()
		plan := registry.SharedPortPlan{Name: "mailpit", Domain: "acme.localhost", Ports: []int{8025}, Range: registrytest.PortRange, Probe: registrytest.FreeProbe{}}
		mutate(&plan)
		_, err := store.SyncSharedPorts(context.Background(), testWriter, plan)
		return err
	}

	t.Run("the second repository on a domain is refused, naming the first by its repository", func(t *testing.T) {
		controlHome := claimedHome(t)
		syncShared(t, registry.NewStore(controlHome, repoA), "mailpit", []int{8025}, routed(8025))
		err := conflict(t, registry.NewStore(controlHome, repoB), routed(8025))
		if err == nil {
			t.Fatal("two repositories routed mailpit.shared.acme.localhost")
		}
		if !strings.Contains(err.Error(), "Rename one of the two services.") || strings.Contains(err.Error(), "different domains") {
			t.Errorf("advice = %q", err)
		}
		for _, want := range []string{"mailpit.shared.acme.localhost", "repository " + repoA} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err, want)
			}
		}
		if record, _ := registry.NewStore(controlHome, repoB).FindShared("mailpit"); record != nil {
			t.Errorf("the refused service was recorded: %+v", record)
		}
	})

	t.Run("names the first by the hostname of its registered worktree when it has one", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeMinimalRecord(t, controlHome, repoA, wtA, "feature-a", "acme.localhost", "complete")
		syncShared(t, registry.NewStore(controlHome, repoA), "mailpit", []int{8025}, routed(8025))
		err := conflict(t, registry.NewStore(controlHome, repoB), routed(8025))
		if err == nil || !strings.Contains(err.Error(), "the repository of the worktree feature-a.acme.localhost") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("compares domains as hostnames do", func(t *testing.T) {
		controlHome := claimedHome(t)
		syncShared(t, registry.NewStore(controlHome, repoA), "mailpit", []int{8025}, routed(8025))
		err := conflict(t, registry.NewStore(controlHome, repoB), func(plan *registry.SharedPortPlan) {
			plan.Domain = "ACME.localhost"
			routed(8025)(plan)
		})
		if err == nil {
			t.Fatal("a differently cased domain was not a conflict")
		}
	})

	t.Run("a different domain, a different name, or a service that routes nothing is no conflict", func(t *testing.T) {
		controlHome := claimedHome(t)
		syncShared(t, registry.NewStore(controlHome, repoA), "mailpit", []int{8025, 1025}, routed(8025))
		syncShared(t, registry.NewStore(controlHome, repoA), "smtp", []int{1025}, routed(0))
		if err := conflict(t, registry.NewStore(controlHome, repoB), func(plan *registry.SharedPortPlan) {
			plan.Domain = "widgets.localhost"
			routed(8025)(plan)
		}); err != nil {
			t.Errorf("another domain: %v", err)
		}
		if err := conflict(t, registry.NewStore(controlHome, repoB), func(plan *registry.SharedPortPlan) {
			plan.Name = "minio"
			routed(8025)(plan)
		}); err != nil {
			t.Errorf("another name: %v", err)
		}
		if err := conflict(t, registry.NewStore(controlHome, repoB), func(plan *registry.SharedPortPlan) {
			plan.Name = "smtp"
			routed(8025)(plan)
		}); err != nil {
			t.Errorf("the first routes nothing: %v", err)
		}
		if err := conflict(t, registry.NewStore(controlHome, repoB), routed(0)); err != nil {
			t.Errorf("the second routes nothing: %v", err)
		}
	})

	t.Run("a repository already routing its own service is not in conflict with itself", func(t *testing.T) {
		store := registry.NewStore(claimedHome(t), repoA)
		syncShared(t, store, "mailpit", []int{8025}, routed(8025))
		if err := conflict(t, store, routed(8025)); err != nil {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a record that cannot be read may be the first, so it refuses", func(t *testing.T) {
		controlHome := claimedHome(t)
		writeSharedFile(t, controlHome, repoA, "mailpit", "{")
		err := conflict(t, registry.NewStore(controlHome, repoB), routed(8025))
		if err == nil || !strings.Contains(err.Error(), "unreadable") {
			t.Fatalf("err = %v", err)
		}
	})

	// The allocator refuses an unreadable record too, but only when it has a
	// port to allocate. Here the second repository already holds its pin, so
	// only the conflict check can refuse, and it is the one being pinned.
	t.Run("the conflict check itself refuses on an unreadable record, with nothing to allocate", func(t *testing.T) {
		controlHome := claimedHome(t)
		storeB := registry.NewStore(controlHome, repoB)
		syncShared(t, storeB, "mailpit", []int{8025}, nil)
		writeSharedFile(t, controlHome, repoA, "mailpit", "{")
		err := conflict(t, storeB, routed(8025))
		if err == nil || !strings.Contains(err.Error(), "unreadable") || !strings.Contains(err.Error(), "a routed shared service") {
			t.Fatalf("err = %v", err)
		}
		if record, _ := storeB.FindShared("mailpit"); record == nil || record.Routed != 0 {
			t.Errorf("record = %+v", record)
		}
	})
}
