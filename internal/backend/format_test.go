package backend

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/worktree"
)

// TestFormatLabelOwnership covers the format label on a worktree's Docker
// resources (docs/design/state.md): the ownership labels prove who owns a
// resource only in the format they were written in, so a resource of a newer
// format, or with no format label, is not proven owned and is never touched.
func TestFormatLabelOwnership(t *testing.T) {
	ctx := context.Background()
	format := brand.FormatLabel(ns)

	t.Run("up stamps every service and network it creates with the format label, and no volume", func(t *testing.T) {
		rt, controlHome := routedRuntime(t, newFake(), nil)
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, []string{"api", "worker", "quiet"}); err != nil {
			t.Fatal(err)
		}
		doc := overrideDoc(t, controlHome)
		for _, section := range []string{"services", "volumes", "networks"} {
			entries := doc[section].(map[string]any)
			if len(entries) == 0 {
				t.Fatalf("no %s in the override", section)
			}
			for name, entry := range entries {
				labels := entry.(map[string]any)["labels"].(map[string]any)
				got, stamped := labels[format]
				// Compose offers to recreate a volume whose label changed,
				// losing its data, so a volume never carries the label.
				if section == "volumes" && stamped {
					t.Errorf("volume %s carries %s = %v", name, format, got)
				}
				if section != "volumes" && got != "1" {
					t.Errorf("%s %s: %s = %v", section, name, format, got)
				}
				if labels[brand.WorktreeLabel(ns)] != ref.WorktreeID {
					t.Errorf("%s %s: labels = %v", section, name, labels)
				}
			}
		}
	})

	t.Run("Inventory refuses a resource of a newer format, naming it and both versions, removing nothing", func(t *testing.T) {
		for _, kind := range []struct{ id, noun, service string }{{"c1", "container", " (service api)"}, {"n1", "network", ""}, {"i1", "image", ""}} {
			fake := populated()
			fake.labels[kind.id][format] = "2"
			rt, _ := newRuntime(t, fake)
			_, err := rt.Inventory(ctx, ref)
			want := "Refusing to touch the " + kind.noun + " " + kind.id + kind.service + ": it is format 2, written by a newer treerig, and this build reads format 1. Upgrade treerig, then run this again."
			if err == nil || err.Error() != want || fake.removals() {
				t.Fatalf("%s: err = %v", kind.noun, err)
			}
		}
	})

	t.Run("Inventory refuses a resource with no format label as not proven owned, removing nothing", func(t *testing.T) {
		for _, missing := range []string{"", "0", "one", "1.0"} {
			fake := populated()
			if missing == "" {
				delete(fake.labels["c2"], format)
			} else {
				fake.labels["c2"][format] = missing
			}
			rt, _ := newRuntime(t, fake)
			_, err := rt.Inventory(ctx, ref)
			want := "Refusing to touch the container c2 (service worker): its " + format +
				" label is missing or not recognised, so treerig cannot prove it is this worktree's. After checking that nothing needs it, remove it with `docker container rm c2`."
			if err == nil || err.Error() != want || fake.removals() {
				t.Fatalf("label %q: err = %v", missing, err)
			}
		}
	})

	t.Run("proves a volume by the ownership labels alone, whatever its format label says", func(t *testing.T) {
		for _, value := range []string{"", "9", "junk"} {
			fake := populated()
			if value == "" {
				delete(fake.labels["v1"], format)
			} else {
				fake.labels["v1"][format] = value
			}
			rt, _ := newRuntime(t, fake)
			owned, err := rt.Inventory(ctx, ref)
			if err != nil || !slices.ContainsFunc(owned, func(r worktree.OwnedResource) bool { return r.Kind == "volume" && r.ID == "v1" }) {
				t.Fatalf("format %q: owned = %+v, err = %v", value, owned, err)
			}
		}
	})

	t.Run("finds a resource of a newer format by its ownership labels, so that it is refused rather than orphaned", func(t *testing.T) {
		fake := populated()
		fake.labels["n1"][format] = "9"
		rt, _ := newRuntime(t, fake)
		if _, err := rt.Inventory(ctx, ref); err == nil || !strings.Contains(err.Error(), "the network n1") {
			t.Fatalf("err = %v", err)
		}
		for _, call := range fake.calls {
			if slices.ContainsFunc(call.Args, func(arg string) bool { return strings.Contains(arg, format) && strings.HasPrefix(arg, "label=") }) {
				t.Fatalf("a listing filtered on the format label, which would hide a newer resource: %v", call.Args)
			}
		}
	})
}

