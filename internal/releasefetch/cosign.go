package releasefetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sofired/berth/internal/run"
	"github.com/sofired/berth/internal/semver"
	"github.com/sofired/berth/internal/text"
)

// The cosign contract (docs/design/teardown.md, "Verification").
const (
	// MinCosign is the oldest cosign that reads the bundle release.yml's
	// pinned cosign writes.
	MinCosign = "3.0.0"
	// Issuer is the OIDC issuer the release workflow's certificate carries.
	Issuer = "https://token.actions.githubusercontent.com"
	// identityBase is the release workflow at a tag, without the tag.
	identityBase = "https://github.com/sofired/berth/.github/workflows/release.yml@refs/tags/v"
	// VersionTimeout and VerifyTimeout bound the two cosign runs.
	VersionTimeout = 10 * time.Second
	VerifyTimeout  = 60 * time.Second
	// MaxStderr is how much of cosign's stderr a refusal quotes.
	MaxStderr = 2048
)

// allowedVariables are the only variables of berth's own environment that
// cosign is given, besides the ones berth sets itself (Dirs). cosign binds
// every flag to a COSIGN_<FLAG> variable (COSIGN_TRUSTED_ROOT picks the trust
// root, COSIGN_INSECURE_IGNORE_TLOG=true switches the log check off) and reads
// the SIGSTORE_* settings, so what a checkout's .envrc could set cannot be
// listed away: only what cosign needs to find itself, speak the user's language
// and reach the network through the user's own proxy and certificates is let
// through.
var allowedVariables = []string{
	"PATH", "LANG",
	"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// Policy is how cosign is told to verify checksums.txt: the identity the pin
// records and the arguments after `verify-blob`. KeylessPolicy is the one
// production implementation; tests supply their own.
type Policy interface {
	// Signer is the identity recorded in the pin as the one that was verified.
	Signer() string
	// Args are verify-blob's arguments, for the bundle and blob at those paths.
	Args(bundle, blob string) []string
}

// KeylessPolicy is the release workflow's keyless identity at the tag of
// Release: an exact identity, not a pattern, so another tag's signature does
// not verify.
type KeylessPolicy struct{ Release string }

// Signer is the exact certificate identity.
func (p KeylessPolicy) Signer() string { return identityBase + p.Release }

// Args is the whole of verify-blob's argv after the subcommand.
func (p KeylessPolicy) Args(bundle, blob string) []string {
	return []string{"--bundle", bundle, "--certificate-identity", p.Signer(), "--certificate-oidc-issuer", Issuer, blob}
}

// Root is a place cosign must not live: a directory branch code could have
// written to.
type Root struct {
	// Path is the root, canonical (symlinks resolved).
	Path string
	// Where describes it for the refusal: "the worktree <id>", "this
	// checkout" or "the control home".
	Where string
}

// InsideError is a cosign that resolves inside a Root.
type InsideError struct {
	Path, Canonical, Where string
}

// Error describes the refusal's cause.
func (e *InsideError) Error() string {
	return fmt.Sprintf("cosign %s (%s) is inside %s", e.Path, e.Canonical, e.Where)
}

// VersionError is a cosign that cannot be used: none was found, or its version
// is not MinCosign or newer.
type VersionError struct {
	// Path is the cosign found; empty when none was.
	Path string
	// Found says why it does not qualify.
	Found string
}

// Error describes the refusal's cause.
func (e *VersionError) Error() string {
	if e.Path == "" {
		return "no usable cosign was found (" + e.Found + ")"
	}
	return fmt.Sprintf("cosign %s does not qualify (%s)", e.Path, e.Found)
}

// VerifyKind says what kind of failure a verify-blob was, as far as cosign's
// exit and stderr tell. The kinds decide what a refusal may advise: only a
// trust-root failure is a reason to verify by hand, and a signature or identity
// failure is a reason to distrust the release.
type VerifyKind int

const (
	// FailUnknown is a failure that is none of the others, or that could not
	// be told apart: no advice is given beyond cosign's own words.
	FailUnknown VerifyKind = iota
	// FailStart: cosign could not be run.
	FailStart
	// FailTimeout: cosign did not finish in VerifyTimeout.
	FailTimeout
	// FailTrustRoot: cosign could not load the Sigstore trust root (TUF),
	// which says nothing about the release.
	FailTrustRoot
	// FailSignature: the signature, the certificate identity or the log
	// entry did not check out.
	FailSignature
)

// VerifyError is a verify-blob that did not succeed.
type VerifyError struct {
	Kind VerifyKind
	// Stderr is cosign's stderr, printable and cut to MaxStderr; or the
	// reason it could not be run.
	Stderr string
}

// Error is the stderr.
func (e *VerifyError) Error() string { return e.Stderr }

// signatureFailures and trustRootFailures are what cosign v3's error lines say
// (checked against v3.0.6). Only lines that begin "Error:" are read: a
// "WARNING:" about the trust root can precede a genuine signature failure.
var (
	signatureFailures = []string{
		"none of the expected identities matched", "invalid signature", "failed to verify signature",
		"failed to verify log inclusion", "failed to verify certificate", "no matching signatures",
		"certificate identity", "certificate issuer", "expected issuer", "expected identity",
	}
	// trustRootFailures is one pattern: "tuf" only as a word, so "stuff" is not a
	// trust-root failure.
	trustRootFailures = regexp.MustCompile(`trusted root|\btuf\b|root\.json`)
)

// classify reads cosign's error lines. A signature failure wins over a
// trust-root one, so the more serious reading is never hidden.
func classify(stderr string) VerifyKind {
	var lines []string
	for _, line := range strings.Split(stderr, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Error:"); ok {
			lines = append(lines, strings.ToLower(rest))
		}
	}
	joined := strings.Join(lines, "\n")
	for _, needle := range signatureFailures {
		if strings.Contains(joined, needle) {
			return FailSignature
		}
	}
	if trustRootFailures.MatchString(joined) {
		return FailTrustRoot
	}
	return FailUnknown
}

