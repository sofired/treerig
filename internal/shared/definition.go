package shared

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/docker"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/text"
)

// DefinitionVersion prefixes the definition hash label, "v1:<sha256>", so a
// future change of what is hashed is told apart from a changed definition.
const DefinitionVersion = "v1"

// Definition is the shared Compose file as Compose resolves it, before
// berth's override (docs/design/routing.md, decision 16): what the stack is
// asked to run, from which a service's definition hash comes.
type Definition struct {
	// services holds each service's entry from `compose config`, decoded
	// with numbers kept as written.
	services map[string]map[string]any
	// volumes, networks, configs and secrets are the top-level sections:
	// for the override, and for the hash of whatever a service references.
	volumes  map[string]map[string]any
	networks map[string]map[string]any
	configs  map[string]map[string]any
	secrets  map[string]map[string]any
}

// Services are the names of the services in the file, sorted.
func (d Definition) Services() []string {
	names := make([]string, 0, len(d.services))
	for name := range d.services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Has reports whether the file defines a service called name.
func (d Definition) Has(name string) bool {
	_, ok := d.services[name]
	return ok
}

// Closure is name and every service it needs, transitively: what it names in
// depends_on, in links and in volumes_from, and the service whose network it
// shares with network_mode: service:X. Sorted. A service the file does not
// define yields nothing.
func (d Definition) Closure(name string) []string {
	if !d.Has(name) {
		return nil
	}
	seen := map[string]bool{}
	var visit func(string)
	visit = func(current string) {
		if seen[current] {
			return
		}
		entry, ok := d.services[current]
		if !ok {
			return
		}
		seen[current] = true
		for _, dependency := range needs(entry) {
			visit(dependency)
		}
	}
	visit(name)
	closure := make([]string, 0, len(seen))
	for member := range seen {
		closure = append(closure, member)
	}
	sort.Strings(closure)
	return closure
}

// needs is the services an entry cannot run without: its depends_on (in
// either form Compose may print, the map it normally prints or a plain list),
// its links, its volumes_from and a service whose network namespace it joins.
func needs(entry map[string]any) []string {
	var names []string
	switch value := entry["depends_on"].(type) {
	case map[string]any:
		for name := range value {
			names = append(names, name)
		}
	case []any:
		for _, item := range value {
			if name, ok := item.(string); ok {
				names = append(names, name)
			}
		}
	}
	// "svc" or "svc:alias" for a link, "svc[:ro|:rw]" for volumes_from; the
	// container: form of volumes_from names no service of this file.
	for _, key := range []string{"links", "volumes_from"} {
		list, _ := entry[key].([]any)
		for _, item := range list {
			text, _ := item.(string)
			name, _, _ := strings.Cut(text, ":")
			if name != "" && name != "container" {
				names = append(names, name)
			}
		}
	}
	if mode, _ := entry["network_mode"].(string); strings.HasPrefix(mode, "service:") {
		names = append(names, strings.TrimPrefix(mode, "service:"))
	}
	sort.Strings(names)
	return slices.Compact(names)
}

// referenced is the keys of the top-level volumes, networks, configs and
// secrets the services of a closure use: a named volume mounted, a network
// listed in the resolved service entry (Compose lists `default` there for a
// service that names none), a config or secret attached.
func referenced(entries map[string]map[string]any) (volumes, networks, configs, secrets []string) {
	add := func(into *[]string, name string) {
		if name != "" && !slices.Contains(*into, name) {
			*into = append(*into, name)
		}
	}
	for _, entry := range entries {
		mounts, _ := entry["volumes"].([]any)
		for _, mount := range mounts {
			fields, _ := mount.(map[string]any)
			if fields["type"] == "volume" {
				source, _ := fields["source"].(string)
				add(&volumes, source)
			}
		}
		switch joined := entry["networks"].(type) {
		case map[string]any:
			for name := range joined {
				add(&networks, name)
			}
		case []any:
			for _, item := range joined {
				name, _ := item.(string)
				add(&networks, name)
			}
		}
		for _, attachment := range []struct {
			key  string
			into *[]string
		}{{"configs", &configs}, {"secrets", &secrets}} {
			list, _ := entry[attachment.key].([]any)
			for _, item := range list {
				switch value := item.(type) {
				case string:
					add(attachment.into, value)
				case map[string]any:
					source, _ := value["source"].(string)
					add(attachment.into, source)
				}
			}
		}
	}
	sort.Strings(volumes)
	sort.Strings(networks)
	sort.Strings(configs)
	sort.Strings(secrets)
	return volumes, networks, configs, secrets
}

// Hash is the definition label value for service: "v1:" and the sha256 of
// the canonical JSON of the service, everything it needs (Closure), and the
// top-level volumes, networks, configs and secrets those use. The entries
// come from `compose config` before berth's override, so the published ports
// are not in it, and are interpolated with the shared environment only and no
// .env file; other inherited variables may still affect them. Canonical
// means object keys in sorted order (encoding/json's rule for a map),
// numbers as Compose printed them, and no HTML escaping. A Compose upgrade
// can change what `compose config` prints for the same file, which shows as
// a differing definition once; `berth shared up --recreate <name>` brings
// the label up to date.
// An undefined service or a value that JSON cannot encode returns an error.
func (d Definition) Hash(service string) (string, error) {
	closure := d.Closure(service)
	if closure == nil {
		return "", clierr.Newf("The shared Compose file defines no service called %s.", service)
	}
	entries := make(map[string]map[string]any, len(closure))
	for _, member := range closure {
		entries[member] = d.services[member]
	}
	volumes, networks, configs, secrets := referenced(entries)
	pick := func(from map[string]map[string]any, keys []string) map[string]any {
		picked := map[string]any{}
		for _, key := range keys {
			if entry, ok := from[key]; ok {
				picked[key] = entry
			} else {
				picked[key] = nil
			}
		}
		return picked
	}
	document := map[string]any{
		"services": entries,
		"volumes":  pick(d.volumes, volumes),
		"networks": pick(d.networks, networks),
		"configs":  pick(d.configs, configs),
		"secrets":  pick(d.secrets, secrets),
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return "", fmt.Errorf("hashing the definition of %s: %w", service, err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(encoded.Bytes(), []byte("\n")))
	return DefinitionVersion + ":" + hex.EncodeToString(sum[:]), nil
}

// environment is what the shared file is interpolated with and Compose runs
// under: the inherited environment, and the variables that mean the same in
// every worktree — the project's name, the repository, and the Compose
// project. Per-worktree BERTH_* variables such as BERTH_ROOT and
// BERTH_WORKTREE_ID are removed. BERTH_HOME, BERTH_CONTROL_HOME and other
// inherited variables allowed by docker.ComposeEnvironment can still affect
// interpolation.
func (s *Stack) environment() []string {
	extra := []string{
		"COMPOSE_PROJECT_NAME=" + s.Project(),
		"BERTH_PROJECT=" + s.o.ProjectName,
		"BERTH_REPOSITORY_ID=" + s.o.RepositoryID,
	}
	// A process started from a hook inherits that worktree's BERTH_*
	// variables; they would make the same file read differently from there.
	// BERTH_HOME and BERTH_CONTROL_HOME are the user's own and stay.
	var inherited []string
	for _, entry := range s.o.Environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, brand.EnvPrefix+"_") && name != brand.HomeEnv && name != brand.ControlHomeEnv {
			continue
		}
		inherited = append(inherited, entry)
	}
	return docker.ComposeEnvironment(inherited, extra...)
}

