package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/ports"
)

// Shared-service state (docs/design/routing.md, decision 19): one record per
// shared service, at <control home>/repositories/<repo>/shared/<name>.json,
// holding the loopback ports pinned to its container ports. The container is
// gone after `shared down` while its ports must stay put, which is why a
// record exists at all. It is written only by SyncSharedPorts and removed
// only by RemoveSharedRecord, both under the registry lock and both through
// recordForShared, the one builder, so no writer can drop a field it does not
// know about.

// SharedRecord is one shared service's pinned ports.
type SharedRecord struct {
	// RepositoryID is the repository the record belongs to; it is the
	// directory the file lives in, not a field of the file.
	RepositoryID string
	// Name is the service's name: the file's own name without .json.
	Name string
	// Domain is the project domain at the time the first port was
	// allocated. The web UI's hostname, <name>.shared.<domain>, is built
	// from it (layout.SharedHost), and it is what two repositories' routed
	// services of the same name conflict on.
	Domain string
	// Ports maps each container port to its pinned host port on 127.0.0.1.
	Ports map[int]int
	// Routed is the container port of the web UI the proxy routes to, one of
	// Ports' keys; 0 when the service has none. It lives in the record, not
	// only in berth.toml, so route repair, `doctor` and the checkout index
	// can judge a route file from the record alone, as they judge a
	// worktree's from its registration.
	Routed int
}

// RoutedHostPort is the host port the web UI is published on, or 0 when the
// service has no routed port.
func (r SharedRecord) RoutedHostPort() int { return r.Ports[r.Routed] }

// Host is the routed hostname of the service's web UI, or "" when it has none.
func (r SharedRecord) Host() string {
	if r.RoutedHostPort() == 0 {
		return ""
	}
	return layout.SharedHost(r.Name, r.Domain)
}

// HostPort is the host port pinned to container port, or 0 when none is.
func (r SharedRecord) HostPort(container int) int { return r.Ports[container] }

// sharedRecordJSON is the record as written, in a fixed key order. Nothing
// but recordForShared constructs one.
type sharedRecordJSON struct {
	Version int            `json:"version"`
	Name    string         `json:"name"`
	Domain  string         `json:"domain"`
	Ports   map[string]int `json:"ports"`
	Routed  int            `json:"routed,omitempty"`
}

// recordForShared is the one shared-record builder. It is total over
// SharedRecord (TestSharedRecordCarriesEveryField fills each field by
// reflection), so a writer that changes one field and passes the rest
// through cannot drop the others. Ports are written as {"<container>":
// <host>}: JSON object keys are strings, the host ports numbers.
func recordForShared(record SharedRecord) sharedRecordJSON {
	written := make(map[string]int, len(record.Ports))
	for container, host := range record.Ports {
		written[strconv.Itoa(container)] = host
	}
	return sharedRecordJSON{Version: 1, Name: record.Name, Domain: record.Domain, Ports: written, Routed: record.Routed}
}

// rawSharedRecord is the on-disk shape, decoded strictly: an unknown key is a
// shape error.
type rawSharedRecord struct {
	Version *float64       `json:"version"`
	Name    *string        `json:"name"`
	Domain  *string        `json:"domain"`
	Ports   map[string]int `json:"ports"`
	Routed  *int           `json:"routed"`
}

// parseSharedRecordJSON validates data as a shared-service record: version 1,
// a service name, a non-empty domain, and ports mapping container ports to
// host ports, each 1 to 65535, with no host port used twice, and an optional
// routed port that is one of the container ports. It returns the
// record without its repository ID.
func parseSharedRecordJSON(data []byte) (SharedRecord, error) {
	if !json.Valid(data) {
		return SharedRecord{}, errInvalidJSON
	}
	var raw rawSharedRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return SharedRecord{}, errUnrecognizedShape
	}
	if raw.Version == nil || *raw.Version != 1 || raw.Name == nil || !slugPattern.MatchString(*raw.Name) ||
		raw.Domain == nil || *raw.Domain == "" || raw.Ports == nil {
		return SharedRecord{}, errUnrecognizedShape
	}
	record := SharedRecord{Name: *raw.Name, Domain: *raw.Domain, Ports: map[int]int{}}
	used := map[int]bool{}
	for key, value := range raw.Ports {
		container, err := strconv.Atoi(key)
		if err != nil || !validPort(container) || strconv.Itoa(container) != key || !validPort(value) || used[value] {
			return SharedRecord{}, errUnrecognizedShape
		}
		used[value] = true
		record.Ports[container] = value
	}
	if raw.Routed != nil {
		if _, held := record.Ports[*raw.Routed]; !held {
			return SharedRecord{}, errUnrecognizedShape
		}
		record.Routed = *raw.Routed
	}
	return record, nil
}