// Dirs are the berth-owned directories cosign runs in, supplied by the
// caller (this package reads no environment and chooses no directory). All
// three exist, are private to the user and are under the control home, so no
// .envrc can choose where cosign keeps its trust root or its scratch files.
type Dirs struct {
	// Home is cosign's HOME.
	Home string
	// TUFRoot is cosign's TUF_ROOT: its cache of the Sigstore trust root,
	// fetched on first use and kept across runs.
	TUFRoot string
	// Tmp is cosign's TMPDIR and the parent of Verify's temporary directory.
	Tmp string
}

// Cosign is the cosign to run, resolved once.
type Cosign struct {
	// Path is the PATH-resolved path, made absolute. It is what runs: a
	// multi-call binary or a wrapper script dispatches on its own name.
	Path string
	// Canonical is Path with its symlinks resolved, which is what the
	// containment test used.
	Canonical string
	// Version is cosign's gitVersion without a leading v.
	Version string

	runner run.Runner
	env    []string
	dirs   Dirs
}

// CosignEnv is the environment cosign runs in: the allowlisted variables of
// environ (allowedVariables, and every LC_*), with a relative PATH entry made
// absolute against cwd, and then HOME, TUF_ROOT and TMPDIR set to dirs. Nothing
// else of the caller's reaches it, COSIGN_*, SIGSTORE_*, HOME and TUF_ROOT
// included. The result is never nil, so a spec that carries it never inherits
// the process environment.
func CosignEnv(environ []string, cwd string, dirs Dirs) []string {
	out := make([]string, 0, len(allowedVariables)+4)
	for _, entry := range environ {
		name, value, _ := strings.Cut(entry, "=")
		allowed := strings.HasPrefix(name, "LC_")
		for _, candidate := range allowedVariables {
			allowed = allowed || name == candidate
		}
		if !allowed {
			continue
		}
		if name == "PATH" {
			entry = "PATH=" + absolutePath(value, cwd)
		}
		out = append(out, entry)
	}
	return append(out, "HOME="+dirs.Home, "TUF_ROOT="+dirs.TUFRoot, "TMPDIR="+dirs.Tmp)
}

// absolutePath is a PATH value whose relative entries (an empty one is the
// working directory) are joined to cwd, so that resolving cosign does not depend
// on this process's own working directory.
func absolutePath(path, cwd string) string {
	entries := filepath.SplitList(path)
	for i, entry := range entries {
		if entry == "" {
			entry = "."
		}
		if !filepath.IsAbs(entry) {
			entry = filepath.Join(cwd, entry)
		}
		entries[i] = entry
	}
	return strings.Join(entries, string(filepath.ListSeparator))
}

