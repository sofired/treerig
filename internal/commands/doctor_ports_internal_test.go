package commands

// doctor's lines about loopback ports (#119): checkout.ports,
// checkout.published-port and registry.port-conflict.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
)

// registerWithPorts registers f's checkout and records ports for it.
func registerWithPorts(t *testing.T, f *checkoutFixture, ports map[string]int) registry.RegisteredWorktree {
	t.Helper()
	record := f.register("feature-login")
	if _, err := registrytest.WriteRecord(f.controlHome, registrytest.RecordJSON{
		Version: 1, RepositoryID: record.RepositoryID, WorktreeID: record.WorktreeID,
		Slug: record.Slug, Domain: record.Domain, Initialization: "complete", Ports: ports,
	}); err != nil {
		t.Fatal(err)
	}
	record.Ports = ports
	return record
}

// portLines are the checkout section's lines with the given id.
func portLines(t *testing.T, f *checkoutFixture, opts doctorOptions, id string) []Check {
	t.Helper()
	sections, err := doctorSections(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	checkReport(t, sections)
	var lines []Check
	for _, check := range sectionByID(sections, "checkout") {
		if check.ID == id {
			lines = append(lines, check)
		}
	}
	return lines
}

// publishedOn is a container's NetworkSettings.Ports document publishing
// each port on loopback.
func publishedOn(ports ...int) string {
	var entries []string
	for _, port := range ports {
		entries = append(entries, fmt.Sprintf(`"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"%d"}]`, port))
	}
	return "{" + strings.Join(entries, ",") + "}"
}

func TestDoctorLoopbackPorts(t *testing.T) {
	t.Run("is ok for a free port", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		registerWithPorts(t, f, map[string]int{"api": 30010})
		opts := f.opts()
		opts.PortProbe = registrytest.FreeProbe{}
		lines := portLines(t, f, opts, "checkout.ports")
		if len(lines) != 1 || lines[0].Status != StatusOK || lines[0].Instance != "api" || lines[0].Detail != "127.0.0.1:30010 is free" {
			t.Fatalf("lines = %+v", lines)
		}
	})

	t.Run("is skipped until a port is allocated", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		registerWithPorts(t, f, nil)
		lines := portLines(t, f, f.opts(), "checkout.ports")
		if len(lines) != 1 || lines[0].Status != StatusSkip || !strings.Contains(lines[0].Detail, "'treerig up' allocates one") {
			t.Fatalf("lines = %+v", lines)
		}
	})

	t.Run("is ok when this worktree's own container holds the port, proven by labels", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		record := registerWithPorts(t, f, map[string]int{"api": 30010})
		f.runner.state.docker["ps"] = okOutcome("c1\n")
		f.runner.state.docker["inspect"] = okOutcome("true\t" + record.RepositoryID + "\t" + record.WorktreeID + "\tapi\t/acme-api-1\t" + publishedOn(30010) + "\n")
		opts := f.opts()
		opts.PortProbe = registrytest.FreeProbe{Busy: map[int]bool{30010: true}}
		lines := portLines(t, f, opts, "checkout.ports")
		if len(lines) != 1 || lines[0].Status != StatusOK || !strings.Contains(lines[0].Detail, "held by this worktree's own container acme-api-1") {
			t.Fatalf("lines = %+v", lines)
		}
	})

	t.Run("warns when the running container publishes a different loopback port than the one recorded", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		record := registerWithPorts(t, f, map[string]int{"api": 30010})
		f.runner.state.docker["ps"] = okOutcome("c1\n")
		f.runner.state.docker["inspect"] = okOutcome("true\t" + record.RepositoryID + "\t" + record.WorktreeID + "\tapi\t/acme-api-1\t" + publishedOn(30099) + "\n")
		opts := f.opts()
		opts.PortProbe = registrytest.FreeProbe{} // the recorded port is free, yet a container runs on another
		lines := portLines(t, f, opts, "checkout.ports")
		if len(lines) != 1 || lines[0].Status != StatusWarn ||
			!strings.Contains(lines[0].Detail, "recorded, but the running container publishes 127.0.0.1:30099") {
			t.Fatalf("lines = %+v", lines)
		}
	})

	t.Run("fails for a container that is not this worktree's, and says how to move the service", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		registerWithPorts(t, f, map[string]int{"api": 30010})
		f.runner.state.docker["ps"] = okOutcome("c1\n")
		// Named like ours, carrying no labels at all.
		f.runner.state.docker["inspect"] = okOutcome("\t\t\t\t/acme-api-1\n")
		opts := f.opts()
		opts.PortProbe = registrytest.FreeProbe{Busy: map[int]bool{30010: true}}
		lines := portLines(t, f, opts, "checkout.ports")
		if len(lines) != 1 || lines[0].Status != StatusFail ||
			!strings.Contains(lines[0].Detail, "is held by container acme-api-1") ||
			!strings.Contains(lines[0].Detail, "'treerig up --reallocate-port api'") {
			t.Fatalf("lines = %+v", lines)
		}
	})

	t.Run("fails for another process when no container publishes the port", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		registerWithPorts(t, f, map[string]int{"api": 30010})
		opts := f.opts()
		opts.PortProbe = registrytest.FreeProbe{Busy: map[int]bool{30010: true}}
		lines := portLines(t, f, opts, "checkout.ports")
		if len(lines) != 1 || lines[0].Status != StatusFail || !strings.Contains(lines[0].Detail, "is held by another process") {
			t.Fatalf("lines = %+v", lines)
		}
	})

	t.Run("hints how to find the process, by the operating system it is told", func(t *testing.T) {
		for os, want := range map[string]string{"linux": "ss -ltnp 'sport = :30010'", "darwin": "lsof -nP -iTCP:30010 -sTCP:LISTEN"} {
			f := newCheckoutFixture(t, "")
			registerWithPorts(t, f, map[string]int{"api": 30010})
			opts := f.opts()
			opts.OS = os
			opts.PortProbe = registrytest.FreeProbe{Busy: map[int]bool{30010: true}}
			lines := portLines(t, f, opts, "checkout.ports")
			if len(lines) != 1 || !strings.Contains(lines[0].Detail, want) {
				t.Fatalf("%s: lines = %+v, want %q", os, lines, want)
			}
		}
	})

	t.Run("warns, rather than guessing, when the port is busy and Docker is not answering", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		record := registerWithPorts(t, f, map[string]int{"api": 30010})
		opts := f.opts()
		opts.PortProbe = registrytest.FreeProbe{Busy: map[int]bool{30010: true}}
		registration := &registry.WorktreeRegistration{RegisteredWorktree: record}
		lines, err := loopbackPortChecks(context.Background(), opts, f.loaded, registration)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != 1 || lines[0].Status != StatusWarn || !strings.Contains(lines[0].Detail, "Docker is not answering") {
			t.Fatalf("lines = %+v", lines)
		}
	})

	t.Run("warns for a routed service whose Compose file also publishes a fixed host port", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		registerWithPorts(t, f, map[string]int{"api": 30010})
		f.runner.state.composeConfig = okOutcome(`{"services":{"api":{"ports":[{"target":80,"published":"8080"}]},"other":{"ports":[{"target":1,"published":"9"}]}}}`)
		opts := f.opts()
		opts.PortProbe = registrytest.FreeProbe{}
		lines := portLines(t, f, opts, "checkout.published-port")
		if len(lines) != 1 || lines[0].Status != StatusWarn || lines[0].Instance != "api" || !strings.Contains(lines[0].Detail, "host port 8080 to container port 80") {
			t.Fatalf("lines = %+v", lines)
		}
	})

	t.Run("says nothing about published ports when the Compose file publishes none", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		registerWithPorts(t, f, map[string]int{"api": 30010})
		opts := f.opts()
		opts.PortProbe = registrytest.FreeProbe{}
		if lines := portLines(t, f, opts, "checkout.published-port"); len(lines) != 0 {
			t.Fatalf("lines = %+v", lines)
		}
		if lines := portLines(t, f, opts, "checkout.published-port-scan"); len(lines) != 0 {
			t.Fatalf("lines = %+v", lines)
		}
	})

	t.Run("reports a Compose file it cannot read under checkout.published-port-scan", func(t *testing.T) {
		f := newCheckoutFixture(t, "")
		registerWithPorts(t, f, map[string]int{"api": 30010})
		f.runner.state.composeConfig = okOutcome(`not json`)
		opts := f.opts()
		opts.PortProbe = registrytest.FreeProbe{}
		lines := portLines(t, f, opts, "checkout.published-port-scan")
		if len(lines) != 1 || lines[0].Status != StatusFail {
			t.Fatalf("lines = %+v", lines)
		}
	})
}