// composeArgs is the `docker compose` command line for args, with the shared
// file and any override as -f files.
func (s *Stack) composeArgs(args []string, withFiles ...string) []string {
	return s.composeArgsIn(filepath.Dir(s.o.ComposeFile), args, withFiles...)
}

// composeArgsIn is composeArgs with an explicit Compose project directory,
// against which Compose resolves every relative path in the file.
func (s *Stack) composeArgsIn(projectDirectory string, args []string, withFiles ...string) []string {
	full := []string{"compose", "--project-name", s.Project()}
	if len(withFiles) > 0 {
		// An explicit empty env file, so a .env beside the shared file is
		// never read: what runs and what is hashed depend on the file and the
		// shared environment alone. Every profile is on, so the hash covers a
		// helper behind a profile and `up <svc>` can start one.
		full = append(full, "--env-file", os.DevNull, "--profile", "*", "--project-directory", projectDirectory)
		for _, file := range withFiles {
			full = append(full, "-f", file)
		}
	}
	return append(full, args...)
}

// ReadDefinition reads the shared Compose file with `compose config` and
// refuses a definition that depends on the checkout that started it: a
// service with build:, a bind mount whose source is inside a worktree, a
// config or secret read from one. The project directory and every relative
// path resolve to whichever worktree starts the stack first, which `worktree
// remove` can delete, and the resolved absolute paths would make every other
// worktree's hash differ (decision 16, "Configuration"). Images, named
// volumes and absolute paths outside the supplied WorktreeRoots are fine.
// A definition whose resolved values change with the directory it is read
// from (a relative bind source that climbs out of the checkout, ${PWD}) is
// refused as well, so every checkout computes the same hash or none starts it.
// Non-empty ports entries on Declared services and their dependencies are
// also refused; berth publishes their ports itself. Missing configuration,
// Compose execution failures and invalid JSON are returned as errors.
func (s *Stack) ReadDefinition(ctx context.Context) (Definition, error) {
	file := s.o.ComposeFile
	if file == "" {
		return Definition{}, clierr.New("No shared Compose file is configured; this is a bug in berth, please report it.")
	}
	result, err := docker.Run(ctx, s.o.Runner, s.composeArgs([]string{"config", "--format", "json"}, file),
		run.Spec{Env: s.environment(), AllowFailure: true, Timeout: probeTimeout})
	if err != nil {
		return Definition{}, err
	}
	if result.ExitCode != 0 {
		return Definition{}, clierr.Newf("Compose could not read %s:\n%s", file, firstNonEmpty(strings.TrimSpace(result.Stderr), strings.TrimSpace(result.Stdout)))
	}
	definition, err := decodeDefinition(result.Stdout, file)
	if err != nil {
		return Definition{}, err
	}
	if err := definition.refuseUnsuitable(file, s.o.WorktreeRoots, s.o.Declared); err != nil {
		return Definition{}, err
	}
	if err := s.refuseCheckoutDependent(ctx, file, result.Stdout); err != nil {
		return Definition{}, err
	}
	return definition, nil
}