// Locate resolves cosign once, refuses one inside roots, and checks that it is
// MinCosign or newer. environ is berth's own environment, of which cosign
// receives only the allowlist (CosignEnv); its PATH is the one searched, a
// relative entry against cwd. The PATH-resolved path is made absolute and is
// what runs; its canonical form, with symlinks resolved, is what the roots are
// tested against, by path (filepath.Rel) and by os.SameFile for every ancestor,
// which sees through the case-insensitive volumes of macOS.
//
// The errors are *InsideError and *VersionError.
func Locate(ctx context.Context, runner run.Runner, environ []string, cwd string, dirs Dirs, roots []Root) (*Cosign, error) {
	env := CosignEnv(environ, cwd, dirs)
	resolved, startErr := run.ResolveExecutable("cosign", env)
	if startErr != nil {
		found := "cosign " + strings.TrimSuffix(startErr.Detail, ".")
		if startErr.Code == "ENOENT" {
			found = "cosign is not on PATH"
		}
		return nil, &VersionError{Found: found}
	}
	path := filepath.Clean(resolved)
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, &VersionError{Path: path, Found: "it cannot be resolved: " + text.Printable(err.Error())}
	}
	for _, root := range roots {
		if inside(root.Path, canonical) {
			return nil, &InsideError{Path: path, Canonical: canonical, Where: root.Where}
		}
	}
	c := &Cosign{Path: path, Canonical: canonical, runner: runner, env: env, dirs: dirs}
	if err := c.checkVersion(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// inside reports whether path is root or below it. Paths are compared as
// paths (filepath.Rel), never as strings, so a sibling that shares a prefix
// (berth.foo beside berth) is not inside. Each ancestor of path is also
// compared with root through os.SameFile, which sees through the
// case-insensitive volumes of macOS (and costs one stat per ancestor).
func inside(root, path string) bool {
	if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	for dir := path; ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(dir); err == nil && os.SameFile(rootInfo, info) {
			return true
		}
		if parent := filepath.Dir(dir); parent == dir {
			return false
		}
	}
}

// versionOutput is the part of `cosign version --json` that is read.
type versionOutput struct {
	GitVersion string `json:"gitVersion"`
}

// checkVersion runs `cosign version --json` and requires MinCosign or newer.
// A development build, a pre-release of MinCosign, output that is not the
// expected JSON and a failing run all refuse.
func (c *Cosign) checkVersion(ctx context.Context) error {
	result, err := c.runner.Run(ctx, run.Spec{
		Command: c.Path, Args: []string{"version", "--json"}, Env: c.env,
		AllowFailure: true, Timeout: VersionTimeout, Label: "cosign",
	})
	if err != nil {
		return &VersionError{Path: c.Path, Found: "it could not be run: " + runFailure(err)}
	}
	if result.TimedOut || result.ExitCode != 0 {
		return &VersionError{Path: c.Path, Found: "`cosign version --json` failed: " + text.OneLine(strings.TrimSpace(result.Stderr), 200)}
	}
	var out versionOutput
	if err := json.Unmarshal([]byte(result.Stdout), &out); err != nil {
		return &VersionError{Path: c.Path, Found: "`cosign version --json` printed no JSON"}
	}
	version := strings.TrimPrefix(out.GitVersion, "v")
	if !semver.AtLeast(version, MinCosign) {
		if out.GitVersion == "" {
			return &VersionError{Path: c.Path, Found: "it reported no gitVersion"}
		}
		return &VersionError{Path: c.Path, Found: "version " + text.OneLine(out.GitVersion, 64)}
	}
	c.Version = version
	return nil
}

// runFailure is why a run could not start, without repeating "could not be
// started" for a *run.StartError.
func runFailure(err error) string {
	var start *run.StartError
	if errors.As(err, &start) {
		return "it " + text.OneLine(strings.TrimSuffix(start.Detail, "."), 200)
	}
	return text.OneLine(err.Error(), 200)
}

// Verify checks that checksums (blob) is signed as policy says, by cosign
// verify-blob. Both files are written to a private temporary directory under
// Dirs.Tmp first,
// and the blob is read back afterwards: what cosign verified on disk must be
// the bytes the caller holds and will parse. The temporary directory is under
// Dirs.Tmp and is removed afterwards; nothing else is written.
func (c *Cosign) Verify(ctx context.Context, policy Policy, blob, bundle []byte) error {
	dir, err := os.MkdirTemp(c.dirs.Tmp, "verify-")
	if err != nil {
		return fmt.Errorf("could not create a temporary directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	blobPath := filepath.Join(dir, "checksums.txt")
	bundlePath := filepath.Join(dir, "checksums.txt.sigstore.json")
	for path, data := range map[string][]byte{blobPath: blob, bundlePath: bundle} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("could not write %s: %w", path, err)
		}
	}
	args := append([]string{"verify-blob"}, policy.Args(bundlePath, blobPath)...)
	result, err := c.runner.Run(ctx, run.Spec{
		Command: c.Path, Args: args, Env: c.env, Dir: dir,
		AllowFailure: true, Timeout: VerifyTimeout, Label: "cosign",
	})
	if err != nil {
		return &VerifyError{Kind: FailStart, Stderr: runFailure(err)}
	}
	if result.TimedOut {
		return &VerifyError{Kind: FailTimeout, Stderr: "it did not finish in time"}
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = fmt.Sprintf("it exited %d and printed nothing", result.ExitCode)
		}
		return &VerifyError{Kind: classify(detail), Stderr: text.OneLine(detail, MaxStderr)}
	}
	onDisk, err := os.ReadFile(blobPath)
	if err != nil {
		return fmt.Errorf("could not read back %s: %w", blobPath, err)
	}
	if !bytes.Equal(onDisk, blob) {
		return errors.New("checksums.txt changed on disk while cosign verified it")
	}
	return nil
}