func TestRegistryChecksPortConflicts(t *testing.T) {
	a := testWorktree("alpha", "acme.localhost")
	a.Ports = map[string]int{"api": 30010, "web": 30011}
	b := testWorktree("beta", "acme.localhost")
	b.Ports = map[string]int{"api": 30011}

	t.Run("fails for a port two worktrees record, once, naming both", func(t *testing.T) {
		checks := registryChecks(registry.RegistryScan{Records: []registry.RegisteredWorktree{a, b}}, "treerig")
		var conflicts []Check
		for _, check := range checks {
			if check.ID == "registry.port-conflict" {
				conflicts = append(conflicts, check)
			}
		}
		if len(conflicts) != 1 || conflicts[0].Status != StatusFail || conflicts[0].Instance != "30011" {
			t.Fatalf("conflicts = %+v", conflicts)
		}
		for _, want := range []string{"web of alpha.acme.localhost", "api of beta.acme.localhost", "--reallocate-port"} {
			if !strings.Contains(conflicts[0].Detail, want) {
				t.Errorf("detail %q lacks %q", conflicts[0].Detail, want)
			}
		}
	})

	t.Run("fails for one worktree recording a port for two services", func(t *testing.T) {
		twice := testWorktree("gamma", "acme.localhost")
		twice.Ports = map[string]int{"api": 30020, "web": 30020}
		for _, check := range registryChecks(registry.RegistryScan{Records: []registry.RegisteredWorktree{twice}}, "treerig") {
			if check.ID != "registry.port-conflict" {
				continue
			}
			// The remedy fits one checkout as well as several.
			for _, want := range []string{"api of gamma.acme.localhost", "web of gamma.acme.localhost", "for all but one of those services, each in the checkout that records it"} {
				if !strings.Contains(check.Detail, want) {
					t.Errorf("detail %q lacks %q", check.Detail, want)
				}
			}
			return
		}
		t.Fatal("no port conflict reported")
	})

	t.Run("says nothing when ports are distinct", func(t *testing.T) {
		c := testWorktree("delta", "acme.localhost")
		c.Ports = map[string]int{"api": 30030}
		for _, check := range registryChecks(registry.RegistryScan{Records: []registry.RegisteredWorktree{a, c}}, "treerig") {
			if check.ID == "registry.port-conflict" {
				t.Fatalf("unexpected %+v", check)
			}
		}
	})
}
