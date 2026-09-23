// Command spikebench measures the TypeScript and Go builds of berth side by
// side on the same registered state, for the Go port spike's decision report.
//
//	go run ./tools/spikebench -go dist/berth              # timing table
//	go run ./tools/spikebench -sizes                       # cross-compiled binary sizes
//
// Every scenario runs WarmUp times unmeasured, then Runs times measured: the
// median and 90th percentile of wall time, the peak resident set size, and
// whether the two builds printed the same thing. Nothing here is imported by
// berth itself; only the standard library is used.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

type scenario struct {
	Name string
	Args []string
	// Dir is relative to the fixture root.
	Dir string
}

type sample struct {
	MedianMs float64 `json:"medianMs"`
	P90Ms    float64 `json:"p90Ms"`
	MaxRSSKB int64   `json:"maxRssKb"`
	Exit     int     `json:"exit"`
	Stdout   string  `json:"-"`
	Stderr   string  `json:"-"`
}

type row struct {
	Scenario   string  `json:"scenario"`
	TS         *sample `json:"ts,omitempty"`
	Go         *sample `json:"go,omitempty"`
	SameOutput *bool   `json:"sameOutput,omitempty"`
}

func main() {
	goBinary := flag.String("go", "dist/berth", "the Go build's executable (skipped when missing)")
	node := flag.String("node", "node", "node for the TypeScript build")
	runs := flag.Int("runs", 50, "measured runs per scenario")
	warmup := flag.Int("warmup", 5, "unmeasured runs per scenario")
	out := flag.String("json", "", "also write the results as JSON to this file")
	sizes := flag.Bool("sizes", false, "report cross-compiled binary sizes instead of timings")
	only := flag.String("only", "", "comma-separated scenario names to run")
	flag.Parse()

	repoRoot, err := os.Getwd()
	check(err)
	if *sizes {
		reportSizes(repoRoot)
		return
	}

	fixture := buildFixture(repoRoot, *node)
	defer cleanUp()

	scenarios := []scenario{
		{Name: "--help", Args: []string{"--help"}, Dir: "repo"},
		{Name: "config validate", Args: []string{"config", "validate"}, Dir: "repo"},
		{Name: "worktree list", Args: []string{"worktree", "list"}, Dir: "repo"},
		{Name: "worktree list --json", Args: []string{"worktree", "list", "--json"}, Dir: "repo"},
		{Name: "worktree list --all", Args: []string{"worktree", "list", "--all"}, Dir: "repo"},
		{Name: "urls", Args: []string{"urls"}, Dir: "repo"},
		{Name: "status (instant fake docker)", Args: []string{"status"}, Dir: "repo"},
	}
	if *only != "" {
		wanted := strings.Split(*only, ",")
		scenarios = slices.DeleteFunc(scenarios, func(s scenario) bool { return !slices.Contains(wanted, s.Name) })
	}

	tsCommand := []string{*node, filepath.Join(repoRoot, "src", "cli", "main.ts")}
	var goCommand []string
	if absolute, err := filepath.Abs(*goBinary); err == nil {
		if _, err := os.Stat(absolute); err == nil {
			goCommand = []string{absolute}
		}
	}

	commands := [][]string{tsCommand}
	if goCommand != nil {
		commands = append(commands, goCommand)
	}
	var rows []row
	for _, s := range scenarios {
		r := row{Scenario: s.Name}
		samples := measure(commands, s, fixture, *warmup, *runs)
		r.TS = samples[0]
		if goCommand != nil {
			r.Go = samples[1]
			same := r.TS.Exit == r.Go.Exit && r.TS.Stdout == r.Go.Stdout && r.TS.Stderr == r.Go.Stderr
			r.SameOutput = &same
		}
		rows = append(rows, r)
		fmt.Fprintf(os.Stderr, "measured %s\n", s.Name)
	}
	printTable(rows, *runs)
	if *out != "" {
		data, err := json.MarshalIndent(map[string]any{
			"machine": runtime.GOOS + "/" + runtime.GOARCH, "runs": *runs, "warmup": *warmup, "rows": rows,
		}, "", "  ")
		check(err)
		check(os.WriteFile(*out, append(data, '\n'), 0o644))
	}
}