// validPort reports whether port is a TCP port number.
func validPort(port int) bool { return port >= 1 && port <= 65_535 }

// ValidSharedName reports whether name can name a shared service, and so a
// record file: the service-name pattern (lower-case letters, digits and
// hyphens, at most 63), which has no path separator and no dot. Every reader
// and writer of a record checks it before a name reaches a path.
func ValidSharedName(name string) bool {
	return len(name) <= 63 && slugPattern.MatchString(name)
}

// checkSharedName refuses a name that is not a shared service's.
func checkSharedName(name string) error {
	if !ValidSharedName(name) {
		return clierr.Newf("%q is not a shared service name: lower-case letters, digits and hyphens, at most 63.", name)
	}
	return nil
}

// SharedDirectory is where one repository's shared-service records live.
func SharedDirectory(controlHome, repositoryID string) string {
	return layout.SharedLayout(controlHome, repositoryID).Dir
}

// readSharedRecord reads one record, reporting invalid names, non-regular
// files and invalid contents as a *clierr.Error. Filesystem errors propagate
// unchanged, including a *fs.PathError wrapping fs.ErrNotExist for a missing
// file.
func (s *Store) readSharedRecord(file string) (*SharedRecord, error) {
	name := strings.TrimSuffix(filepath.Base(file), ".json")
	if !slugPattern.MatchString(name) {
		return nil, clierr.Newf("Shared-service record is not named by a service name: %s", file)
	}
	// Regular files only, as for a registration: a symlink is a record
	// berth cannot vouch for, so it is reported and never followed.
	info, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, clierr.Newf("Shared-service record must be a regular file, not a symbolic link: %s", file)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	record, err := parseSharedRecordJSON(data)
	if err != nil {
		if errors.Is(err, errInvalidJSON) {
			return nil, clierr.Newf("Shared-service record is not valid JSON: %s", file)
		}
		return nil, clierr.Newf("Shared-service record has an unrecognized version or shape: %s", file)
	}
	if record.Name != name {
		return nil, clierr.Newf("Shared-service record's name does not match its path: %s", file)
	}
	record.RepositoryID = s.RepositoryID
	return &record, nil
}

// SharedRecords reads every readable shared-service record of this
// repository, sorted by name, plus the files that were not readable or valid.
// Missing directories and vanished files are ignored. Other directory-read
// errors are returned; individual record errors become RegistryProblems.
func (s *Store) SharedRecords() ([]SharedRecord, []RegistryProblem, error) {
	dir := SharedDirectory(s.ControlHome, s.RepositoryID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var records []SharedRecord
	var problems []RegistryProblem
	for _, entry := range entries {
		// Only *.json is a record: the directory also holds the override.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file := filepath.Join(dir, entry.Name())
		record, err := s.readSharedRecord(file)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			problems = append(problems, RegistryProblem{File: file, Reason: err.Error()})
			continue
		}
		records = append(records, *record)
	}
	slices.SortFunc(records, func(a, b SharedRecord) int { return strings.Compare(a.Name, b.Name) })
	return records, problems, nil
}

