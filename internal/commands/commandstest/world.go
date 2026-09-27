// Package commandstest is a Go port of tests/helpers/command-world.ts,
// minus the runtime, router and checkout index: a hermetic world for
// command tests, with a fake Git runner and a real filesystem control home.
//
// World.Context builds a commands.CommandContext directly from a hand-built
// config.Config rather than loading a berth.toml, so a test states its
// config in Go. There is no Dispose: New uses a t.TempDir, which the testing
// package removes, where the TypeScript helper needs an explicit rm.
package commandstest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/brand"
	"github.com/sofired/berth/internal/checkoutindex/card"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/config"
	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/registry"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/run/fakerun"
	"github.com/sofired/berth/internal/worktree"
)

// services every test config declares.
type serviceSpec struct {
	name      string
	subdomain string
}

var testServices = []serviceSpec{{name: "api"}, {name: "mail", subdomain: "mail"}}

// World is a hermetic world for command tests.
type World struct {
	t           *testing.T
	Base        string
	ControlHome string
	// UserHome stands in for the caller's own resolved home directory
	// (commands.checkedHome's userHome, #80): a fixed, harmless path
	// nothing here ever creates or resolves against, since ControlHome is
	// always a fresh temp directory nowhere near it.
	UserHome string
	// RouterState is a fake shared router's own state directory: the
	// checkout index writes its page and takes its lock under here, never
	// under ControlHome — the router's state is machine-wide, per berth
	// root, not per control home (see internal/layout's RouterStateDir
	// comment).
	RouterState layout.RouterLayout
	// Root is the primary worktree's root — the first one New creates.
	Root   string
	common string
	Git    *registrytest.GitRunner
	Runner *fakerun.Runner
	// Docker answers every docker call the world's commands make.
	Docker *Docker
}

// TestContext is a commands.CommandContext plus the recording logger behind it.
type TestContext struct {
	*commands.CommandContext
	Logger *logger.Recording
}

// New is a world with one worktree (Root) already registered with Git, on branch.
func New(t *testing.T, branch, domain string) *World {
	t.Helper()
	base := registrytest.TempDir(t)
	routerDir := filepath.Join(base, "router")
	w := &World{
		Base:        base,
		ControlHome: filepath.Join(base, "control"),
		UserHome:    filepath.Join(base, "user-home"),
		RouterState: layout.RouterLayout{
			StateDir:     routerDir,
			ComposeFile:  filepath.Join(routerDir, "compose.yaml"),
			ProtocolFile: filepath.Join(routerDir, "protocol"),
			DynamicDir:   filepath.Join(routerDir, "dynamic"),
			IndexDir:     filepath.Join(routerDir, "index"),
			IndexFile:    filepath.Join(routerDir, "index", "index.html"),
			LockPath:     filepath.Join(routerDir, "lock"),
		},
		common: filepath.Join(base, "common"),
		t:      t,
	}
	w.Runner, w.Git = registrytest.NewFakeRunner()
	w.Docker = NewDocker("api", "mail")
	git := w.Git.Handler()
	w.Runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
		if spec.Command == "docker" {
			return w.Docker.Handle(ctx, spec)
		}
		return git(ctx, spec)
	}
	w.Root = w.addWorktree("worktree", branch, w.common)
	_ = domain // kept for parity with the TypeScript helper's signature; New itself needs no domain.
	return w
}

// Worktree adds another Git worktree, sharing this repository's common Git
// directory by default (mirroring `git worktree add` from the primary
// checkout), or common when given.
func (w *World) Worktree(name, branch string, common ...string) string {
	shared := w.common
	if len(common) > 0 && common[0] != "" {
		shared = common[0]
	}
	return w.addWorktree(name, branch, shared)
}

// addWorktree creates a worktree directory and its Git dir under common,
// registers it with the fake Git runner, and returns its root.
func (w *World) addWorktree(name, branch, common string) string {
	w.t.Helper()
	root := filepath.Join(w.Base, name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		w.t.Fatalf("mkdir worktree %s: %v", name, err)
	}
	if err := os.MkdirAll(filepath.Join(common, "worktrees", name), 0o700); err != nil {
		w.t.Fatalf("mkdir git dir for %s: %v", name, err)
	}
	w.Git.Add(root, branch, common)
	return root
}

