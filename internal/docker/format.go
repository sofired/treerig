package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/stateformat"
)

// NewerResource is a Docker resource whose format label says a newer treerig
// stamped it.
type NewerResource struct {
	// Kind is "container", "network" or "image".
	Kind string
	// ID is the resource's ID as Docker prints it (an image's is its short ID).
	ID string
	// Found is the format the label declares, as written.
	Found string
}

// FindNewerFormats lists the containers and networks matching every filter
// (label filters, without the "label=" prefix), and the images too when
// images is true, and returns those whose format label (the full name, label)
// is newer than supported. It is the cheap check made before `compose up`,
// which would otherwise recreate a container or network a newer treerig owns, or
// re-tag over an image it built, before treerig could refuse: one `docker ps`
// and one `docker network ls`, each printing only the ID and that label, and
// for images one `docker image ls -q` plus, only when it finds any, a single
// `docker image inspect` of all of them. Resources with a missing or
// unrecognised label are not reported: they are refused by the ownership
// proof at teardown, and `up` must not trap a project on them.
func FindNewerFormats(ctx context.Context, runner run.Runner, filters []string, label string, supported int, images bool, timeout time.Duration) ([]NewerResource, error) {
	template := fmt.Sprintf(`{{.ID}}%s{{.Label "%s"}}`, "\t", label)
	var newer []NewerResource
	collect := func(kind, output string) {
		for _, line := range strings.Split(output, "\n") {
			id, value, found := strings.Cut(strings.TrimSpace(line), "\t")
			if !found {
				continue
			}
			if verdict, version := stateformat.Label(strings.TrimSpace(value), supported); verdict == stateformat.FromNewer {
				newer = append(newer, NewerResource{Kind: kind, ID: id, Found: version})
			}
		}
	}
	for _, kind := range []struct {
		name string
		args []string
	}{{"container", []string{"ps", "-a"}}, {"network", []string{"network", "ls"}}} {
		args := append([]string{}, kind.args...)
		for _, filter := range filters {
			args = append(args, "--filter", "label="+filter)
		}
		args = append(args, "--format", template)
		result, err := Run(ctx, runner, args, run.Spec{Timeout: timeout})
		if err != nil {
			return nil, err
		}
		collect(kind.name, result.Stdout)
	}
	if !images {
		return newer, nil
	}
	// `docker image ls` cannot print a label, so the owned images are listed
	// by ID and then read together with one inspect.
	args := []string{"image", "ls", "-q"}
	for _, filter := range filters {
		args = append(args, "--filter", "label="+filter)
	}
	result, err := Run(ctx, runner, args, run.Spec{Timeout: timeout})
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(result.Stdout)
	if len(ids) == 0 {
		return newer, nil
	}
	inspect := fmt.Sprintf(`{{.Id}}%s{{index .Config.Labels "%s"}}`, "\t", label)
	result, err = Run(ctx, runner, append([]string{"image", "inspect", "--format", inspect}, ids...), run.Spec{Timeout: timeout})
	if err != nil {
		return nil, err
	}
	var short strings.Builder
	for _, line := range strings.Split(result.Stdout, "\n") {
		id, value, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found {
			continue
		}
		id = strings.TrimPrefix(id, "sha256:")
		if len(id) > 12 {
			id = id[:12]
		}
		short.WriteString(id + "\t" + value + "\n")
	}
	collect("image", short.String())
	return newer, nil
}