// networkRunner answers `docker network inspect` with one network carrying
// labels, and everything else as the embedded fake does.
type networkRunner struct {
	*dockerFake
	labels map[string]string
}

// Run answers the network inspect from labels and defers the rest.
func (n networkRunner) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	if len(spec.Args) >= 3 && spec.Args[0] == "network" && spec.Args[1] == "inspect" {
		document, _ := json.Marshal([]map[string]any{{"Id": "n1", "Name": "acme_default", "Labels": n.labels}})
		return run.Finish(spec, run.Result{Stdout: string(document)})
	}
	return n.dockerFake.Run(ctx, spec)
}

// TestProveNetworkFormat is the same rule for a network proven before a
// connect or a disconnect.
func TestProveNetworkFormat(t *testing.T) {
	format := brand.FormatLabel(ns)
	owned := func() map[string]string {
		return map[string]string{
			brand.ManagedLabel(ns): "true", brand.RepositoryLabel(ns): ref.RepositoryID, brand.WorktreeLabel(ns): ref.WorktreeID,
			ComposeProjectLabel: project, format: "1",
		}
	}
	prove := func(t *testing.T, labels map[string]string) NetworkProof {
		t.Helper()
		rt, _ := newRuntime(t, newFake())
		rt.options.Runner = networkRunner{dockerFake: newFake(), labels: labels}
		proof, err := rt.ProveNetwork(context.Background(), ref, "n1")
		if err != nil {
			t.Fatal(err)
		}
		return proof
	}

	t.Run("owns a network of the current format", func(t *testing.T) {
		if proof := prove(t, owned()); !proof.Owned || proof.Reason != "" {
			t.Fatalf("proof = %+v", proof)
		}
	})

	t.Run("does not own a network of a newer format, and says to upgrade", func(t *testing.T) {
		labels := owned()
		labels[format] = "2"
		proof := prove(t, labels)
		want := "it is format 2, written by a newer treerig, and this build reads format 1. Upgrade treerig, then run this again"
		if proof.Owned || proof.Reason != want {
			t.Fatalf("proof = %+v", proof)
		}
	})

	t.Run("does not own a network with no format label", func(t *testing.T) {
		labels := owned()
		delete(labels, format)
		proof := prove(t, labels)
		want := "its " + format + " label is missing or not recognised, so treerig cannot prove it is this worktree's"
		if proof.Owned || proof.Reason != want {
			t.Fatalf("proof = %+v", proof)
		}
	})
}