type fixtureState struct {
	root string
	env  []string
}

// buildFixture makes a repository with two linked worktrees, registers all
// three through the TypeScript build's own registry, adds 17 records for two
// other repositories, and puts a docker on PATH that answers instantly.
func buildFixture(repoRoot, node string) fixtureState {
	root, err := os.MkdirTemp("", "spikebench-")
	check(err)
	cleanups = append(cleanups, func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	check(err)
	home := filepath.Join(root, "control-home")
	repo := filepath.Join(root, "repo")
	bin := filepath.Join(root, "bin")
	check(os.MkdirAll(repo, 0o755))
	check(os.MkdirAll(bin, 0o755))

	check(os.WriteFile(filepath.Join(repo, "berth.toml"), []byte(`[project]
name = "acme"
domain = "acme.localhost"
[compose]
file = "compose.yaml"
[[service]]
name = "api"
[service.index]
label = "API"
[[service]]
name = "mail"
subdomain = "mail"
[hooks]
prepare = "./prepare.sh"
backup = "./backup.sh"
`), 0o644))
	check(os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte("services:\n  api:\n    image: busybox:1.37.0\n  mail:\n    image: busybox:1.37.0\n"), 0o644))

	// Answers exactly what `status` asks, with no daemon behind it.
	check(os.WriteFile(filepath.Join(bin, "docker"), []byte(`#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    config) echo '{"name":"x","services":{"api":{"image":"busybox"},"mail":{"image":"busybox"}}}'; exit 0 ;;
    ps) echo '{"Service":"api","State":"running","Health":""}'; echo '{"Service":"mail","State":"exited","Health":""}'; exit 0 ;;
  esac
done
exit 0
`), 0o755))

	gitEnv := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	git := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		output, err := cmd.CombinedOutput()
		if err != nil {
			fail("git %v: %v\n%s", args, err, output)
		}
	}
	git(repo, "init", "--initial-branch=main", ".")
	git(repo, "add", ".")
	git(repo, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-m", "initial")
	git(repo, "worktree", "add", "-b", "feature/login", filepath.Join(root, "wt-login"))
	git(repo, "worktree", "add", "-b", "fix/crash", filepath.Join(root, "wt-crash"))

	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + root,
		"BERTH_HOME=" + home,
	}
	cmd := exec.Command(node, filepath.Join(repoRoot, "tools", "spikebench", "fixture.ts"), home, "17",
		repo, filepath.Join(root, "wt-login"), filepath.Join(root, "wt-crash"))
	cmd.Dir = repoRoot
	cmd.Env = append(env, gitEnv...)
	if output, err := cmd.CombinedOutput(); err != nil {
		fail("fixture: %v\n%s", err, output)
	}
	return fixtureState{root: root, env: env}
}

// measure runs each command in turn, round after round, rather than one
// command's runs and then the next's: whatever the machine is doing meanwhile
// (a build, thermal throttling) lands on both alike, and which goes first
// alternates each round so neither always runs on a warmer cache.
func measure(commands [][]string, s scenario, fixture fixtureState, warmup, runs int) []*sample {
	durations := make([][]float64, len(commands))
	results := make([]*sample, len(commands))
	for i := range results {
		results[i] = &sample{}
	}
	for round := 0; round < warmup+runs; round++ {
		for k := range commands {
			which := k
			if round%2 == 1 {
				which = len(commands) - 1 - k
			}
			command, result := commands[which], results[which]
			cmd := exec.Command(command[0], append(slices.Clone(command[1:]), s.Args...)...)
			cmd.Dir = filepath.Join(fixture.root, s.Dir)
			cmd.Env = fixture.env
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			started := time.Now()
			err := cmd.Run()
			elapsed := time.Since(started)
			exit := 0
			if err != nil {
				var exitError *exec.ExitError
				if !asExit(err, &exitError) {
					fail("%s: %v", strings.Join(command, " "), err)
				}
				exit = exitError.ExitCode()
			}
			// ru_maxrss from wait4: the largest single process among the
			// command and the children it reaped, not their sum.
			if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
				rss := int64(usage.Maxrss)
				if runtime.GOOS == "darwin" {
					rss /= 1024 // bytes there, kilobytes on Linux
				}
				result.MaxRSSKB = max(result.MaxRSSKB, rss)
			}
			if round == 0 {
				result.Exit, result.Stdout, result.Stderr = exit, stdout.String(), stderr.String()
			}
			if round >= warmup {
				durations[which] = append(durations[which], float64(elapsed.Microseconds())/1000)
			}
		}
	}
	for i, result := range results {
		slices.Sort(durations[i])
		result.MedianMs = percentile(durations[i], 0.5)
		result.P90Ms = percentile(durations[i], 0.9)
	}
	return results
}