// refuseCheckoutDependent refuses a shared file whose resolved definition
// depends on where it is read from (decision 16): a relative path resolves
// against each checkout's own path, and ${PWD} is the directory the command
// ran in, so two checkouts of one repository would hash the same service
// differently and report a definition that differs when nothing does.
// Compose prints absolute paths, so a relative source cannot be told from an
// absolute one in the output. The file is read a second time from a scratch
// project directory, and with PWD set to it, and any value that differs is
// the file depending on its surroundings.
//
// Only a real difference is an error, or a cancelled ctx. Every other failure
// (no scratch directory, no symbolic link, Compose failing the second read,
// output that does not decode) skips the check with a warning, so a read-only
// command such as doctor never fails on it. The scratch directory holds a
// symbolic link to each entry of the real project directory, so a relative
// env_file beside the file still resolves; one that climbs out of the
// directory does not, and Compose fails the second read. A file whose
// directory is outside every worktree reads the same for every checkout and
// is not checked. A variable that differs between users or machines without
// naming a path (${USER}) cannot be detected and is documented instead.
// first is the output of the read from the real project directory.
func (s *Stack) refuseCheckoutDependent(ctx context.Context, file, first string) error {
	realDirectory := filepath.Dir(s.o.ComposeFile)
	root, inside := insideAny(s.o.WorktreeRoots, realDirectory)
	if !inside {
		return nil
	}
	// The file as the checkout names it, to keep the messages short.
	shown := file
	if relative, err := filepath.Rel(root, s.o.ComposeFile); err == nil && !strings.HasPrefix(relative, "..") {
		shown = relative
	}
	skip := func(reason string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.o.Logger.Warn(fmt.Sprintf(
			"Could not check whether %s reads the same from every checkout (%s); a relative path or ${PWD} there may give each checkout its own definition. "+
				"Use absolute paths there, or keep the files it reads under its own directory, to silence this.",
			text.Printable(shown), text.OneLine(reason, 160)))
		return nil
	}
	entries, err := os.ReadDir(realDirectory)
	if err != nil {
		return skip(err.Error())
	}
	if err := os.MkdirAll(s.o.ControlHome, 0o700); err != nil {
		return skip(err.Error())
	}
	base, err := os.MkdirTemp(s.o.ControlHome, probePrefix)
	if err != nil {
		return skip(err.Error())
	}
	defer func() { _ = os.RemoveAll(base) }()
	// Four levels down, so that a few `../` still land inside base, in a
	// place no real checkout has.
	probe := filepath.Join(base, "a", "b", "c", "d")
	if err := os.MkdirAll(probe, 0o700); err != nil {
		return skip(err.Error())
	}
	for _, entry := range entries {
		if err := os.Symlink(filepath.Join(realDirectory, entry.Name()), filepath.Join(probe, entry.Name())); err != nil {
			return skip(err.Error())
		}
	}
	env := docker.Merge(s.environment(), "PWD="+probe)
	result, err := docker.Run(ctx, s.o.Runner, s.composeArgsIn(probe, []string{"config", "--format", "json"}, file),
		run.Spec{Env: env, AllowFailure: true, Timeout: probeTimeout})
	if err != nil {
		return skip(err.Error())
	}
	if result.ExitCode != 0 {
		return skip(firstNonEmpty(firstLine(result.Stderr), firstLine(result.Stdout), "Compose exited "+strconv.Itoa(result.ExitCode)))
	}
	difference, differs, err := firstDifference(first, result.Stdout)
	if err != nil {
		return skip(err.Error())
	}
	if !differs {
		return nil
	}
	return clierr.Newf(
		"%s reads differently in each checkout: %s resolves to %s here, from %s. "+
			"Use an absolute path outside every worktree, a named volume, or a value that is the same in every checkout.",
		text.Printable(shown), text.Printable(difference.path), difference.describeHere(), difference.cause(realDirectory, probe, envValue(s.environment(), "PWD")))
}

