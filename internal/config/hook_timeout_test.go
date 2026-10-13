package config

import (
	"strings"
	"testing"
	"time"

	"github.com/sofired/berth/internal/text"
)

// parseHooks parses minimalTOML plus tail, failing the test on an error, and
// returns the [hooks] table it read.
func parseHooks(t *testing.T, tail string) Hooks {
	t.Helper()
	cfg, err := Parse([]byte(minimalTOML+tail), "test.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg.Hooks
}

func TestHookTimeouts(t *testing.T) {
	t.Run("a file with no [hooks.timeout] gives every hook no limit, and config show leaves the key out", func(t *testing.T) {
		cfg, err := Parse([]byte(minimalTOML+"\n[hooks]\nprepare = \"./p.sh\"\n"), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Hooks.Timeout != nil {
			t.Fatalf("Timeout = %#v, want nil", cfg.Hooks.Timeout)
		}
		for _, stage := range hookStages {
			if got := cfg.Hooks.TimeoutFor(stage); got != 0 {
				t.Errorf("TimeoutFor(%s) = %s, want no limit", stage, got)
			}
		}
		out, err := text.SafeJSON(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "timeout") {
			t.Fatalf("config show printed a timeout:\n%s", out)
		}
	})

	t.Run("each hook kind reads its own limit, and the rest keep none", func(t *testing.T) {
		hooks := parseHooks(t, `
[hooks]
prepare = "./p.sh"
seed    = "./s.sh"

[hooks.timeout]
prepare = "30m"
verify  = "90s"
backup  = "1h30m"
restore = "250ms"
`)
		want := map[HookStage]time.Duration{
			Prepare: 30 * time.Minute,
			Seed:    0,
			Verify:  90 * time.Second,
			Backup:  90 * time.Minute,
			Restore: 250 * time.Millisecond,
		}
		for stage, d := range want {
			if got := hooks.TimeoutFor(stage); got != d {
				t.Errorf("TimeoutFor(%s) = %s, want %s", stage, got, d)
			}
		}
	})

	t.Run("config show prints the limits as written, in hook order", func(t *testing.T) {
		cfg, err := Parse([]byte(minimalTOML+"\n[hooks]\nprepare = \"./p.sh\"\n[hooks.timeout]\nverify = \"2m\"\nprepare = \"10m\"\n"), "test.toml")
		if err != nil {
			t.Fatal(err)
		}
		out, err := text.SafeJSON(cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := "\"timeout\": {\n      \"prepare\": \"10m\",\n      \"verify\": \"2m\"\n    }"
		if !strings.Contains(out, want) {
			t.Fatalf("config show lacks %q:\n%s", want, out)
		}
	})

	t.Run("a limit is a whole-number duration greater than zero and at most 24h", func(t *testing.T) {
		for _, good := range []string{"1ms", "30s", "10m", "1h30m", "24h", "1440m"} {
			hooks := parseHooks(t, "\n[hooks.timeout]\nprepare = \""+good+"\"\n")
			if hooks.TimeoutFor(Prepare) <= 0 {
				t.Errorf("%q read as no limit", good)
			}
		}
		for _, bad := range []string{"", "soon", "0s", "0", "-5m", "1.5h", "10", "10 m", "25h", "1d", "99999999999h"} {
			got := commandsError(t, "\n[hooks.timeout]\nprepare = \""+bad+"\"\n")
			if want := "hooks.timeout.prepare: "; !strings.Contains(got, want) {
				t.Errorf("%q: error %q lacks %q", bad, got, want)
			}
		}
		got := commandsError(t, "\n[hooks.timeout]\nseed = \"25h\"\n")
		if want := "hooks.timeout.seed: must be a duration greater than zero and at most 24h, such as 30s, 10m or 1h30m"; !strings.Contains(got, want) {
			t.Errorf("error %q lacks %q", got, want)
		}
	})

	t.Run("a limit that is not a string, and an unknown hook, are refused", func(t *testing.T) {
		if got := commandsError(t, "\n[hooks.timeout]\nprepare = 600\n"); !strings.Contains(got, "hooks.timeout.prepare") {
			t.Errorf("error %q does not name hooks.timeout.prepare", got)
		}
		if got := commandsError(t, "\n[hooks.timeout]\nprepar = \"1m\"\n"); !strings.Contains(got, "prepar") {
			t.Errorf("error %q does not name the unknown key", got)
		}
	})
}

func TestTimeoutForReadsOnlyWhatTheSchemaAdmits(t *testing.T) {
	for _, written := range []string{"25h", "junk", "0s", "", "-1m", "1.5h"} {
		hooks := Hooks{Timeout: &HookTimeouts{Prepare: &written}}
		if got := hooks.TimeoutFor(Prepare); got != 0 {
			t.Errorf("TimeoutFor(%q) = %s, want no limit for a value the schema would refuse", written, got)
		}
	}
	ok := "24h"
	if got := (Hooks{Timeout: &HookTimeouts{Seed: &ok}}).TimeoutFor(Seed); got != 24*time.Hour {
		t.Errorf("TimeoutFor(24h) = %s", got)
	}
}