// FindShared reads one shared service's record, or answers (nil, nil) when it
// has none.
func (s *Store) FindShared(name string) (*SharedRecord, error) {
	if err := checkSharedName(name); err != nil {
		return nil, err
	}
	record, err := s.readSharedRecord(layout.SharedLayout(s.ControlHome, s.RepositoryID).RecordPath(name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return record, nil
}

// ScanSharedRecords reads every repository's shared-service records in a
// control home. Like ScanRegistrations it never fails for a bad record: an
// unreadable file is a Problem, so the allocator can refuse while one may be
// holding a port.
func ScanSharedRecords(controlHome string) ([]SharedRecord, []RegistryProblem, error) {
	repositories := layout.ControlLayout(controlHome).RepositoriesDir
	entries, err := os.ReadDir(repositories)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var records []SharedRecord
	var problems []RegistryProblem
	for _, entry := range entries {
		if !entry.IsDir() || !uuidPattern.MatchString(entry.Name()) {
			continue
		}
		found, bad, err := NewStore(controlHome, entry.Name()).SharedRecords()
		if err != nil {
			return nil, nil, err
		}
		records = append(records, found...)
		problems = append(problems, bad...)
	}
	return records, problems, nil
}

// SharedPortPlan is what SyncSharedPorts makes true of one shared service's
// ports.
type SharedPortPlan struct {
	// Name is the shared service.
	Name string
	// Domain is recorded with the first allocation; a service that already
	// has a record keeps the domain it was allocated under.
	Domain string
	// Ports are the container ports the service declares, each of which
	// needs a pinned host port.
	Ports []int
	// Prune drops the pinned port of a container port not in Ports. It is
	// for a caller about to create the container afresh: while one runs, it
	// may still publish the dropped port.
	Prune bool
	// Reallocate names container ports whose pinned port is replaced by a
	// new one, never the one they held.
	Reallocate []int
	// Range is where new host ports come from.
	Range layout.PortRange
	// Probe decides whether a candidate is free; nil means ports.LoopbackProbe.
	Probe ports.Probe
	// Reserved are ports the caller says are taken though no record holds them.
	Reserved []int
	// Routed sets the record's routed port: nil leaves it as it is, a
	// pointer to 0 says the service has no web UI, and any other value is
	// the container port of the UI, which must be pinned once the call has
	// run. Making a service routed is refused when another repository
	// already routes a shared service of the same name on the same domain.
	Routed *int
}

// SharedPortResult is what SyncSharedPorts did.
type SharedPortResult struct {
	// Ports is every container port's host port after the call.
	Ports map[int]int
	// Allocated are the host ports newly assigned, including reallocations,
	// by container port.
	Allocated map[int]int
	// Moved are the previous host ports of reallocated container ports.
	Moved map[int]int
	// Released are the host ports of container ports pruned.
	Released map[int]int
}

// SyncSharedPorts makes the service's record match plan under the registry
// lock: a declared port without a host port gets one, a reallocated one gets
// a new one, and with plan.Prune an undeclared one is dropped. A host port is
// ports.Pick over the range, keyed by (repository ID, "shared:<name>:<container
// port>"), past every port any registration or shared record holds and every
// port the probe reports busy — the same skip set a worktree's allocation
// reads, so neither can take the other's. A registry holding an unreadable
// record refuses the allocation: that record may hold a port. Nothing is
// written when nothing changes, and a service that declares no port and has
// no record gets none.
func (s *Store) SyncSharedPorts(ctx context.Context, writer Writer, plan SharedPortPlan) (SharedPortResult, error) {
	if err := checkSharedName(plan.Name); err != nil {
		return SharedPortResult{}, err
	}
	if err := files.SecureSharedDirectories(layout.SharedLayout(s.ControlHome, s.RepositoryID), writer.UserHome); err != nil {
		return SharedPortResult{}, err
	}
	probe := plan.Probe
	if probe == nil {
		probe = ports.LoopbackProbe{}
	}
	var result SharedPortResult
	err := s.withRegistryLock(ctx, writer, func(ctx context.Context) error {
		file := layout.SharedLayout(s.ControlHome, s.RepositoryID).RecordPath(plan.Name)
		stored, err := s.FindShared(plan.Name)
		if err != nil {
			return err
		}
		current := map[int]int{}
		domain := plan.Domain
		currentRouted := 0
		if stored != nil {
			current = stored.Ports
			domain = stored.Domain
			currentRouted = stored.Routed
		}
		next := maps.Clone(current)
		result = SharedPortResult{Allocated: map[int]int{}, Moved: map[int]int{}, Released: map[int]int{}}

		wanted := map[int]bool{}
		for _, port := range plan.Ports {
			wanted[port] = true
		}
		moving := map[int]bool{}
		for _, port := range plan.Reallocate {
			moving[port] = true
		}
		if plan.Prune {
			for container, host := range current {
				if !wanted[container] {
					result.Released[container] = host
					delete(next, container)
				}
			}
		}
		nextRouted := currentRouted
		if _, kept := next[nextRouted]; !kept {
			nextRouted = 0
		}
		if plan.Routed != nil {
			nextRouted = *plan.Routed
			if nextRouted != 0 && !wanted[nextRouted] && current[nextRouted] == 0 {
				return clierr.Newf("Shared service %s: container port %d is not one it declares, so it cannot be its routed port.", plan.Name, nextRouted)
			}
			if nextRouted != 0 && currentRouted == 0 {
				if err := s.refuseRoutedConflict(plan.Name, domain); err != nil {
					return err
				}
			}
		}
		var needy []int
		for _, container := range plan.Ports {
			if _, held := current[container]; !held || moving[container] {
				needy = append(needy, container)
			}
		}
		if len(needy) > 0 {
			taken, err := takenPorts(s.ControlHome, plan.Reserved)
			if err != nil {
				return err
			}
			for _, container := range needy {
				label := fmt.Sprintf("%s:%d", plan.Name, container)
				host, err := ports.Pick(ctx, plan.Range, probe, taken, s.RepositoryID, "shared:"+plan.Name+":"+strconv.Itoa(container))
				if err != nil {
					return noPortError(label, plan.Range, err)
				}
				taken[host] = true
				if previous, held := current[container]; held {
					result.Moved[container] = previous
				}
				result.Allocated[container] = host
				next[container] = host
			}
		}

		result.Ports = next
		if len(result.Allocated) == 0 && len(result.Released) == 0 && nextRouted == currentRouted {
			return nil
		}
		return files.AtomicJSON(file, recordForShared(SharedRecord{
			RepositoryID: s.RepositoryID, Name: plan.Name, Domain: domain, Ports: next, Routed: nextRouted,
		}))
	})
	if err != nil {
		return SharedPortResult{}, err
	}
	return result, nil
}

// refuseRoutedConflict refuses to route the shared service called name on
// domain when another repository of this control home already routes one of
// the same name there: both would claim <name>.shared.<domain>, and the
// second to be allocated is the one refused, naming the first
// (docs/design/routing.md, decision 19). A record that cannot be read refuses
// too, since it may be the first. Domains compare case-insensitively, as
// hostnames do.
func (s *Store) refuseRoutedConflict(name, domain string) error {
	records, problems, err := ScanSharedRecords(s.ControlHome)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return refuseUnreadable("a routed shared service",
			"a record that cannot be read may already route "+layout.SharedHost(name, domain)+".", problems)
	}
	for _, other := range records {
		if other.RepositoryID == s.RepositoryID || other.Name != name || other.Routed == 0 || !strings.EqualFold(other.Domain, domain) {
			continue
		}
		return clierr.Newf(
			"Cannot route shared service %s at %s: %s already routes a shared service of that name there. "+
				"Rename one of the two services. (If the other is no longer wanted, 'shared down --volumes' run there deletes its record and so releases the name.)",
			name, layout.SharedHost(name, domain), s.describeRepository(other.RepositoryID))
	}
	return nil
}

// describeRepository names a repository for a message: "the repository of
// <host>" after its first registered worktree, or its ID when this control
// home holds none.
func (s *Store) describeRepository(repositoryID string) string {
	registered, err := NewStore(s.ControlHome, repositoryID).List()
	if err != nil || len(registered) == 0 {
		return "repository " + repositoryID
	}
	return "the repository of the worktree " + layout.WorktreeHost(registered[0].Slug, registered[0].Domain)
}

// RemoveSharedRecord releases a shared service's pinned ports by deleting its
// record, under the registry lock. It is idempotent: no record is success.
// `shared down --volumes` calls it once the containers and volumes are gone,
// and nothing else does: not `shared down`, which keeps the ports, and never
// `worktree remove` (docs/design/routing.md, decision 19).
func (s *Store) RemoveSharedRecord(ctx context.Context, writer Writer, name string) error {
	if err := checkSharedName(name); err != nil {
		return err
	}
	if err := files.SecureSharedDirectories(layout.SharedLayout(s.ControlHome, s.RepositoryID), writer.UserHome); err != nil {
		return err
	}
	return s.withRegistryLock(ctx, writer, func(context.Context) error {
		err := os.Remove(layout.SharedLayout(s.ControlHome, s.RepositoryID).RecordPath(name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}
