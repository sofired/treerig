package router_test

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/files"
	"github.com/sofired/berth/internal/logger"
	"github.com/sofired/berth/internal/router"
)

func TestRouterServiceEnsure(t *testing.T) {
	t.Run("writes the stack, starts it once, verifies it, and leaves a correct router alone", func(t *testing.T) {
		w := newWorld(t, 80, "")
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}

		raw, err := os.ReadFile(w.State.ComposeFile)
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Services map[string]struct {
				Image   string   `json:"image"`
				Ports   []string `json:"ports"`
				Command []string `json:"command"`
			} `json:"services"`
			Networks map[string]struct {
				Name     string `json:"name"`
				Internal bool   `json:"internal"`
			} `json:"networks"`
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		var names []string
		for name := range document.Services {
			names = append(names, name)
		}
		wantServices := map[string]bool{"docker-api": true, "index": true, "router": true}
		if len(names) != len(wantServices) {
			t.Fatalf("services = %v", names)
		}
		for _, name := range names {
			if !wantServices[name] {
				t.Fatalf("unexpected service %q", name)
			}
		}
		if info, err := os.Stat(w.State.IndexFile); err != nil || info.IsDir() {
			t.Fatalf("expected a page to exist before the health check needs one: %v", err)
		}
		if got := document.Services["router"].Ports; len(got) != 1 || got[0] != "127.0.0.1:80:80" {
			t.Fatalf("router ports = %v", got)
		}
		if !containsSubstring(document.Services["router"].Command, "--providers.file.directory="+router.DynamicConfigMount) {
			t.Fatalf("router command = %v", document.Services["router"].Command)
		}
		if document.Networks["router"].Name != testNetwork || document.Networks["docker-api"].Internal != true {
			t.Fatalf("networks = %+v", document.Networks)
		}
		// What the containers read must be readable by root with no capabilities; what they don't, stays private.
		if got := fileMode(t, w.State.DynamicDir); got != 0o755 {
			t.Errorf("DynamicDir mode = %o, want 0755", got)
		}
		if got := fileMode(t, w.State.IndexDir); got != 0o755 {
			t.Errorf("IndexDir mode = %o, want 0755", got)
		}
		if got := fileMode(t, w.State.IndexFile); got != 0o644 {
			t.Errorf("IndexFile mode = %o, want 0644", got)
		}
		if got := fileMode(t, w.State.ComposeFile); got != 0o600 {
			t.Errorf("ComposeFile mode = %o, want 0600", got)
		}
		protocol, err := os.ReadFile(w.State.ProtocolFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(protocol) != strconv.Itoa(router.Protocol)+"\n" {
			t.Errorf("protocol file = %q", protocol)
		}
		if got := fileMode(t, w.State.ProtocolFile); got != 0o600 {
			t.Errorf("ProtocolFile mode = %o, want 0600", got)
		}

		ups := w.ups()
		if len(ups) != 1 {
			t.Fatalf("expected exactly one `up`, got %v", ups)
		}
		want := []string{
			"compose", "--project-directory", w.State.StateDir, "--project-name", testProject, "-f", w.State.ComposeFile,
			"up", "--detach", "--wait", "--remove-orphans",
		}
		if strings.Join(ups[0], "\x00") != strings.Join(want, "\x00") {
			t.Errorf("up args = %v, want %v", ups[0], want)
		}
		lines, _ := w.Logger.Snapshot()
		if got := linesWithPrefix(lines, "warn:"); len(got) != 0 {
			t.Errorf("expected no warnings, got %v", got)
		}
	})

	t.Run("runs Compose with the same guarded environment as worktrees, so a shell setting cannot split the two", func(t *testing.T) {
		// COMPOSE_IGNORE_ORPHANS would make `up --remove-orphans` a hard error.
		w := newWorldOpts(t, "", 80, func(o *router.Options) {
			o.Environ = []string{"PATH=/usr/bin", "COMPOSE_IGNORE_ORPHANS=true", "COMPOSE_FILE=/elsewhere/compose.yaml"}
		})
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		env := w.firstUp().env
		for _, unwanted := range []string{"COMPOSE_IGNORE_ORPHANS", "COMPOSE_FILE"} {
			for _, entry := range env {
				if strings.HasPrefix(entry, unwanted+"=") {
					t.Errorf("expected %s to be stripped, got %v", unwanted, env)
				}
			}
		}
		if !containsSubstring(env, "COMPOSE_PROGRESS=plain") {
			t.Errorf("expected COMPOSE_PROGRESS=plain, got %v", env)
		}
		if !containsSubstring(env, "PATH=/usr/bin") {
			t.Errorf("expected PATH to be inherited, got %v", env)
		}
	})

	t.Run("recreates a drifted router once, and accepts it when the recreation fixes it", func(t *testing.T) {
		w := newWorld(t, 80, "")
		drifted := routerDoc(w.State.DynamicDir, w.Port)
		drifted.HostConfig.ReadonlyRootfs = false
		w.Fake.setDoc("router-1", drifted)
		w.Fake.mu.Lock()
		w.Fake.onRecreate = w.setHealthy
		w.Fake.mu.Unlock()

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}

		ups := w.ups()
		if len(ups) != 2 || containsArg(ups[0], "--force-recreate") || !containsArg(ups[1], "--force-recreate") {
			t.Fatalf("ups = %v", ups)
		}
		lines, _ := w.Logger.Snapshot()
		if !containsSubstring(lines, "warn: The shared router is not configured as expected (router: root filesystem is writable)") {
			t.Errorf("expected a drift warning, got %v", lines)
		}
	})

	t.Run("fails, listing every problem, when recreation does not help", func(t *testing.T) {
		w := newWorld(t, 80, "")
		bad := routerDoc(w.State.DynamicDir, w.Port)
		bad.HostConfig.CapDrop = nil
		bad.NetworkSettings.Ports = map[string][]router.InspectPortBinding{"80/tcp": {{HostIP: "0.0.0.0", HostPort: "80"}}}
		w.Fake.setDoc("router-1", bad)

		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(),
			"could not be brought to its expected configuration:\n  router: capabilities are not dropped\n  router: port 80 is not published only on 127.0.0.1:80") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 2 {
			t.Fatalf("ups = %v", w.ups())
		}
	})

	t.Run("says so when it upgrades a router started by an older version, and what that costs other worktrees", func(t *testing.T) {
		w := newWorld(t, 80, "")
		older := routerDoc(w.State.DynamicDir, w.Port)
		older.Config.Labels = testLabels("router")
		older.Config.Labels[testNS+".router-protocol"] = strconv.Itoa(router.RoutedBySinceProtocol - 1)
		w.Fake.setDoc("router-1", older)
		w.Fake.mu.Lock()
		w.Fake.onRecreate = w.setHealthy
		w.Fake.mu.Unlock()

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines, _ := w.Logger.Snapshot()
		mustContain(t, lines, "info: Upgrading the shared router from protocol "+strconv.Itoa(router.RoutedBySinceProtocol-1)+" to "+strconv.Itoa(router.Protocol)+".")
		mustContain(t, lines, unrouted)
	})

	// Switches itself on at the next protocol bump: a router that already
	// carries the label needs no such warning. router.Protocol (4) is now
	// past router.RoutedBySinceProtocol (3), which the TypeScript source's
	// `it.skipIf(ROUTER_PROTOCOL === ROUTED_BY_SINCE_PROTOCOL)` used to skip
	// while they were equal; that can never happen again once the protocol
	// only ever increases, so this runs unconditionally here.
	t.Run("upgrades a router that already routes by label without warning about routing", func(t *testing.T) {
		w := newWorld(t, 80, "")
		older := routerDoc(w.State.DynamicDir, w.Port)
		older.Config.Labels = testLabels("router")
		older.Config.Labels[testNS+".router-protocol"] = strconv.Itoa(router.Protocol - 1)
		w.Fake.setDoc("router-1", older)
		w.Fake.mu.Lock()
		w.Fake.onRecreate = w.setHealthy
		w.Fake.mu.Unlock()

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines, _ := w.Logger.Snapshot()
		mustContain(t, lines, "info: Upgrading the shared router from protocol "+strconv.Itoa(router.Protocol-1)+" to "+strconv.Itoa(router.Protocol)+".")
		// The drift warning still fires here (the fake's doc carries the old label until it is recreated); only routing must stay silent.
		mustNotContain(t, lines, unrouted)
	})

	t.Run("upgrades a router that was taken down across the upgrade, from what this control home last ran", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w, protocolPtr(router.RoutedBySinceProtocol-1))

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}

		lines, _ := w.Logger.Snapshot()
		mustContain(t, lines, "info: Upgrading the shared router from protocol "+strconv.Itoa(router.RoutedBySinceProtocol-1)+" to "+strconv.Itoa(router.Protocol)+".")
		mustContain(t, lines, unrouted)
		if len(w.ups()) != 1 {
			t.Fatalf("expected the start itself to bring it back with no drift repair, got %v", w.ups())
		}
		if containsSubstring(lines, "not configured as expected") {
			t.Errorf("did not expect a drift warning, got %v", lines)
		}
		protocol, err := os.ReadFile(w.State.ProtocolFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(protocol) != strconv.Itoa(router.Protocol)+"\n" {
			t.Errorf("protocol file = %q, expected it promoted once it verified", protocol)
		}
	})

	t.Run("tells a second control home its worktrees went dark when another home did the upgrading", func(t *testing.T) {
		// Adapted: this exercised legacyProtocol(), the stack file's embedded
		// label as a third source of evidence beside the record and the
		// running container. WP3's brief drops legacyProtocol outright
		// ("droppable once ROUTER_PROTOCOL is past 3", src/router/router.ts:342-351),
		// so previousProtocol now has only two sources; a control home with
		// neither a record nor a stale-labelled container has no way to learn
		// it is behind, and there is nothing left this test could exercise.
		t.Skip("the owner decided to drop legacyProtocol (docs/port/divergences.md, row 11);" +
			"the per-home record this test was about no longer exists")
	})

	t.Run("does not let a newer version's stack file, which never started, poison the record", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w, nil)
		// A newer version wrote its stack file and then failed to start: intent, never a run.
		legacy := map[string]any{
			"services": map[string]any{
				"router": map[string]any{"labels": map[string]any{testNS + ".router-protocol": strconv.Itoa(router.Protocol + 1)}},
			},
		}
		encoded, _ := json.Marshal(legacy)
		if err := os.WriteFile(w.State.ComposeFile, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "bind: address already in use"
		if err := w.Router.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "The shared router failed to start") {
			t.Fatalf("got %v", err)
		}

		second := newWorldOpts(t, w.Dir, 80, nil)
		taken(t, second, nil)
		// If that number had been recorded, this control home would refuse itself for ever.
		if err := second.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("expected the second ensure to succeed, got %v", err)
		}
	})

	t.Run("names both steps when a newer router and a newer record are in the way, and both together hand it over", func(t *testing.T) {
		w := newWorld(t, 80, "")
		if err := os.MkdirAll(w.State.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(w.State.ProtocolFile, []byte(strconv.Itoa(router.Protocol+1)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		newer := routerDoc(w.State.DynamicDir, w.Port)
		newer.Config.Labels = testLabels("router")
		newer.Config.Labels[testNS+".router-protocol"] = strconv.Itoa(router.Protocol + 1)
		w.Fake.setDoc("router-1", newer)

		err := w.Router.Ensure(context.Background())
		wantSubstr := "remove it with 'duke router down' and this control home's record with `rm '" + w.State.ProtocolFile + "'`"
		if err == nil || !strings.Contains(err.Error(), wantSubstr) {
			t.Fatalf("got %v, want substring %q", err, wantSubstr)
		}

		// Doing both of the things it named has to be enough.
		w.Fake.mu.Lock()
		w.Fake.ids = map[string]string{}
		w.Fake.mu.Unlock()
		if err := os.Remove(w.State.ProtocolFile); err != nil {
			t.Fatal(err)
		}
		taken(t, w, nil)
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("expected this to succeed once both are cleared, got %v", err)
		}
	})

	t.Run("ignores a record that has been corrupted into something that is not a protocol", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w, nil)
		if err := os.WriteFile(w.State.ProtocolFile, []byte("0x10\n"), 0o600); err != nil { // reads as 16 to Number, but it is corruption
			t.Fatal(err)
		}

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}

		lines, _ := w.Logger.Snapshot()
		mustContain(t, lines, "warn: Ignoring "+w.State.ProtocolFile+": it does not name a router protocol. "+
			"This run takes the previous protocol from the router itself, if it is still there.")
		if containsSubstring(lines, "protocol 16") {
			t.Errorf("did not expect the corrupted value to be treated as protocol 16, got %v", lines)
		}
		mustNotContain(t, lines, unrouted) // no history at all is not an upgrade
	})

	t.Run("reads a record of %s as no protocol at all", func(t *testing.T) {
		for _, recorded := range []string{"0", "99999999999999999999"} {
			t.Run(recorded, func(t *testing.T) {
				w := newWorld(t, 80, "")
				taken(t, w, nil)
				if err := os.WriteFile(w.State.ProtocolFile, []byte(recorded+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}

				if err := w.Router.Ensure(context.Background()); err != nil {
					t.Fatal(err)
				}

				lines, _ := w.Logger.Snapshot()
				if !containsSubstring(linesWithPrefix(lines, "warn: Ignoring "+w.State.ProtocolFile), "") {
					t.Errorf("expected an ignoring warning, got %v", lines)
				}
			})
		}
	})

	t.Run("refuses to guess when its own protocol record cannot be read", func(t *testing.T) {
		skipIfRoot(t)
		w := newWorld(t, 80, "")
		taken(t, w, nil)
		if err := os.WriteFile(w.State.ProtocolFile, []byte(strconv.Itoa(router.Protocol)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(w.State.ProtocolFile, 0o000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(w.State.ProtocolFile, 0o600)

		// Reading it as "no record" is how a newer router gets silently downgraded.
		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), w.State.ProtocolFile+" could not be read (EACCES)") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 0 {
			t.Errorf("expected no `up` calls, got %v", w.ups())
		}
	})

	t.Run("ignores a stack file it cannot parse, which it is about to replace anyway", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w, nil)
		if err := os.WriteFile(w.State.ComposeFile, []byte("{ not json"), 0o600); err != nil {
			t.Fatal(err)
		}

		// One corrupt byte there must not brick every `up` on the machine.
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ignores a stack file it cannot read either", func(t *testing.T) {
		skipIfRoot(t)
		w := newWorld(t, 80, "")
		taken(t, w, nil)
		if err := os.WriteFile(w.State.ComposeFile, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(w.State.ComposeFile, 0o000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(w.State.ComposeFile, 0o600)

		// Unreadable is no answer here, where unreadable is fatal for the record.
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("lets a container carrying no protocol label fall through to the other evidence", func(t *testing.T) {
		w := newWorld(t, 80, "")
		unlabelled := routerDoc(w.State.DynamicDir, w.Port)
		rest := testLabels("router")
		delete(rest, testNS+".router-protocol")
		unlabelled.Config.Labels = rest
		w.Fake.setDoc("router-1", unlabelled)
		w.Fake.mu.Lock()
		w.Fake.onRecreate = w.setHealthy
		w.Fake.mu.Unlock()

		// Not ours, or started under another namespace: no answer, not an invalid one.
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("records nothing when the first ever start fails, because there was no history to record", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w, nil)
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "bind: address already in use"

		if err := w.Router.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "The shared router failed to start") {
			t.Fatalf("got %v", err)
		}
		exists, err := files.PathExists(w.State.ProtocolFile)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Error("expected no protocol file to have been written")
		}
	})

	t.Run("refuses a router name that could rewrite Traefik's discovery predicate", func(t *testing.T) {
		base := router.Options{Port: 80, Runner: newDockerFake(), Logger: logger.Silent{}}
		bad := base
		bad.ProjectName = "berth-router`,`x"
		if _, err := router.New(bad); err == nil || !strings.Contains(err.Error(), "Invalid router project name") {
			t.Errorf("got %v", err)
		}
		badNetwork := base
		badNetwork.Network = "n`,`x"
		if _, err := router.New(badNetwork); err == nil || !strings.Contains(err.Error(), "Invalid router network name") {
			t.Errorf("got %v", err)
		}
		ok := base
		ok.ProjectName = "berth-router-2.test_a"
		if _, err := router.New(ok); err != nil {
			t.Errorf("expected no error, got %v", err)
		}
	})

	t.Run("refuses a router whose protocol label is not a protocol", func(t *testing.T) {
		w := newWorld(t, 80, "")
		odd := routerDoc(w.State.DynamicDir, w.Port)
		odd.Config.Labels = testLabels("router")
		odd.Config.Labels[testNS+".router-protocol"] = "1e3"
		w.Fake.setDoc("router-1", odd)

		err := w.Router.Ensure(context.Background())
		want := "The shared router carries an invalid protocol label: 1e3. Remove the router with 'duke router down' and run this again."
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want %q", err, want)
		}
	})

	t.Run("remembers what it last brought up, so an upgrade that failed to start is still announced next time", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w, protocolPtr(router.RoutedBySinceProtocol-1))
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "bind: address already in use"

		if err := w.Router.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "The shared router failed to start") {
			t.Fatalf("got %v", err)
		}
		// write() already stamped the new protocol into the stack file; the record of what ran must not have moved.
		lines, _ := w.Logger.Snapshot()
		mustNotContain(t, lines, unrouted)

		second := newWorldOpts(t, w.Dir, 80, nil)
		taken(t, second, nil) // same control home, still nothing running

		if err := second.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines2, _ := second.Logger.Snapshot()
		mustContain(t, lines2, "info: Upgrading the shared router from protocol "+strconv.Itoa(router.RoutedBySinceProtocol-1)+" to "+strconv.Itoa(router.Protocol)+".")
		mustContain(t, lines2, unrouted)
	})

	t.Run("trusts what it last brought up over a container a failed start left behind", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w, protocolPtr(router.RoutedBySinceProtocol-1))
		// A start that failed after creating the containers leaves them already carrying
		// the new protocol label, though no router of that protocol has ever served anything.
		w.Fake.mu.Lock()
		w.Fake.ids = map[string]string{"router": "router-1", "docker-api": "api-1", "index": "index-1"}
		w.Fake.mu.Unlock()
		w.setHealthy()
		leftover := routerDoc(w.State.DynamicDir, w.Port)
		leftover.State.Status = "created" // created by the failed `up`, never started
		w.Fake.setDoc("router-1", leftover)

		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}

		lines, _ := w.Logger.Snapshot()
		mustContain(t, lines, "info: Upgrading the shared router from protocol "+strconv.Itoa(router.RoutedBySinceProtocol-1)+" to "+strconv.Itoa(router.Protocol)+".")
		mustContain(t, lines, unrouted)
	})

	t.Run("says nothing about upgrades when the router it brought up is the one this version speaks", func(t *testing.T) {
		w := newWorld(t, 80, "")
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		// The record is written by ensure itself, and is the only source left once the stack file is gone.
		protocol, err := os.ReadFile(w.State.ProtocolFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(protocol) != strconv.Itoa(router.Protocol)+"\n" {
			t.Fatalf("protocol file = %q", protocol)
		}
		if err := os.Remove(w.State.ComposeFile); err != nil {
			t.Fatal(err)
		}
		again := newWorldOpts(t, w.Dir, 80, nil)
		taken(t, again, nil) // taken down, then brought back on the same version

		if err := again.Router.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines, _ := again.Logger.Snapshot()
		if containsSubstring(lines, "Upgrading") {
			t.Errorf("did not expect an upgrade announcement, got %v", lines)
		}
		mustNotContain(t, lines, unrouted)
	})

	t.Run("refuses to take over from a newer version this control home last ran, and hands over once that record is gone", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w, protocolPtr(router.Protocol+1))
		// The newer version wrote its stack file before it ever started; that alone must not keep refusing.
		legacy := map[string]any{
			"services": map[string]any{
				"router": map[string]any{"labels": map[string]any{testNS + ".router-protocol": strconv.Itoa(router.Protocol + 1)}},
			},
		}
		encoded, _ := json.Marshal(legacy)
		if err := os.WriteFile(w.State.ComposeFile, encoded, 0o600); err != nil {
			t.Fatal(err)
		}

		err := w.Router.Ensure(context.Background())
		wantSubstr := "This control home last recorded the shared router at protocol " + strconv.Itoa(router.Protocol+1) +
			"; this version speaks " + strconv.Itoa(router.Protocol) + ". Run that version again, or remove the record with `rm "
		if err == nil || !strings.Contains(err.Error(), wantSubstr) {
			t.Fatalf("got %v, want substring %q", err, wantSubstr)
		}

		// The remedy the message names has to work, or it is not a remedy.
		if err := os.Remove(w.State.ProtocolFile); err != nil {
			t.Fatal(err)
		}
		if err := w.Router.Ensure(context.Background()); err != nil {
			t.Fatalf("expected the remedy to work, got %v", err)
		}
	})

	t.Run("records what it found on a control home that has none yet, so a failed start cannot erase it", func(t *testing.T) {
		w := newWorld(t, 80, "")
		taken(t, w, nil) // no record, as on every control home from before this version
		legacy := map[string]any{
			"services": map[string]any{
				"router": map[string]any{"labels": map[string]any{testNS + ".router-protocol": strconv.Itoa(router.RoutedBySinceProtocol - 1)}},
			},
		}
		encoded, _ := json.Marshal(legacy)
		if err := os.WriteFile(w.State.ComposeFile, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "bind: address already in use"

		if err := w.Router.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "The shared router failed to start") {
			t.Fatalf("got %v", err)
		}
		// Since this control home carries no legacy stack-file reading (dropped
		// per WP3's brief: legacyProtocol is obsolete past protocol 3), there is
		// no third source of evidence left once the record and the container are
		// both absent; nothing is recorded here, matching "records nothing..." above.
		exists, err := files.PathExists(w.State.ProtocolFile)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Error("expected no protocol file to have been written, since legacyProtocol is dropped past protocol 3 (WP3 brief)")
		}
	})

	t.Run("refuses to downgrade a newer router that is running, whatever its own record says", func(t *testing.T) {
		w := newWorld(t, 80, "")
		// The router is machine-wide; another control home may have upgraded it.
		if err := os.MkdirAll(w.State.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(w.State.ProtocolFile, []byte(strconv.Itoa(router.Protocol)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		newer := routerDoc(w.State.DynamicDir, w.Port)
		newer.Config.Labels = testLabels("router")
		newer.Config.Labels[testNS+".router-protocol"] = strconv.Itoa(router.Protocol + 1)
		w.Fake.setDoc("router-1", newer)

		err := w.Router.Ensure(context.Background())
		want := "The shared router is at protocol " + strconv.Itoa(router.Protocol+1) + "; this version speaks " + strconv.Itoa(router.Protocol) + "."
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v", err)
		}
		// The record is at this version's protocol, so it is not in the way and must not be named.
		err2 := w.Router.Ensure(context.Background())
		if err2 == nil || !strings.HasSuffix(strings.TrimRight(err2.Error(), "\n"), "remove it with 'duke router down'.") {
			t.Fatalf("got %v", err2)
		}
		if len(w.ups()) != 0 {
			t.Fatalf("expected the refusal to precede every mutation, got %v", w.ups())
		}
		protocol, err := os.ReadFile(w.State.ProtocolFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(protocol) != strconv.Itoa(router.Protocol)+"\n" {
			t.Errorf("expected the record left alone, got %q", protocol)
		}
	})

	t.Run("does not announce unrouted worktrees when the upgrade itself fails", func(t *testing.T) {
		w := newWorld(t, 80, "")
		older := routerDoc(w.State.DynamicDir, w.Port)
		older.Config.Labels = testLabels("router")
		older.Config.Labels[testNS+".router-protocol"] = strconv.Itoa(router.RoutedBySinceProtocol - 1)
		w.Fake.setDoc("router-1", older)
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "bind: address already in use"

		if err := w.Router.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "The shared router failed to start") {
			t.Fatalf("got %v", err)
		}
		lines, _ := w.Logger.Snapshot()
		mustNotContain(t, lines, unrouted)
	})

	t.Run("tells Traefik to discover only containers labelled for this router, and labels the index that way", func(t *testing.T) {
		// Pinned as literal text: the verifier compares against the same constants, so it alone cannot catch a typo here.
		if !containsSubstring(router.RouterCommand("n"), "--providers.docker.constraints=Label(`dev.berth.router`,`n`)") {
			t.Errorf("RouterCommand missing the discovery constraint")
		}
		labels := router.IndexRouteLabels("n").Map()
		if labels["dev.berth.router"] != "n" || labels["traefik.enable"] != "true" {
			t.Errorf("IndexRouteLabels = %v", labels)
		}
		// Every fixture derives from this, so nothing else would notice it moving.
		if router.RoutedBySinceProtocol != 3 {
			t.Errorf("RoutedBySinceProtocol = %d, want 3", router.RoutedBySinceProtocol)
		}
	})

	t.Run("refuses a router it cannot inspect rather than recreating it blind", func(t *testing.T) {
		w := newWorld(t, 80, "")
		w.Fake.clearDocs() // the containers are there; `docker inspect` says nothing about them

		err := w.Router.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "The shared router exists but could not be inspected.") {
			t.Fatalf("got %v", err)
		}
		if len(w.ups()) != 0 { // a router this version may not downgrade is never recreated blind
			t.Errorf("expected no `up` calls, got %v", w.ups())
		}
	})

	t.Run("warns instead of failing a verified up when the protocol record cannot be written", func(t *testing.T) {
		skipIfRoot(t)
		w := newWorld(t, 80, "")
		taken(t, w, protocolPtr(router.RoutedBySinceProtocol-1))
		w.Fake.mu.Lock()
		bring := w.Fake.onUp
		w.Fake.onUp = func() {
			bring()
			_ = os.Chmod(w.State.StateDir, 0o500) // read-only by the time the record is promoted
		}
		w.Fake.mu.Unlock()
		defer os.Chmod(w.State.StateDir, 0o700) // or the cleanup cannot unlink

		if err := w.Router.Ensure(context.Background()); err != nil { // the router is up; only the record failed
			t.Fatalf("expected success, got %v", err)
		}
		lines, _ := w.Logger.Snapshot()
		if !containsSubstring(linesWithPrefix(lines, "warn: Could not record the router protocol in "+w.State.ProtocolFile+": "), "") {
			t.Errorf("expected a warning naming the failed record write, got %v", lines)
		}
	})

	t.Run("explains a failed start in terms of the port", func(t *testing.T) {
		w := newWorld(t, 8080, "")
		w.Fake.upExitCode = 1
		w.Fake.upStderr = "Error response from daemon: Bind for 127.0.0.1:8080 failed: port is already allocated"

		err := w.Router.Ensure(context.Background())
		if err == nil ||
			!strings.Contains(err.Error(), "127.0.0.1:8080 is free, or choose another port with BERTH_ROUTER_PORT") ||
			!strings.Contains(err.Error(), "port is already allocated") {
			t.Fatalf("got %v", err)
		}
	})
}
