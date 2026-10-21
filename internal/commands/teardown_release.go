package commands

// `teardown pin --release X.Y.Z` and `--latest` (#85, docs/design/teardown.md,
// "Sources", "Download bounds" and "Verification"): download a release, have
// cosign verify its checksums.txt, check the platform archive against it,
// extract the one binary, and pin it. Nothing is written to the control home
// until the user has approved the plan, and nothing runs the extracted
// binary: the gate that first executes it is #146.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/releasefetch"
	"github.com/sofired/treerig/internal/teardown"
	"github.com/sofired/treerig/internal/text"
)

// downloadTimeout is how long one command's downloads and cosign runs share.
const downloadTimeout = 10 * time.Minute

// teardownPinRelease is `teardown pin` for a download: --release X.Y.Z, or
// --latest, which only discovers X.Y.Z and then behaves the same.
func teardownPinRelease(ctx context.Context, env *Env, options TeardownPinOptions) error {
	subject := options.Release
	if options.Latest {
		subject = "the latest release"
	}
	platform, err := teardownPlatform(env, subject)
	if err != nil {
		return err
	}
	if env.Releases == nil {
		return clierr.Newf("Refusing to pin %s: this process has no way to download a release. Nothing was pinned.", text.Printable(subject))
	}
	control, userHome, err := teardownControl(env)
	if err != nil {
		return err
	}
	// A newer or foreign pin refuses before anything is downloaded.
	if _, _, err := teardown.ReadPin(control); err != nil {
		return err
	}

	timeout := options.DownloadTimeout
	if timeout == 0 {
		timeout = downloadTimeout
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	failure := func(release string, err error) error { return downloadFailure(ctx, release, timeout, err) }
	fetcher := loggingFetcher{inner: env.Releases, log: env.Logger}

	release := options.Release
	if options.Latest {
		version, err := releasefetch.Latest(fetchCtx, fetcher, options.Source)
		switch {
		case errors.Is(err, releasefetch.ErrNoStableRelease):
			return clierr.Newf("Refusing to pin the latest release: %s has no release that is not a pre-release yet (or the repository is not reachable). Pin a pre-release with `%s teardown pin %s`. Releases are listed at https://github.com/%s/releases.", brand.Repository, env.CommandName, publishedRelease, brand.Repository)
		case err != nil:
			return failure(subject, err)
		}
		release = version
	}
	var policy releasefetch.Policy = releasefetch.KeylessPolicy{Release: release}
	if options.Policy != nil {
		policy = options.Policy(release)
	}

	roots, err := cosignRoots(env, control)
	if err != nil {
		return clierr.Newf("Refusing to pin %s: could not work out where cosign must not live: %s. Nothing was pinned.", text.Printable(release), text.OneLine(err.Error(), 300))
	}
	dirs := releasefetch.Dirs{Home: control.CosignHome, TUFRoot: control.CosignTUFRoot, Tmp: control.CosignTmp}
	cosign, err := releasefetch.Locate(fetchCtx, env.Runner, env.Environ, env.Cwd, dirs, roots)
	if err != nil {
		if fetchCtx.Err() != nil {
			return failure(release, fetchCtx.Err())
		}
		return cosignRefusal(env, release, err)
	}

	// cosign's HOME, TUF_ROOT and TMPDIR are treerig's own, under the control
	// home: the one thing written there before the plan is approved, and a
	// cache, not the pin. They are made only once cosign has been found,
	// contained and versioned, so a refusal that ran nothing leaves nothing.
	if err := files.SecureCosignDirectories(control, userHome); err != nil {
		return clierr.Newf("Refusing to pin %s: could not prepare %s for cosign: %s. Nothing was pinned.", text.Printable(release), text.Printable(control.CosignHome), text.OneLine(err.Error(), 300))
	}

	base := options.Source.ReleaseBase(release)
	checksums, err := fetcher.Get(fetchCtx, base+"checksums.txt", releasefetch.MaxChecksums)
	if err != nil {
		return failure(release, err)
	}
	bundle, err := fetcher.Get(fetchCtx, base+"checksums.txt.sigstore.json", releasefetch.MaxBundle)
	if err != nil {
		return failure(release, err)
	}
	if err := cosign.Verify(fetchCtx, policy, checksums, bundle); err != nil {
		if fetchCtx.Err() != nil {
			return failure(release, fetchCtx.Err())
		}
		var verify *releasefetch.VerifyError
		if errors.As(err, &verify) {
			return verifyRefusal(env, release, verify)
		}
		return failure(release, err)
	}
	// Only now, with those very bytes verified, is checksums.txt parsed.
	digests, err := releasefetch.ParseChecksums(checksums, release)
	if err != nil {
		return clierr.Newf("Refusing to pin %s: the verified %s. Nothing was pinned.", text.Printable(release), err)
	}

	archiveName := releasefetch.ArchiveName(release, platform)
	archive, err := fetcher.Get(fetchCtx, base+archiveName, releasefetch.MaxArchive)
	if err != nil {
		return failure(release, err)
	}
	if found := binstore.Hash(archive); found != digests[platform] {
		return clierr.Newf("Refusing to pin %s: %s has sha256 %s, but the verified checksums.txt lists %s. Nothing was pinned.",
			text.Printable(release), archiveName, found, digests[platform])
	}
	binary, err := releasefetch.ExtractTreerig(archive, releaseLimits)
	if err != nil {
		return clierr.Newf("Refusing to pin %s: the archive %s %s. Nothing was pinned.", text.Printable(release), archiveName, err)
	}
	if err := teardown.CheckExecutable(binary, platform); err != nil {
		return clierr.Newf("Refusing to pin %s: the treerig in %s is not a %s executable (%s). Nothing was pinned.", text.Printable(release), archiveName, platform, err)
	}
	cancel() // the prompt is not part of the download

	binaryDigest := binstore.Hash(binary)
	checksumsDigest := binstore.Hash(checksums)
	cosignRow := text.Printable(cosign.Path)
	if cosign.Canonical != cosign.Path {
		cosignRow += " -> " + text.Printable(cosign.Canonical)
	}
	cosignRow += "  (" + text.Printable(cosign.Version) + ")"
	return installPin(env, options, binary, release, platform, func(now time.Time) teardown.Pin {
		return teardown.ReleasePin(release, base, policy.Signer(), checksumsDigest, digests, platform, binaryDigest, now)
	}, func(existing teardown.Pin, found bool, outcome teardown.Outcome) string {
		return pinPlan(existing, found, outcome, platform, binaryDigest, [][2]string{
			{"signer", text.Printable(policy.Signer()) + "  (cosign verified)"},
			{"cosign", cosignRow},
		})
	})
}

// loggingFetcher tells the user what is being fetched, since a stalled body
// otherwise waits out the whole timeout in silence.
type loggingFetcher struct {
	inner releasefetch.Fetcher
	log   logger.Logger
}

// Get logs the URL on stderr, then fetches it.
func (l loggingFetcher) Get(ctx context.Context, url string, limit int64) ([]byte, error) {
	l.log.Info("Downloading " + text.Printable(url) + "…")
	return l.inner.Get(ctx, url, limit)
}

// fileFormCommand is the by-hand fallback some cosign refusals offer.
func fileFormCommand(env *Env) string {
	return fmt.Sprintf("`%s teardown pin --file PATH --sha256 HEX --release LABEL`", env.CommandName)
}

// networkAdvice follows a refusal that the network may have caused.
const networkAdvice = "Check your network connection and HTTPS_PROXY settings, then run it again."

// durationText writes d as the user would: "10 minutes", not "10m0s".
func durationText(d time.Duration) string {
	if d > 0 && d%time.Minute == 0 {
		if d == time.Minute {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", d/time.Minute)
	}
	return d.String()
}

// downloadFailure is the refusal for a fetch that failed: a 404, a body past
// its cap, a deadline, or the network.
func downloadFailure(ctx context.Context, release string, timeout time.Duration, err error) error {
	subject := text.Printable(release)
	var status *releasefetch.StatusError
	var large *releasefetch.TooLargeError
	switch {
	case errors.As(err, &status) && status.Status == http.StatusNotFound:
		return clierr.Newf("Refusing to pin %s: %s was not found (HTTP 404). Nothing was pinned. Check that the release exists and has these files.", subject, text.Printable(status.URL))
	case errors.As(err, &status):
		return clierr.Newf("Refusing to pin %s: %s answered HTTP %d. Nothing was pinned. %s", subject, text.Printable(status.URL), status.Status, networkAdvice)
	case errors.As(err, &large):
		return clierr.Newf("Refusing to pin %s: %s is larger than %s. Nothing was pinned.", subject, text.Printable(large.URL), sizeText(large.Limit))
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, context.DeadlineExceeded):
		return clierr.Newf("Refusing to pin %s: the download and verification did not finish within %s. Nothing was pinned. %s", subject, durationText(timeout), networkAdvice)
	}
	return clierr.Newf("Refusing to pin %s: %s. Nothing was pinned. %s", subject, text.OneLine(err.Error(), 300), networkAdvice)
}

// verifyRefusal words a verify-blob that failed. What it may advise depends on
// what failed: only a trust root that could not be loaded says nothing about the
// release, so only that offers the by-hand route; a signature or identity
// failure says not to trust the release; a failure it cannot tell apart gets
// cosign's words and nothing more.
func verifyRefusal(env *Env, release string, verify *releasefetch.VerifyError) error {
	subject := text.Printable(release)
	said := fmt.Sprintf("cosign said: %q", verify.Stderr)
	switch verify.Kind {
	case releasefetch.FailStart:
		return clierr.Newf("Refusing to pin %s: cosign could not be run: %s. If it cannot load its trust root, run `cosign initialize` once with network access, then run this again. Nothing was pinned.", subject, verify.Stderr)
	case releasefetch.FailTimeout:
		return clierr.Newf("Refusing to pin %s: cosign did not finish within %d seconds. If it is fetching its trust root on a slow connection, run `cosign initialize` once with network access, then run this again. Nothing was pinned.", subject, int(releasefetch.VerifyTimeout.Seconds()))
	case releasefetch.FailTrustRoot:
		return clierr.Newf("Refusing to pin %s: cosign could not load the Sigstore trust root. %s. Run `cosign initialize` once with network access, or verify the download by hand and use %s. Nothing was pinned.", subject, said, fileFormCommand(env))
	case releasefetch.FailSignature:
		return clierr.Newf("Refusing to pin %s: checksums.txt did not verify against the release workflow's signature. %s. Do not trust this release. Nothing was pinned.", subject, said)
	}
	return clierr.Newf("Refusing to pin %s: cosign could not verify checksums.txt. %s. Nothing was pinned.", subject, said)
}

// cosignRefusal words the two ways cosign can be unusable.
func cosignRefusal(env *Env, release string, err error) error {
	subject := text.Printable(release)
	var inside *releasefetch.InsideError
	var version *releasefetch.VersionError
	switch {
	case errors.As(err, &inside):
		found := text.Printable(inside.Path)
		if inside.Canonical != inside.Path {
			found += " (" + text.Printable(inside.Canonical) + ")"
		}
		return clierr.Newf("Refusing to pin %s: cosign resolves to %s, which is inside %s, where branch code could have put it. Install cosign outside any checkout, or verify the download by hand and use %s. Nothing was pinned.",
			subject, found, text.Printable(inside.Where), fileFormCommand(env))
	case errors.As(err, &version):
		what, advice := "cosign is not on PATH", "Install cosign"
		switch {
		case version.Path != "" && strings.HasPrefix(version.Found, "version "):
			what, advice = fmt.Sprintf("%s does not qualify (%s)", text.Printable(version.Path), text.Printable(version.Found)), "Upgrade cosign"
		case version.Path != "" && !strings.HasPrefix(version.Found, "`cosign version --json` failed") && !strings.HasPrefix(version.Found, "it could not be run"):
			what = fmt.Sprintf("%s does not qualify (%s)", text.Printable(version.Path), text.Printable(version.Found))
		case version.Path != "":
			// The version call itself failed: most often a version-manager shim
			// (asdf, mise), which needs the HOME and variables treerig withholds.
			what = fmt.Sprintf("%s does not qualify (%s; a version-manager shim such as asdf or mise needs an environment treerig withholds, so install a cosign binary on PATH)", text.Printable(version.Path), text.Printable(version.Found))
		case version.Found != "cosign is not on PATH":
			what = fmt.Sprintf("no usable cosign was found (%s)", text.Printable(version.Found))
		}
		return clierr.Newf("Refusing to pin %s: cosign %s or newer is required to verify it, and %s. %s, or verify the download by hand and use %s. Nothing was pinned.",
			subject, releasefetch.MinCosign, what, advice, fileFormCommand(env))
	}
	return err
}

// plural is singular when n is 1, else plural.
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return singular
	}
	return pluralForm
}

