//go:build integration

package commands_test

// A real-daemon case of a shared service's routed web UI (#123): a
// container removed behind treerig's back, whose route file must be reported
// and then removed by `shared down`.

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/router"
)

func TestGoneSharedContainerKeepsA502RouteUntilSharedDownOnARealDaemon(t *testing.T) {
	w := newWalkFixture(t, "sharedgone", "feature/ui", sharedWalkToml, sharedWalkCompose)
	w.writeShared(t, w.checkout, "shared v1")
	ctx := context.Background()
	const uiHost = "mailpit.shared.shared-walk.localhost"
	result := w.up(t)
	repository := result.Registration.RepositoryID
	route := router.SharedFragmentPath(w.state, repository, "mailpit")
	w.eventually(t, uiHost, func(status int, _ string) bool { return status == http.StatusOK })

	ids := dockerIDs(t, "ps", "-aq", "--filter", "label="+w.namespace+".repository-uuid="+repository, "--filter", "label="+w.namespace+".shared=true")
	if len(ids) == 0 {
		t.Fatal("no shared container")
	}
	if output, err := exec.Command("docker", append([]string{"rm", "-f"}, ids...)...).CombinedOutput(); err != nil {
		t.Fatalf("docker rm: %v %s", err, output)
	}
	w.eventually(t, uiHost, func(status int, _ string) bool { return status == http.StatusBadGateway })

	sections, err := commands.DoctorSections(ctx, w.env, w.svc, w.state)
	if err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, section := range sections {
		for _, check := range section.Checks {
			warned = warned || (check.ID == "shared.route" && check.Status == commands.StatusWarn && strings.Contains(check.Detail, "answers 502"))
		}
	}
	if !warned {
		t.Error("doctor did not warn about the route of a service that is gone")
	}

	if err := commands.SharedDownWithContext(ctx, w.context(t), w.env, commands.SharedDownOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(route); !os.IsNotExist(err) {
		t.Fatalf("the route file survived shared down with its container gone: %v", err)
	}
	w.eventually(t, uiHost, func(status int, _ string) bool { return status == http.StatusNotFound })
	if err := commands.SharedDownWithContext(ctx, w.context(t), w.env, commands.SharedDownOptions{Volumes: true, Yes: true}); err != nil {
		t.Fatal(err)
	}
	w.downAndRemove(t, result, "feature-ui.shared-walk.localhost")
}