// TestComposeNeverPrompts pins why a label Compose sees as changed cannot
// leave `up` or `down` waiting on a question: Compose asks "Recreate (data will
// be lost)?" about a volume whose labels differ, and treerig never runs it where
// that question could be read or answered. Neither verb is Interactive, which
// is the only mode that hands the child this process's terminal (a nil Input
// gives the child /dev/null), and neither passes --yes.
func TestComposeNeverPrompts(t *testing.T) {
	fake := newFake()
	rt, controlHome := routedRuntime(t, fake, nil)
	ensureHome(t, controlHome)
	if err := rt.Up(context.Background(), ref, []string{"api"}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Down(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	calls := append(fake.composeCalls("up"), fake.composeCalls("down")...)
	if len(calls) != 2 {
		t.Fatalf("compose up and down calls = %d", len(calls))
	}
	for _, call := range calls {
		if call.Output == run.Interactive || call.Input != nil {
			t.Errorf("%v runs with a terminal or input: output %v, input %q", call.Args, call.Output, call.Input)
		}
		for _, arg := range call.Args {
			if arg == "--yes" || arg == "-y" {
				t.Errorf("%v answers Compose's prompts for it", call.Args)
			}
		}
	}
}

// listingRunner answers the pre-start listing of containers and networks with
// each ID and its format label, and defers everything else to the fake.
type listingRunner struct {
	*dockerFake
	// formats maps a kind ("ps", "network" or "image") to the value of its
	// resource's format label; an image is only listed when it has an entry.
	formats map[string]string
}

// Run answers a `ps` or `network ls` carrying an ID-and-label template.
func (l listingRunner) Run(ctx context.Context, spec run.Spec) (run.Result, error) {
	if len(spec.Args) >= 2 && spec.Args[0] == "image" {
		value, listed := l.formats["image"]
		l.dockerFake.mu.Lock()
		l.dockerFake.calls = append(l.dockerFake.calls, spec)
		l.dockerFake.mu.Unlock()
		switch {
		case !listed:
			return run.Finish(spec, run.Result{})
		case spec.Args[1] == "ls":
			return run.Finish(spec, run.Result{Stdout: "0123456789ab\n"})
		case spec.Args[1] == "inspect":
			return run.Finish(spec, run.Result{Stdout: "sha256:0123456789abcdef0123456789abcdef\t" + value + "\n"})
		}
	}
	index := slices.Index(spec.Args, "--format")
	if index >= 0 && strings.HasPrefix(spec.Args[index+1], "{{.ID}}\t{{.Label") {
		l.dockerFake.mu.Lock()
		l.dockerFake.calls = append(l.dockerFake.calls, spec)
		l.dockerFake.mu.Unlock()
		kind := spec.Args[0]
		return run.Finish(spec, run.Result{Stdout: "res-" + kind + "\t" + l.formats[kind] + "\n"})
	}
	return l.dockerFake.Run(ctx, spec)
}

// TestUpRefusesNewerResources covers the check made before `compose up`:
// Compose recreates a container or network whose labels differ from the
// override, so a resource a newer treerig stamped has to be refused first, with
// two listings and no inspect (docs/design/state.md).
func TestUpRefusesNewerResources(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []struct{ arg, noun string }{{"ps", "container"}, {"network", "network"}, {"image", "image"}} {
		t.Run("refuses a "+kind.noun+" of a newer format before `compose up` runs", func(t *testing.T) {
			fake := newFake()
			rt, controlHome := routedRuntime(t, fake, nil)
			rt.options.Runner = listingRunner{dockerFake: fake, formats: map[string]string{kind.arg: "2"}}
			ensureHome(t, controlHome)
			err := rt.Up(ctx, ref, []string{"api"})
			id := "res-" + kind.arg
			if kind.arg == "image" {
				id = "0123456789ab"
			}
			want := "Refusing to touch the " + kind.noun + " " + id + ": it is format 2, written by a newer treerig, and this build reads format 1. Upgrade treerig, then run this again."
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v\nwant %s", err, want)
			}
			if len(fake.composeCalls("up")) != 0 {
				t.Fatalf("Compose ran: %v", fake.calls)
			}
		})
	}

	t.Run("refuses an image a newer treerig built, so an older build cannot re-tag over it", func(t *testing.T) {
		// Covered by the table above; this pins that the image is inspected
		// once, all together, and only because one was listed.
		fake := newFake()
		rt, controlHome := routedRuntime(t, fake, nil)
		rt.options.Runner = listingRunner{dockerFake: fake, formats: map[string]string{"image": "3"}}
		ensureHome(t, controlHome)
		if err := rt.Up(ctx, ref, []string{"api"}); err == nil || !strings.Contains(err.Error(), "the image 0123456789ab: it is format 3") {
			t.Fatalf("err = %v", err)
		}
		inspects := 0
		for _, call := range fake.calls {
			if len(call.Args) > 1 && call.Args[0] == "image" && call.Args[1] == "inspect" {
				inspects++
			}
		}
		if inspects != 1 || len(fake.composeCalls("up")) != 0 {
			t.Fatalf("image inspects = %d, compose up calls = %d", inspects, len(fake.composeCalls("up")))
		}
	})

	t.Run("starts when the format labels are current, missing or unrecognised, using two listings, one image listing and no inspect", func(t *testing.T) {
		for _, value := range []string{"1", "", "junk"} {
			fake := newFake()
			rt, controlHome := routedRuntime(t, fake, nil)
			rt.options.Runner = listingRunner{dockerFake: fake, formats: map[string]string{"ps": value, "network": value}}
			ensureHome(t, controlHome)
			if err := rt.Up(ctx, ref, []string{"api"}); err != nil {
				t.Fatalf("label %q: %v", value, err)
			}
			listings, inspects, imageCalls := 0, 0, 0
			for _, call := range fake.calls {
				if len(call.Args) > 1 && call.Args[0] == "image" {
					imageCalls++
				}
				if slices.ContainsFunc(call.Args, func(a string) bool { return strings.HasPrefix(a, "{{.ID}}") }) {
					listings++
				}
				if len(call.Args) > 0 && call.Args[0] == "inspect" {
					inspects++
				}
			}
			if listings != 2 || inspects != 0 || imageCalls != 1 {
				t.Fatalf("label %q: listings = %d, inspects = %d, image calls = %d", value, listings, inspects, imageCalls)
			}
		}
	})
}