// probePrefix names the scratch directories of refuseCheckoutDependent.
const probePrefix = "shared-probe-"

// envValue is the value of name in an environment list, or "".
func envValue(environment []string, name string) string {
	for _, entry := range slices.Backward(environment) {
		if value, found := strings.CutPrefix(entry, name+"="); found {
			return value
		}
	}
	return ""
}

// firstLine is the first non-empty line of output, trimmed.
func firstLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// difference is a value at which two reads of the shared file differ: its
// path in Compose's JSON, such as services.db.volumes[0].source, and what the
// read from the real project directory (here) and the scratch one (other)
// held. A nil value is a key the read lacked.
type difference struct {
	path  string
	here  any
	other any
}

// describeLimit is the most characters of a value the refusal quotes, so a
// long one (a key, a list) cannot push the remedy past a reader's cut.
const describeLimit = 80

// describeHere is the value here as the refusal quotes it.
func (d difference) describeHere() string {
	switch value := d.here.(type) {
	case nil:
		return "nothing"
	case string:
		if value == "" {
			return "an empty value"
		}
		return fmt.Sprintf("%q", text.OneLine(value, describeLimit))
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprintf("%q", text.OneLine(fmt.Sprint(value), describeLimit))
		}
		return text.OneLine(string(encoded), describeLimit)
	}
}