// cosignRoots are the places cosign must not live (docs/design/teardown.md,
// "Containment roots"), gathered without running git, and failing closed: a
// root that must exist and cannot be resolved is an error, never a root
// quietly left out.
//
//   - the control home, and the current checkout (the nearest ancestor of the
//     working directory holding a .git) and its repository: required;
//   - every worktree the repository's own .git/worktrees/*/gitdir files list,
//     which covers siblings that were never registered;
//   - every registered worktree whose index card says where it is.
//
// The last two are best effort per worktree: ones that cannot be read are
// counted and reported once, and a registry that cannot be read at all is
// skipped with a note, since pinning is the bootstrap.
func cosignRoots(env *Env, control layout.Control) ([]releasefetch.Root, error) {
	var roots []releasefetch.Root
	seen := map[string]bool{}
	add := func(path, where string) error {
		canonical, err := registry.CanonicalPath(path)
		if err != nil {
			return fmt.Errorf("%s (%s): %w", where, path, err)
		}
		if !seen[canonical+"\x00"+where] {
			seen[canonical+"\x00"+where] = true
			roots = append(roots, releasefetch.Root{Path: canonical, Where: where})
		}
		return nil
	}
	if err := add(control.ControlHome, "the control home"); err != nil {
		return nil, err
	}
	checkout, common, err := findCosignCheckout(env.Cwd)
	if err != nil {
		return nil, err
	}
	skipped := 0
	if checkout != "" {
		if err := add(checkout, "this checkout"); err != nil {
			return nil, err
		}
		if common != "" {
			repository := common
			if filepath.Base(common) == ".git" {
				repository = filepath.Dir(common)
			}
			if err := add(repository, "the repository of this checkout"); err != nil {
				return nil, err
			}
			listed, unreadable := listedWorktrees(common)
			skipped += unreadable
			for _, root := range listed {
				if add(root, "a worktree of this repository") != nil {
					skipped++
				}
			}
		}
	}
	scan, err := registry.ScanRegistrations(control.ControlHome)
	if err != nil {
		env.Logger.Warn(fmt.Sprintf("Could not read the registry (%s), so registered worktrees are not checked for cosign.", text.OneLine(err.Error(), 200)))
	} else {
		if n := len(scan.Problems); n > 0 {
			env.Logger.Warn(fmt.Sprintf("%d registry %s could not be read, so %s not checked for cosign.", n, plural(n, "record", "records"), plural(n, "its worktree is", "their worktrees are")))
		}
		for _, record := range scan.Records {
			indexCard, err := card.ReadIndexCard(control.ControlHome, record.RepositoryID, record.WorktreeID)
			if err != nil {
				skipped++
				continue
			}
			if indexCard != nil && indexCard.Root != "" && add(indexCard.Root, "the worktree "+record.Slug) != nil {
				skipped++
			}
		}
	}
	if skipped > 0 {
		env.Logger.Warn(fmt.Sprintf("%d %s could not be checked for cosign (an index card, a gitdir file or the worktree list could not be read).", skipped, plural(skipped, "worktree", "worktrees")))
	}
	return roots, nil
}

