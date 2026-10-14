package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/stateformat"
)

// NewerResource is a Docker resource whose format label says a newer berth
// stamped it.
type NewerResource struct {
	// Kind is "container" or "network".
	Kind string
	// ID is the resource's ID as `docker ps` or `docker network ls` prints it.
	ID string
	// Found is the format the label declares, as written.
	Found string
}

// FindNewerFormats lists the containers and networks matching every filter
// (label filters, without the "label=" prefix) and returns those whose format
// label (the full name, label) is newer than supported. It is the cheap check
// made before `compose up`, which would otherwise recreate a resource a newer
// berth owns before berth could refuse it: one `docker ps` and one `docker
// network ls`, each printing only the ID and that label, and no inspect.
// Resources with a missing or unrecognised label are not reported: they are
// refused by the ownership proof at teardown, and `up` must not trap a project
// on them.
func FindNewerFormats(ctx context.Context, runner run.Runner, filters []string, label string, supported int, timeout time.Duration) ([]NewerResource, error) {
	template := fmt.Sprintf(`{{.ID}}%s{{.Label "%s"}}`, "\t", label)
	var newer []NewerResource
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
		for _, line := range strings.Split(result.Stdout, "\n") {
			id, value, found := strings.Cut(strings.TrimSpace(line), "\t")
			if !found {
				continue
			}
			if verdict, version := stateformat.Label(strings.TrimSpace(value), supported); verdict == stateformat.FromNewer {
				newer = append(newer, NewerResource{Kind: kind.name, ID: id, Found: version})
			}
		}
	}
	return newer, nil
}