// cause names what made the value differ, only when substituting the second
// read's directories for the first's reproduces it: the PWD the command ran in
// for the scratch directory is ${PWD}; the project directory, or a directory
// above it by the same number of levels (a path that climbs out of it), is a
// relative path. A value both explain (a command run from the project
// directory), neither explains, or one without a PWD to try gets the neutral
// wording.
func (d difference) cause(realDirectory, probe, pwd string) string {
	const neutral = "a path or variable that depends on the checkout"
	here, _ := d.here.(string)
	other, _ := d.other.(string)
	substitutes := func(from, to string) bool {
		return from != "" && from != string(filepath.Separator) && strings.Contains(here, from) && strings.ReplaceAll(here, from, to) == other
	}
	viaPWD := substitutes(pwd, probe)
	viaPath := false
	for from, to := realDirectory, probe; from != filepath.Dir(from) && to != filepath.Dir(to); from, to = filepath.Dir(from), filepath.Dir(to) {
		if substitutes(from, to) {
			viaPath = true
			break
		}
	}
	switch {
	case viaPWD && !viaPath:
		return "${PWD}"
	case viaPath && !viaPWD:
		return "a relative path"
	default:
		return neutral
	}
}

// firstDifference reports the first value, in sorted key order, at which two
// `compose config --format json` outputs differ. An output that cannot be
// decoded reports an error.
func firstDifference(here, other string) (difference, bool, error) {
	decode := func(output string) (any, error) {
		decoder := json.NewDecoder(strings.NewReader(output))
		decoder.UseNumber()
		var document any
		if err := decoder.Decode(&document); err != nil {
			return nil, err
		}
		return document, nil
	}
	left, err := decode(here)
	if err != nil {
		return difference{}, false, fmt.Errorf("decoding the first read: %w", err)
	}
	right, err := decode(other)
	if err != nil {
		return difference{}, false, fmt.Errorf("decoding the second read: %w", err)
	}
	found, differs := diffValues("", left, right)
	return found, differs, nil
}