// maxListedWorktrees bounds how many .git/worktrees entries are read.
const maxListedWorktrees = 1000

// findCosignCheckout finds the checkout containing cwd by walking up to the first
// ancestor with a .git, and that checkout's common git directory (the
// repository's own .git, which a linked worktree's .git file reaches through
// `gitdir:` and the commondir file). checkout is "" when cwd is in no checkout;
// common is "" when the common directory cannot be known (a .git file with no
// commondir, as a submodule has). Anything that cannot be read is an error:
// this is a root that must not be dropped.
func findCosignCheckout(cwd string) (checkout, common string, err error) {
	for dir := cwd; ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(filepath.Join(dir, ".git"))
		switch {
		case err == nil && info.IsDir():
			return dir, filepath.Join(dir, ".git"), nil
		case err == nil:
			common, err := commonDirOfLinkedWorktree(dir)
			return dir, common, err
		case !errors.Is(err, fs.ErrNotExist):
			return "", "", fmt.Errorf("the current checkout (%s): %w", filepath.Join(dir, ".git"), err)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return "", "", nil
		}
	}
}

// commonDirOfLinkedWorktree follows dir/.git (a file) through `gitdir:` to the
// git directory and its commondir file. "" with no error means there is no
// commondir.
func commonDirOfLinkedWorktree(dir string) (string, error) {
	data, err := readSmallFile(filepath.Join(dir, ".git"))
	if err != nil {
		return "", fmt.Errorf("the current checkout (%s): %w", filepath.Join(dir, ".git"), err)
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", fmt.Errorf("the current checkout (%s) is a file that does not say `gitdir:`", filepath.Join(dir, ".git"))
	}
	gitDir := strings.TrimSpace(target)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	rel, err := readSmallFile(filepath.Join(gitDir, "commondir"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("the current checkout's git directory (%s): %w", gitDir, err)
	}
	common := strings.TrimSpace(string(rel))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	return filepath.Clean(common), nil
}

// listedWorktrees are the working directories the repository's
// common/worktrees/*/gitdir files name (each holds the path of that
// worktree's .git file), and how many entries could not be read. No git is
// run; it covers worktrees that were never registered with treerig.
func listedWorktrees(common string) (roots []string, unreadable int) {
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0 // a repository with no linked worktrees
	}
	if err != nil {
		// The list exists but cannot be read: every worktree in it is
		// unchecked, which the caller reports rather than passing over.
		return nil, 1
	}
	for i, entry := range entries {
		if i >= maxListedWorktrees {
			unreadable += len(entries) - i
			break
		}
		entryDir := filepath.Join(common, "worktrees", entry.Name())
		data, err := readSmallFile(filepath.Join(entryDir, "gitdir"))
		if err != nil {
			unreadable++
			continue
		}
		if gitFile := strings.TrimSpace(string(data)); gitFile != "" {
			// git 2.48+ with worktree.useRelativePaths writes a path relative to
			// this entry's directory.
			if !filepath.IsAbs(gitFile) {
				gitFile = filepath.Join(entryDir, gitFile)
			}
			roots = append(roots, filepath.Dir(gitFile))
		}
	}
	return roots, unreadable
}

// readSmallFile reads a git bookkeeping file, refusing one over 4 KiB.
func readSmallFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil && len(data) > 4096 {
		return nil, errors.New("it is larger than 4 KiB")
	}
	return data, err
}