func asExit(err error, target **exec.ExitError) bool {
	exitError, ok := err.(*exec.ExitError)
	if ok {
		*target = exitError
	}
	return ok
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(q*float64(len(sorted)-1) + 0.5)
	return sorted[index]
}

func printTable(rows []row, runs int) {
	fmt.Printf("| Scenario (%d runs) | TS median | TS p90 | Go median | Go p90 | Saved | TS RSS | Go RSS | Same output |\n", runs)
	fmt.Println("|---|---:|---:|---:|---:|---:|---:|---:|:---:|")
	for _, r := range rows {
		goMedian, goP90, saved, goRSS, same := "—", "—", "—", "—", "—"
		if r.Go != nil {
			goMedian = fmt.Sprintf("%.1f ms", r.Go.MedianMs)
			goP90 = fmt.Sprintf("%.1f ms", r.Go.P90Ms)
			saved = fmt.Sprintf("%.0f ms", r.TS.MedianMs-r.Go.MedianMs)
			goRSS = fmt.Sprintf("%.1f MB", float64(r.Go.MaxRSSKB)/1024)
		}
		if r.SameOutput != nil {
			same = map[bool]string{true: "yes", false: "**no**"}[*r.SameOutput]
		}
		fmt.Printf("| `%s` | %.1f ms | %.1f ms | %s | %s | %s | %.1f MB | %s | %s |\n",
			r.Scenario, r.TS.MedianMs, r.TS.P90Ms, goMedian, goP90, saved, float64(r.TS.MaxRSSKB)/1024, goRSS, same)
	}
}

// reportSizes cross-compiles cmd/berth for the four supported targets,
// stripped as a release would be, and prints raw and gzip sizes.
func reportSizes(repoRoot string) {
	dir, err := os.MkdirTemp("", "spikebench-sizes-")
	check(err)
	cleanups = append(cleanups, func() { _ = os.RemoveAll(dir) })
	defer cleanUp()
	fmt.Println("| Target | Stripped | gzip |")
	fmt.Println("|---|---:|---:|")
	for _, target := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		goos, goarch, _ := strings.Cut(target, "/")
		output := filepath.Join(dir, strings.ReplaceAll(target, "/", "-"))
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", output, "./cmd/berth")
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		if built, err := cmd.CombinedOutput(); err != nil {
			fail("build %s: %v\n%s", target, err, built)
		}
		info, err := os.Stat(output)
		check(err)
		fmt.Printf("| %s | %.1f MB | %.1f MB |\n", target, float64(info.Size())/1e6, float64(gzipSize(output))/1e6)
	}
}

func gzipSize(file string) int64 {
	input, err := os.Open(file)
	check(err)
	defer input.Close()
	var counter countingWriter
	writer, err := gzip.NewWriterLevel(&counter, gzip.BestCompression)
	check(err)
	_, err = io.Copy(writer, input)
	check(err)
	check(writer.Close())
	return counter.n
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

// cleanups undo what the run made — the fixture — however it ends.
var cleanups []func()

func cleanUp() {
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
	cleanups = nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "spikebench: "+format+"\n", args...)
	cleanUp()
	os.Exit(1)
}