// diffValues walks two decoded JSON values together and reports the first
// difference.
func diffValues(path string, a, b any) (difference, bool) {
	switch left := a.(type) {
	case map[string]any:
		right, ok := b.(map[string]any)
		if !ok {
			return difference{path, a, b}, true
		}
		keys := make([]string, 0, len(left))
		for key := range left {
			keys = append(keys, key)
		}
		for key := range right {
			if _, ok := left[key]; !ok {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		keys = slices.Compact(keys)
		for _, key := range keys {
			child := key
			if path != "" {
				child = path + "." + key
			}
			if found, differs := diffValues(child, left[key], right[key]); differs {
				return found, true
			}
		}
		return difference{}, false
	case []any:
		right, ok := b.([]any)
		if !ok || len(left) != len(right) {
			return difference{path, a, b}, true
		}
		for i := range left {
			if found, differs := diffValues(fmt.Sprintf("%s[%d]", path, i), left[i], right[i]); differs {
				return found, true
			}
		}
		return difference{}, false
	default:
		if !reflect.DeepEqual(a, b) {
			return difference{path, a, b}, true
		}
		return difference{}, false
	}
}

// decodeDefinition decodes `compose config --format json` output.
func decodeDefinition(stdout, file string) (Definition, error) {
	var document struct {
		Services map[string]map[string]any `json:"services"`
		Volumes  map[string]map[string]any `json:"volumes"`
		Networks map[string]map[string]any `json:"networks"`
		Configs  map[string]map[string]any `json:"configs"`
		Secrets  map[string]map[string]any `json:"secrets"`
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return Definition{}, clierr.Newf("Compose returned invalid JSON for %s.", file)
	}
	definition := Definition{
		services: document.Services, volumes: document.Volumes, networks: document.Networks,
		configs: document.Configs, secrets: document.Secrets,
	}
	if definition.services == nil {
		definition.services = map[string]map[string]any{}
	}
	return definition, nil
}

// refuseUnsuitable refuses a definition that cannot be shared (decision 15's
// "Configuration"): one that depends on the checkout that started it — a
// service with build:, a bind mount whose source is inside a worktree, a
// config or secret read from one — and one that publishes a port of its own.
// The ports a shared service publishes are the ones berth.toml declares, on
// 127.0.0.1 and pinned; a `ports:` entry in the file would publish another,
// outside berth's allocation. declared are the services berth.toml names;
// the port check covers them and what they need. The build and path checks
// cover every service, config and secret in the definition.
func (d Definition) refuseUnsuitable(file string, roots, declared []string) error {
	for _, name := range d.Services() {
		entry := d.services[name]
		if _, ok := entry["build"]; ok {
			return clierr.Newf(
				"Shared service %s in %s has build:. A shared service outlives the checkout that started it, so it must run an image: "+
					"build it and push or tag it, and name it with image:.", name, file)
		}
		mounts, _ := entry["volumes"].([]any)
		for _, mount := range mounts {
			fields, _ := mount.(map[string]any)
			if fields["type"] != "bind" {
				continue
			}
			source, _ := fields["source"].(string)
			if root, inside := insideAny(roots, source); inside {
				return clierr.Newf(
					"Shared service %s in %s bind-mounts %s, which is inside the worktree %s. A shared service outlives the checkout that started it: "+
						"use a named volume, or an absolute path outside every worktree.", name, file, source, root)
			}
		}
	}
	for _, section := range []map[string]map[string]any{d.configs, d.secrets} {
		for _, entry := range section {
			if source, ok := entry["file"].(string); ok {
				if root, inside := insideAny(roots, source); inside {
					return clierr.Newf(
						"%s reads a config or secret from %s, which is inside the worktree %s. A shared service outlives the checkout that started it: "+
							"use an absolute path outside every worktree.", file, source, root)
				}
			}
		}
	}
	for _, name := range declared {
		for _, member := range d.Closure(name) {
			if ports, _ := d.services[member]["ports"].([]any); len(ports) > 0 {
				return clierr.Newf(
					"Shared service %s in %s has ports:. Declare the ports in [shared.service.%s] in %s instead: %s publishes them on 127.0.0.1, "+
						"on ports it pins, and a ports: entry would publish another on every interface.",
					member, file, name, brand.ConfigFile, brand.Name)
			}
		}
	}
	return nil
}

// insideAny reports the first of roots that path is equal to or inside,
// whether compared as written or with symbolic links resolved on both sides:
// a bind source reached through a link into a worktree is inside it, and on
// macOS the temporary and home directories are links.
func insideAny(roots []string, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	resolvedPath := path
	if canonical, err := registry.CanonicalPath(path); err == nil {
		resolvedPath = canonical
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		resolvedRoot := root
		if canonical, err := registry.CanonicalPath(root); err == nil {
			resolvedRoot = canonical
		}
		if within(root, path) || within(resolvedRoot, resolvedPath) {
			return root, true
		}
	}
	return "", false
}

// firstNonEmpty is the first non-empty value, or "" if every one is empty.
func firstNonEmpty(values ...string) string {
	if i := slices.IndexFunc(values, func(v string) bool { return v != "" }); i >= 0 {
		return values[i]
	}
	return ""
}

// within reports whether path is root itself or inside it, by path alone: a
// bind source is compared as Compose resolved it, without following links.
func within(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