// Context is a context whose config lives in root, with domain (and
// optionally a label-namespace) in its config. root defaults to w.Root.
func (w *World) Context(root, domain string, labelNamespace *string) *TestContext {
	if root == "" {
		root = w.Root
	}
	log := &logger.Recording{}
	services := make([]config.Service, len(testServices))
	for i, s := range testServices {
		services[i] = config.Service{Name: s.name, Subdomain: s.subdomain}
	}
	cfg := config.Config{
		Project: config.Project{
			Name:           projectName(domain),
			Domain:         domain,
			LabelNamespace: labelNamespace,
		},
		Compose: config.Compose{File: "compose.yaml"},
		Service: services,
	}
	loaded := &config.Loaded{Config: cfg, File: filepath.Join(root, brand.ConfigFile), Root: root}
	reg := registry.New(registry.Options{
		ControlHome:    w.ControlHome,
		Root:           root,
		Domain:         domain,
		LabelNamespace: cfg.LabelNamespace(),
		Runner:         w.Runner,
		Logger:         log,
		CommandName:    "berth",
		UserHome:       w.UserHome,
	})
	return &TestContext{
		CommandContext: &commands.CommandContext{
			CommandName: "berth",
			Invocation:  "berth",
			Logger:      log,
			Runner:      w.Runner,
			Loaded:      loaded,
			ControlHome: w.ControlHome,
			UserHome:    w.UserHome,
			RouterPort:  80,
			RouterState: w.RouterState,
			Registry:    reg,
		},
		Logger: log,
	}
}

// projectName mirrors the TypeScript helper's own derivation: the domain
// with a trailing ".localhost" trimmed off.
func projectName(domain string) string {
	return strings.TrimSuffix(domain, ".localhost")
}

// indexCardFixture is the on-disk shape of a card fixture, matching
// card.IndexCard's schema (see internal/checkoutindex/card).
type indexCardFixture struct {
	Version        int      `json:"version"`
	Project        string   `json:"project"`
	Domain         string   `json:"domain"`
	Slug           string   `json:"slug"`
	Host           string   `json:"host"`
	Root           string   `json:"root"`
	Branch         string   `json:"branch"`
	LabelNamespace string   `json:"labelNamespace"`
	Services       []string `json:"services"`
	UpdatedAt      string   `json:"updatedAt"`
}

// WriteIndexCard writes a fixture index card directly, as `up` would have
// left it — mirroring registrytest.WriteRecord.
// This is what gives `list` a "last seen" branch and root for a
// registration whose live Git identity is gone.
func (w *World) WriteIndexCard(repositoryID, worktreeID, project, domain, slug, host, root, branch string) {
	w.t.Helper()
	path := card.IndexCardPath(w.ControlHome, repositoryID, worktreeID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		w.t.Fatalf("mkdir for index card: %v", err)
	}
	data, err := json.Marshal(indexCardFixture{
		Version: 1, Project: project, Domain: domain, Slug: slug, Host: host,
		Root: root, Branch: branch, LabelNamespace: brand.LabelNamespace,
		Services: []string{}, UpdatedAt: "2026-01-01T00:00:00.000Z",
	})
	if err != nil {
		w.t.Fatalf("marshal index card: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		w.t.Fatalf("write index card: %v", err)
	}
}

// Register is what a finished `up` leaves behind, as far as the ported verbs
// can see: identity markers, a complete registration recording namespace, the
// worktree's home, and one running container per service plus a data volume.
// root defaults to w.Root.
func (w *World) Register(root, domain, slug, namespace string, services ...string) worktree.Ref {
	w.t.Helper()
	if root == "" {
		root = w.Root
	}
	// Claimed first, as up claims it: a control home berth did not create is refused.
	if err := files.SecureControlDirectories(layout.ControlLayout(w.ControlHome), w.UserHome); err != nil {
		w.t.Fatalf("control home: %v", err)
	}
	identities := registry.NewIdentityService(root, w.Runner)
	identity, err := registrytest.Register(context.Background(), w.ControlHome, identities, root, domain, slug, &namespace, "complete")
	if err != nil {
		w.t.Fatalf("register %s: %v", slug, err)
	}
	if err := files.SecureWorktreeDirectories(layout.WorktreeLayout(w.ControlHome, identity.RepositoryID, identity.WorktreeID), w.UserHome); err != nil {
		w.t.Fatalf("home for %s: %v", slug, err)
	}
	ref := worktree.Ref{RepositoryID: identity.RepositoryID, WorktreeID: identity.WorktreeID, Slug: slug}
	w.Docker.Up(ref, namespace, services...)
	return ref
}
