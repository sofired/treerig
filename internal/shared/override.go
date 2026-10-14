package shared

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/stateformat"
)

// LoopbackHost is the host address a shared service's ports are published on.
const LoopbackHost = "127.0.0.1"

// overrideDocument is the Compose override berth starts the stack with
// (decision 17): ownership labels on every service, volume and network the
// shared file creates (and, on all but the volumes, the format label, which
// says what they mean: docs/design/state.md; a volume is proven by the
// ownership labels alone, since Compose offers to recreate a volume whose
// label changed, losing its data), the definition hash and the defining worktree on every
// service, and each declared port published on its pinned loopback port.
// published maps service to container port to host port. encoding/json sorts
// map keys, so the same inputs are the same bytes.
func (s *Stack) overrideDocument(d Definition, published map[string]map[int]int, definedBy string) (map[string]any, error) {
	ns := s.o.LabelNamespace
	ownership := func(withFormat bool) map[string]any {
		labels := map[string]any{
			brand.ManagedLabel(ns):    "true",
			brand.RepositoryLabel(ns): s.o.RepositoryID,
			brand.SharedLabel(ns):     "true",
		}
		if withFormat {
			labels[brand.FormatLabel(ns)] = strconv.Itoa(stateformat.DockerLabel)
		}
		return labels
	}

	services := map[string]any{}
	for _, name := range d.Services() {
		labels := ownership(true)
		hash, err := d.Hash(name)
		if err != nil {
			return nil, err
		}
		labels[brand.SharedDefinitionLabel(ns)] = hash
		if definedBy != "" {
			labels[brand.SharedDefinedByLabel(ns)] = definedBy
		}
		entry := map[string]any{"labels": labels}
		var ports []any
		containerPorts := make([]int, 0, len(published[name]))
		for container := range published[name] {
			containerPorts = append(containerPorts, container)
		}
		sort.Ints(containerPorts)
		for _, container := range containerPorts {
			// The long syntax pins the address to loopback and merges with
			// the service's own ports.
			ports = append(ports, map[string]any{
				"target": container, "published": strconv.Itoa(published[name][container]),
				"host_ip": LoopbackHost, "protocol": "tcp",
			})
		}
		if len(ports) > 0 {
			entry["ports"] = ports
		}
		services[name] = entry
	}

	volumes := map[string]any{}
	for name, volume := range d.volumes {
		if !isExternal(volume) {
			volumes[name] = map[string]any{"labels": ownership(false)}
		}
	}
	networks := map[string]any{}
	for name, network := range d.networks {
		if !isExternal(network) {
			networks[name] = map[string]any{"labels": ownership(true)}
		}
	}
	return map[string]any{"services": services, "volumes": volumes, "networks": networks}, nil
}

// isExternal reports whether a volume or network entry is declared external,
// which berth neither creates nor labels.
func isExternal(entry map[string]any) bool {
	switch value := entry["external"].(type) {
	case bool:
		return value
	case nil:
		_, present := entry["external"]
		return present
	default:
		return true
	}
}

// encodeOverride is the document as indented JSON, which is also valid YAML:
// Compose reads it as such, and berth needs no YAML emitter.
func encodeOverride(document map[string]any) ([]byte, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}
