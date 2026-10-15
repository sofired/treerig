# Teardown pin

`worktree remove` runs a pinned, trusted build of berth, never the code of the
build that was invoked (#85). This note says where the pin lives, what it
holds, how it is installed and verified, how removal uses it, and what it does
not protect against. The pin is stored state, so it follows `state.md`.

**Status.** The pin store, `berth teardown pin` in all three forms (`--release X.Y.Z`,
`--latest` and `--file`) and `teardown status` exist (steps 2 to 4), and the
`release-snapshot` job pins the snapshot with a real cosign and checks the packages'
binaries (step 5). The pin command in the messages below is `berth teardown pin
--release X.Y.Z` while the repository has only pre-releases; when the first
release that is not a pre-release is published, `teardownPinCommand` becomes
`--latest`, the one function that names it, and the goldens that quote it are
regenerated. The removal gate and `--dev-teardown` are tracked in #146 and the
cross-platform smoke test in #147; both wait for the first tag, `v0.1.0-rc.1`. Until the gate lands,
`worktree remove` is unchanged and nothing but this note, `state.md`,
`release.md`, `AGENTS.md`, the README's "Teardown pin" section and the help of
the `teardown` commands mentions it. Text marked **[gate]** describes what
exists only after #146; the strings #146 changes are listed under "Order of
work".

## Decisions

| # | Decision |
| --- | --- |
| 1 | The commands are `berth teardown pin` and `berth teardown status`. |
| 2 | Downloads are verified by a cosign shell-out; `--file --sha256` serves users without cosign. No `gh attestation` check, and sigstore-go is not embedded. |
| 3 | The boundary is berth's own code. A hook later added to removal needs its own trust rule. |
| 4 | The store, the commands, verification and the CI tests come first; the gate waits for the first tag. |
| 5 | The pin protects against unreviewed branch code, not against a hostile process of the same user. Branch code that changes `PATH` or sigstore settings (a `.envrc`) is in scope for the cosign run; a process that can write the control home is not. |
| 6 | `down` and `restore` are out of scope. |
| 7 | `teardown status` and the removal refusal hint at the pin command when the running build is newer than the pin. Nothing updates the pin on its own. |
| 8 | There is one pin per control home. |
| 9 | `--yes` is allowed on `teardown pin`. |

**The pin command**, in every message below, is `berth teardown pin --latest`,
or `berth teardown pin --release X.Y.Z` while the repository has only
pre-releases (`--latest` never selects one); it is the latter until the first
release that is not a pre-release exists.

## Rules

1. **No branch code runs after the gate [gate].** `worktree remove` re-executes
   the pinned binary unless it already is that binary, and refuses when there is
   no usable pin. The invoked build parses its flags before the gate, so a flag
   error or `--help` comes from it; neither removes anything.
2. **A pin is installed by an explicit, approved command.** Nothing pins, updates
   or unpins implicitly, and nothing downloads on its own.
3. **Trust is decided once, at pin time.** Afterwards the recorded sha256 is the
   anchor, rechecked at every use, so removal needs no network and no verifier.
4. **The developer escape hatch is a per-invocation flag [gate].** It is never a
   setting, an environment variable or a default.
5. **Ownership is unchanged.** The pinned build proves ownership by labels, as
   every build does; the pin adds a precondition and moves no check.

## Scope and threat model

`worktree remove` runs no project hooks (the hook stages are prepare, seed,
verify, backup and restore), and `berth.toml` and Compose files are data, so the
code to pin is berth's own. A hook later added to removal needs its own trust
rule. `down` and `restore` are out of scope.

The pin protects against **unreviewed branch code running as berth during a
removal**. It does not protect against a hostile process running as the same
user with write access to the control home, which can rewrite the pin as easily
as anything else there. A root-owned location is not a goal.

**Verification trusts the user's `PATH`.** `teardown pin` runs the `cosign` it
finds there, and a fake one can report success. Embedding sigstore-go, which
would remove that dependence, was declined (decision 2). So:

- Run `teardown pin` from a shell you trust, not from a directory whose `.envrc`
  or similar you have not read. Pinning is the user's one deliberate trust act;
  the gate protects the removals after it. Branch code that controls `PATH` at pin
  time could equally replace `berth` itself, so nothing here claims to defend that.
- berth narrows the common case: it refuses a cosign that resolves inside a
  checkout or the control home (below), and gives cosign a fixed environment
  (below), so that no `.envrc` can choose its trust root.
- **Accepted limit:** a cosign elsewhere on a hijacked `PATH`, outside every
  checkout and the control home, is trusted.
- **Not supported:** a cosign that is a version-manager shim (asdf, mise). The
  allowlist withholds `HOME`, `ASDF_*` and `MISE_*` on purpose, so such a shim
  cannot run; its version call fails and the refusal says to install a cosign
  binary on `PATH`.

## Where the pin lives

| Item | Path | Version |
| --- | --- | --- |
| Pin file | `<control home>/teardown/pin.json` (`layout.Control.PinPath`) | `"version": 1`, `stateformat.TeardownPin` |
| Pinned teardown binaries | `<control home>/teardown/<binary sha256>` (`layout.Control.TeardownDir`) | none: the name is the version |
| cosign's cache | `<control home>/teardown/cosign/{tuf,tmp}` (`layout.Control.CosignHome`, `CosignTUFRoot`, `CosignTmp`) | none: a derived cache, safe to delete |

One pin per control home, shared by every berth root that uses it. `teardown/` is
separate from `bin/`: `bin/` holds unvetted copies of whatever was invoked and is
swept by age and count. Directories are `0700` and created by
`files.SecureTeardownDirectory`, which mirrors `SecureBinDirectory`; the pin
file is `0600`, written with `files.AtomicJSON`. Every read and write of the pin
first passes `files.AssertSafeFilePath`. Binaries are stored with
`binstore.Install` (mode `0700`) and `binstore.Hash`/`Path`. The update rules
below are `teardown.Update`, a pure function; `teardown.Save` is the writer
(binary, then `pin.json`, then the sweep) and refuses a pin that does not name
the bytes it installs.

```json
{
  "version": 1,
  "release": "1.2.3",
  "source": "https://github.com/sofired/berth/releases/download/v1.2.3/",
  "signer": "https://github.com/sofired/berth/.github/workflows/release.yml@refs/tags/v1.2.3",
  "checksums_sha256": "<sha256 of the verified checksums.txt>",
  "pinned_at": "2026-10-14T12:00:00Z",
  "platforms": {
    "linux_amd64":  { "archive_sha256": "…" },
    "linux_arm64":  { "archive_sha256": "…" },
    "darwin_amd64": { "archive_sha256": "…" },
    "darwin_arm64": { "archive_sha256": "…", "binary_sha256": "…" }
  }
}
```

| Field | Rule |
| --- | --- |
| `release` | A release pin: the version, matching the pattern below and parsing as strict SemVer (`internal/semver`). A file pin: `--release`, `[0-9A-Za-z][0-9A-Za-z._+-]{0,63}`. |
| `source` | The `https://…/releases/download/vX.Y.Z/` base (host required; no user, query or fragment; the `v<release>` path segment must equal `release`, so `vX.Y.Z` or `vX.Y.Z-pre`), or `file:<absolute, cleaned path>`. A `file:` source is what makes it a file pin. |
| `signer` | The cosign identity that was verified: for a release pin, non-empty, with no whitespace and no control characters. Empty for a file pin. |
| `checksums_sha256` | The hash of the verified `checksums.txt`; empty for a file pin. |
| `pinned_at` | RFC 3339, UTC, `Z` suffix. |
| `platforms` | Keyed `<GOOS>_<GOARCH>` of the process that pins and the one that removes (a darwin/amd64 build under Rosetta is `darwin_amd64`). Only four keys exist: `linux_amd64`, `linux_arm64`, `darwin_amd64` and `darwin_arm64`. A release pin has all four; a file pin has exactly one entry, the machine it was made on. At least one entry names a binary. |
| `archive_sha256` | Lowercase 64-hex; empty for a file pin, which has no archive. Recorded for all four platforms of a release pin, from the one verified `checksums.txt`. Kept for platforms not installed here because it records what was signed, lets `status` name the platforms with no binary, and lets a later `pin` for another platform of the same release merge without a new decision. |
| `binary_sha256` | Lowercase 64-hex hash of the extracted `berth`: the **runtime trust anchor**. `checksums.txt` lists archive digests only, so it is computed at install, after the archive digest matched. Present only for platforms installed on this machine, and omitted rather than written empty: `"binary_sha256": ""` is foreign. Required (and the only digest) in a file pin's entry. |

**Unknown keys are foreign.** Keys are matched exactly (`Release` is not
`release`), and a key may not appear twice in one object. Every key is required
except a platform's `binary_sha256`. None may be `null`. This holds at the top
level and in each platform entry.

A pin carrying a key this build does not know, at a version it reads, is not a
pin this build wrote: a new key is a version bump (`state.md`, "Adding a
migration"), and a security record is not parsed leniently. Version is judged
first, on its own (`state.md`, rule 4).

**Reading.** `teardown.ReadPin` is the one reader, used by `pin`, `status` and
the gate. It returns the pin, no pin, or a refusal:

| Found | Result |
| --- | --- |
| Newer `version` | `stateformat.Newer("use", "the teardown pin <path>", …)`: `state.md`'s refusal, "Upgrade berth". |
| No version, an unknown version, an unknown key, a duplicate key or a bad field | Foreign: the text below. Never "Upgrade berth"; never rewritten. |
| `pin.json` is a symlink or not a regular file, or the directory is | `Refusing symlinked or non-file managed path: <path>` (`files.AssertSafeFilePath`). |
| The file cannot be read | `Could not read the teardown pin <path>: <reason>. Check its permissions, or delete <control home>/teardown/ and pin again.` `<reason>` is the system error without the path (`permission denied`). The gate re-wraps it in its own refusal. |

```
Refusing to use the teardown pin <path>: it is not a pin this build wrote. Delete <control home>/teardown/ and pin again.
```

## Installing or updating the pin

```
berth teardown pin --release X.Y.Z | --latest   [--yes]
berth teardown pin --file PATH --sha256 HEX --release LABEL   [--yes]
berth teardown status [--json]
```

`--release` means "which release" everywhere: with `--file` it is the label the
pin carries, and without it it is the version to download. `--release` and
`--latest` are mutually exclusive, and `--latest` takes no label. `--file`
requires `--sha256` and `--release` and excludes `--latest`; `--sha256` is for
`--file` only; a download takes `--release` or `--latest`. The refusals, in the
order they are checked:

```
--latest cannot be used with --file: a file pin is for a binary you verified yourself.
Missing required flag: --sha256.            (or --release; "Missing required flags: --sha256, --release." for both)
--sha256 can only be used with --file.
--release and --latest cannot be used together.
Missing a source to pin: pass --release X.Y.Z or --latest to download and verify a release, or --file with --sha256 and --release to pin a binary you have verified.
--release must be a version such as 1.2.3 or 0.1.0-rc.1, without a leading v.
```

(The root's persistent `-V, --version` is the build's own version, so no pin flag
uses that name.) The usage line shows the three forms as `(--release X.Y.Z |
--latest | --file PATH)`, and the description spells out the three forms, because
the full `--file` triple does not fit the 80-column usage line.

There is no `unpin`: deleting `<control home>/teardown/` does it. `--yes` is
"skip the confirmation phrase", as on `worktree remove`.

### Sources

| Source | Flow |
| --- | --- |
| `--release X.Y.Z` | The value must match release.yml's pattern `^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`. `0.1.0-rc.1` passes, and so does the snapshot's `0.0.0-dev+<sha12>` (a pre-release part and a build part), which is why the CI pin flow runs unchanged against it. Everything is fetched from `https://github.com/sofired/berth/releases/download/vX.Y.Z/`. |
| `--latest` | **Discovery only.** Fetches `…/releases/latest/download/checksums.txt`, reads the version from the archive names in it (they must all agree and match the pattern), and discards the bytes. Then it behaves as `--release` with that value and refetches everything, `checksums.txt` included, from the versioned URL. `releases/latest` never selects a pre-release, so before a stable release exists it answers 404 and the command refuses (below). |
| `--file` | Reads the file once into memory (at most 200 MiB), hashes those bytes, compares with `--sha256`, and `binstore.Install`s the same bytes. For users without cosign and for development. |

```
Refusing to pin the latest release: sofired/berth has no release that is not a pre-release yet (or the repository is not reachable). Pin a pre-release with `berth teardown pin --release X.Y.Z`. Releases are listed at https://github.com/sofired/berth/releases.
```

**What `--file` does, in order.** It checks the flags (all missing required flags are named in one line;
`--sha256` is 64 hex
digits, in either case, compared in lowercase; `--release` fits the label
pattern; this platform is one of the four), reads the file once, hashes those
bytes and compares, checks that they are an executable for this platform, reads the pin (a newer or foreign one refuses here),
shows the plan, asks, reads the pin again, and calls `teardown.Update` and
`teardown.Save` with the bytes it hashed. The second read is the
read-modify-write's: the plan was drawn from a read a prompt old, and nothing
locks the pin, so a pin that became newer or foreign while the prompt was up is
refused rather than overwritten; a writer landing after that read still loses to
this one (last writer wins). The plan shows the full digest, not an elided one.
A relative `--file` is resolved against the working directory the entry point
gave, and the source is the absolute, cleaned result. A mismatch refuses before
anything is asked or written:

```
Refusing to pin <release>: <path> has sha256 <found>, not the <given> you supplied. Nothing was pinned. Check that you are pinning the file you verified and that --sha256 is its digest.
```

A digest that matches is the approval, but it does not make any file a pin. After the
comparison, `teardown.CheckExecutable` reads only the headers (`debug/elf` for
`linux_*`, `debug/macho` for `darwin_*`) and refuses an empty file, a file of
another format or one built for another architecture (a fat Mach-O passes when it
holds this one), so `status` cannot report such a file as intact:

```
Refusing to pin <release>: <path> is not a <platform> executable (<reason>). Nothing was pinned. Pin the berth binary built for this machine.
```

On success, one line on stderr and nothing on stdout:
`Pinned the teardown binary: release <release> (<platform>), stored at <path>.`
(the path is named by the hash); a sweep that could not finish is a warning after it.

`--file --sha256` is also the **offline route**: a user who verified a download
by hand with the README's steps, or has no network, pins without cosign.

### Download bounds

- HTTPS only, for the base URL and every redirect; a redirect to another scheme
  is refused. One command's downloads share a 10-minute timeout.
- Size caps: `checksums.txt` and the bundle 1 MiB each, the archive 100 MiB.
- Extraction reads the gzip stream through a 500 MiB bound and accepts exactly
  one regular entry named `berth` or `./berth`, of at most 200 MiB. Other
  regular entries (including `completions/berth`) and directories are skipped
  and never written. A link entry of any kind, an absolute name, a name with
  `..`, a duplicate `berth` or a missing one refuses the archive.
- The fetcher is an interface (`releasefetch.Fetcher`) whose one implementation
  wraps an `*http.Client`; `commands.Env.Releases` carries it. `cli.MainProcess`
  builds the client, with `Proxy: http.ProxyFromEnvironment` set explicitly and a
  `CheckRedirect` that refuses non-HTTPS (the fetcher also refuses a non-HTTPS
  redirect through a client that would follow one, and asks for the identity
  encoding so `net/http` does not decompress a body behind the size cap).
  `cli.Main` passes none, so a download from an in-process test refuses (`this
  process has no way to download a release`) rather than reaching the network. That is a deliberate exception to
  "values come from `getenv`": `net/http` reads the proxy variables itself, and
  `MainProcess` is the entry point, where environment reads belong. Library code
  never reads them.

### Verification

Before the pin is written to the control home. (The one thing written there
earlier is cosign's own cache, `teardown/cosign/`, below: derived, not stored
state.)

1. `checksums.txt` and `checksums.txt.sigstore.json` are written to a `0700`
   temporary directory under `teardown/cosign/tmp/`, chosen by berth and never by
   `TMPDIR`, and the bytes held in memory are the ones parsed later; the file
   cosign verified is read back and must equal them. Each fetch is announced on
   stderr (`Downloading <url>…`), since a stalled body otherwise waits out the
   timeout in silence.
2. cosign is resolved **once** with `run.ResolveExecutable`, and that
   **PATH-resolved path**, made absolute (a relative `PATH` entry against the
   working directory the entry point gave, never the process's own), is the one run for
   both steps below, so a `PATH` change between them cannot swap it. It is run as
   resolved, not as its canonical form, because a multi-call binary or wrapper
   script dispatches on `argv[0]`. For the containment test only,
   it is then canonicalised with `filepath.EvalSymlinks`, and **refused** when
   that result is inside one of the roots below. The plan shows both paths and
   the version, as in `cosign /usr/local/bin/cosign -> /opt/cosign/cosign (3.0.6)`.
   The gap between the check and the exec is accepted, as in the TOCTOU row. A
   cosign that is a symlink can be repointed inside that gap by a process that
   can write the link's directory; that is the same accepted limit as a cosign
   on a hijacked `PATH`.
3. `<cosign> version --json` is parsed and its `gitVersion` read (a leading `v`
   dropped). **3.0.0 or newer** is required, because the release workflow
   installs cosign v3.0.6 (`release.md`) and writes the protobuf bundle that
   cosign 3 reads. `devel`, a pre-release of 3.0.0, an unparsable value or
   missing output refuses.
4. `<cosign> verify-blob` runs with this argv and nothing else:

   ```
   <cosign> verify-blob
     --bundle <tmp>/checksums.txt.sigstore.json
     --certificate-identity https://github.com/sofired/berth/.github/workflows/release.yml@refs/tags/vX.Y.Z
     --certificate-oidc-issuer https://token.actions.githubusercontent.com
     <tmp>/checksums.txt
   ```

   The identity is an exact string, not a regular expression, and carries the
   version being pinned, so another tag's signature does not verify.
5. Only now is `checksums.txt` parsed: exactly one line `<64-hex>  <archive
   name>` for the exact archive name, else refuse. The archive's sha256 must
   equal it.
6. The archive is extracted as above; the `berth` bytes are hashed, giving
   `binary_sha256`.

For `--file` only the supplied digest is checked; the plan says so. There is no
sha256-only download mode (a digest fetched from the file's own source proves
nothing), and no flag skips the signature or changes the identity.

**Containment roots** (no `git` is run):

| Root | Source |
| --- | --- |
| The control home | `canonical(controlHome)`. **Required.** |
| The current checkout | Walk up from the working directory to the first ancestor holding a `.git` file or directory. **Required** when there is one: a `.git` that cannot be read or followed refuses. |
| That checkout's repository | For a worktree, follow the `.git` file's `gitdir:` to the git directory and its `commondir`, and take the parent when the common directory is named `.git`, else the directory itself; named "the repository of this checkout". With no `commondir` (a submodule) there is no repository root and none is guessed. |
| The repository's listed worktrees | Each `<common dir>/worktrees/*/gitdir` file holds the path of a linked worktree's `.git` file; its directory is a root ("a worktree of this repository"). This covers worktrees berth never registered, such as this project's `../berth.<slug>` siblings, which sit outside the repository. No `git` is run; at most 1000 entries. |
| Registered worktrees | The registry's records, each placed by the root its index card last recorded (a registration carries no path of its own, and `git` is not run to find one). Empty or absent: no roots; a record with no card contributes none, so a worktree that never ran `up` and is neither the checkout nor listed is not checked. Unreadable or of a newer format: skipped with a note on stderr, not a refusal, since pinning is the bootstrap. |

Discovery fails closed: a required root that cannot be resolved refuses the pin
(`Refusing to pin <release>: could not work out where cosign must not live: …`).
A listed or registered worktree whose card or `gitdir` file cannot be read is
skipped, counted, and reported once (`N worktrees could not be checked for
cosign (an index card, a gitdir file or the worktree list could not be read).`).
A worktree list that exists but cannot be read counts as one; only an absent
list means "no listed worktrees". Unreadable registry records are reported
once, with their count.

Each root is canonicalised with `registry.CanonicalPath`, which tolerates a path
that does not exist yet. Containment compares canonical forms with `filepath.Rel` and rejects a result
that is `..` or starts with `..` plus a separator; a string prefix is never used,
so `berth.foo` is not inside `berth`. Each ancestor of the cosign path is also
compared with each root through `os.SameFile`, on every platform (one stat per
ancestor), which sees through the case-insensitive volumes of macOS.

**The cosign contract.**

- The policy (the argv) is a small interface with one production implementation,
  keyless with the identity above; tests supply their own.
- cosign runs through `run.Runner` under a context timeout (10 seconds for
  `version`, 60 for `verify-blob`).
- Its environment is an **allowlist**, not berth's own with some variables
  removed. cosign binds every flag to a `COSIGN_<FLAG>` variable
  (`COSIGN_TRUSTED_ROOT` picks the trust root, `COSIGN_INSECURE_IGNORE_TLOG=true`
  turns the log check off), reads `SIGSTORE_*`, and finds its TUF cache from
  `HOME`, so a denylist cannot be made complete against what a checkout's
  `.envrc` can set. (A `.envrc` can set variables but cannot write the control
  home, which is why the same reasoning does not apply to the pin.) cosign gets
  `PATH` (relative entries made absolute), `LANG` and `LC_*`, `HTTP_PROXY`,
  `HTTPS_PROXY` and `NO_PROXY` in both cases, `SSL_CERT_FILE` and `SSL_CERT_DIR`,
  and three that berth sets itself: `HOME=<control home>/teardown/cosign`,
  `TUF_ROOT=<control home>/teardown/cosign/tuf` and
  `TMPDIR=<control home>/teardown/cosign/tmp`, all `0700` and checked like the
  other managed directories. `ALL_PROXY` is not passed. Checked against cosign
  v3.0.6: with only these, `TUF_ROOT` relocates the cache, the first run fetches
  it from `tuf-repo-cdn.sigstore.dev`, and the cache persists across runs. The
  directory is a cache: derived, unversioned, safe to delete, never touched by
  the sweep (which reads only regular files) or by `status`.
- Its stderr, through `text.Printable` and cut to about 2 KiB, is part of every
  refusal:

```
Refusing to pin <release>: cosign resolves to <path> [(<canonical path>, only when it differs)], which is inside <the worktree <slug> | this checkout | the repository of this checkout | a worktree of this repository | the control home>, where branch code could have put it. Install cosign outside any checkout, or verify the download by hand and use `berth teardown pin --file PATH --sha256 HEX --release LABEL`. Nothing was pinned.
Refusing to pin <release>: cosign 3.0.0 or newer is required to verify it, and cosign is not on PATH | <path> does not qualify (<found>) | no usable cosign was found (<found>). Install cosign | Upgrade cosign, or verify the download by hand and use `berth teardown pin --file PATH --sha256 HEX --release LABEL`. Nothing was pinned.
```

A failed `verify-blob` is classified from cosign's exit and its `Error:` lines
(a `WARNING:` about the trust root can precede a real signature failure), in this
order of precedence, and only what the class supports is advised:

| Class | Refusal |
| --- | --- |
| Signature or identity (`none of the expected identities matched`, `invalid signature`, `failed to verify log inclusion`, …) | `` Refusing to pin <release>: checksums.txt did not verify against the release workflow's signature. cosign said: "<stderr>". Do not trust this release. Nothing was pinned. `` The `--file` form is never offered. |
| Trust root not loaded (`trusted root is required`, TUF errors) | `` Refusing to pin <release>: cosign could not load the Sigstore trust root. cosign said: "<stderr>". Run `cosign initialize` once with network access, or verify the download by hand and use `berth teardown pin --file PATH --sha256 HEX --release LABEL`. Nothing was pinned. `` The only class that offers the file form. |
| cosign could not be started | `` … cosign could not be run (<reason>). If it cannot load its trust root, run `cosign initialize` once with network access, then run this again. Nothing was pinned. `` |
| Timeout (60 seconds) | `` … cosign did not finish within 60 seconds. If it is fetching its trust root on a slow connection, run `cosign initialize` … `` |
| Anything else | `` Refusing to pin <release>: cosign could not verify checksums.txt. cosign said: "<stderr>". Nothing was pinned. `` No advice. |

A cancellation or the download deadline ending while cosign runs is returned as
that, never as a cosign refusal. Network failures end `Nothing was pinned. Check
your network connection and HTTPS_PROXY settings, then run it again.`, and a
timeout reads `10 minutes`.

### Versions

`internal/semver` is a small hand-written comparer, not
`golang.org/x/mod/semver`, which would be a new dependency in `go.mod`.

- **Parsing is strict SemVer 2.0.** `major.minor.patch` with no leading zeros, an
  optional `-` pre-release and an optional `+` build, each a dot-separated list of
  `[0-9A-Za-z-]` identifiers that are never empty; a numeric pre-release
  identifier has no leading zero. `01.2.3`, `1.2.3-01`, `1.2.3-a..b` and
  `1.2.3-+` are rejected.
- **Precedence is section 11.** Build metadata is ignored. A pre-release ranks
  below its release. Identifiers compare left to right: numeric below
  alphanumeric, numeric by value, alphanumeric as ASCII strings, and a shorter
  list ranks below a longer one when the shared identifiers are equal. Numeric
  values are compared as big integers (digit count, then lexically), so no width
  overflows.
- **Failure is conservative.** A version that does not parse, and a development
  version (`0.0.0-dev…`), are never newer than anything, and a `gitVersion` that
  does not parse fails the cosign minimum.
- **The table test** uses section 11's ordering (`1.0.0-alpha < 1.0.0-alpha.1 <
  1.0.0-alpha.beta < 1.0.0-beta < 1.0.0-beta.2 < 1.0.0-beta.11 < 1.0.0-rc.1 <
  1.0.0`) and the rejected strings above.

### Updating an existing pin

| Existing pin | Result |
| --- | --- |
| None | Written. |
| Same `checksums_sha256` (the same release, another platform) | Merged, in this order: (1) a platform only the new pin names is added; (2) a platform both name takes the new `binary_sha256`, and keeps the existing one when the new pin recorded none; (3) platforms the new pin omits, and the existing `release`, `source` and `signer`, are kept; (4) `pinned_at` is refreshed. Two pins with one `checksums_sha256` cannot differ in a platform's `archive_sha256` (one verified `checksums.txt` lists one), so that case is unreachable; the code replaces such an entry whole, and the writer refuses a result that names no binary. |
| Any other release or source | Replaced. A file pin has an empty `checksums_sha256`, so it never merges. |
| Older or newer release than the new pin | Allowed, including a downgrade; the plan names it: `(current pin: 1.3.0; this is older)`. File labels are not compared. |
| Newer format | `pin` refuses with `state.md`'s refusal. |
| Foreign | `pin` refuses with the foreign text above, which says to delete `<control home>/teardown/`. |

### Approval

`pin` reuses `confirm` (prompt.go): the plan on stderr, then `Type this exact
phrase to continue:` with the phrase `pin teardown binary`. Without a terminal it
refuses with `Interactive confirmation required: "pin teardown binary".`; a wrong
answer gives `Confirmation did not match; operation cancelled.` Values from the
network or the file system go through `text.Printable`.

A release pin's plan:

```
Pin the teardown binary
  release   1.2.3  (current pin: 1.1.0)
  source    https://github.com/sofired/berth/releases/download/v1.2.3/
  signer    https://github.com/sofired/berth/.github/workflows/release.yml@refs/tags/v1.2.3  (cosign verified)
  cosign    /usr/local/bin/cosign -> /opt/cosign/cosign  (3.0.6)
  platform  linux_amd64  sha256 3f2a…
  `worktree remove` does not run this binary yet.
```

A `--file` pin's plan, where no signature is checked and cosign is not run:

```
Pin the teardown binary
  release   my-build  (current pin: none)
  source    file:/home/me/berth
  signer    none (digest supplied by you); no signature was checked
  platform  linux_amd64  sha256 3f2a…
  `worktree remove` does not run this binary yet.
```

### Writing, concurrency and the sweep

No lock exists for the pin, so the lock order is unchanged. A writer:

1. `binstore.Install`s the binary (atomic, idempotent, refreshes the mtime of a
   reused one);
2. replaces `pin.json` by atomic rename; the last writer wins, and a reader sees
   the old pin or the new one, never a mixture;
3. checks that the binary is still there and installs it again if it is not;
4. drops the name of any merged-in binary whose file is gone, rewriting
   `pin.json` if it did;
5. then, best-effort and after the last pin write, runs the **teardown sweep**.

`teardown.Save` is this sequence, and `teardown.Update` is the pure merge step
before it. A concurrent merge is a read-modify-write with no lock, so two
writers pinning other platforms of one release at once can lose one platform's
entry (the last writer wins). `teardown status` reports the platform with no
binary, and pinning again on the machine for that platform repairs it.

**The sweep's rules.** It reads `teardown/` with `Lstat` and considers only
regular files, never a symlink, a directory, `pin.json` or anything else.

- It removes a stored binary, named 64 lowercase hex characters, that no pin
  names.
- It removes a temporary file a killed writer left, named by
  `files.AtomicWrite`'s own format: `<64 hex>.tmp.<pid>.<suffix>` or
  `pin.json.tmp.<pid>.<suffix>`
  (`^([0-9a-f]{64}|pin\.json)\.tmp\.[0-9]+\.[0-9a-z]+$`). Without this they
  would stay until `teardown/` is deleted.
- It reads the pin on disk once, before removing anything, and keeps every
  `binary_sha256` that pin names as well as those of the pin it was given. If
  that read fails for any reason but the file being absent (an I/O error, a
  foreign or newer pin, an unsafe path), it removes nothing. So while the pin
  is foreign or newer every sweep is skipped, and temporary files from killed
  writers stay until the pin is fixed.
- It keeps any file modified within the last 10 minutes.
- It looks at each file again just before removing it, and skips one that is no
  longer a regular file or has become fresh.

The grace window protects a binary installed in the last 10 minutes: a
concurrent writer's fresh install that its own `pin.json` rename has not reached
yet, and the temporary file of a write in progress. The remaining window is a
writer stalled for more than 10 minutes between its install and its rename, or a
rename that lands between the sweep's read of the pin and its unlink: the new
pin then names a binary that is not there. That costs a refusal and a re-pin,
never a wrong binary run, because the gate checks the binary's hash at every
use.

The sweep never runs from the gate or from `status`. `binstore.Prune` is not
used: it ranks every entry of a directory by age and count under `bin/`'s policy
and has no notion of a pin file or of which entries a record names. `Save`'s
errors are internal and lowercase; the commands wrap them.

### `status`

Reports the pin's integrity on this machine and says nothing about removal until
the gate exists. Stdout carries the report; a problem is a refusal on stderr with
exit 1, and exit 0 means the pin is intact for this platform.

| State | Stdout | Exit |
| --- | --- | --- |
| Intact | `Teardown pin: release 1.2.3 (linux_amd64)`, then `source`, `signer` (`none (digest supplied by you)` for a file pin), `binary <path> sha256 <hash> ok`, `pinned <time>`, and `attestation not checked` for a release pin | 0 |
| No pin | nothing | 1 |
| Newer or foreign | nothing | 1 |
| No binary for this platform, or the binary missing or different | the report, with `missing` or `sha256 <found>, pinned <want>` | 1 |

The problem lines on stderr:

```
No teardown binary is pinned. Pin one with <pin command>.
The teardown pin has no binary for <platform>. Pin one on this machine with <pin command>.
The pinned teardown binary <path> <problem>. Pin it again with <pin command>.
```

`--json` prints one document on stdout and the same exit code:
`{"pinned", "ok", "release", "source", "signer", "pinnedAt", "platform",
"binary": {"path", "sha256", "state"}, "running": {"version", "isPin",
"newerThanPin"}}`, where `state` is `ok`, `missing`, `mismatch` or `none`. With
no pin it prints `{"pinned": false}` and exits 1.

Details the table leaves open:

- With `--json`, the problem line still goes to stderr and the exit code is the
  same; the document is printed first. A newer or foreign pin prints no document
  (the pin cannot be described), only the refusal.
- Keys are camelCase and optional keys are omitted, as for every other
  document (README, "Machine-readable output"): `signer` is absent for a file
  pin, and `binary.path` and `binary.sha256` are absent when `state` is `none`.
  A newer or foreign pin prints nothing on stdout, and README says that empty
  stdout with exit 1 means the pin could not be read.
- `binary.sha256` is the digest the pin records for this platform, and `state`
  says whether the file at `binary.path` has it; the digest found on disk is in
  the text form only. The state set stays closed: `mismatch` also covers a
  stored path that cannot be read, or is not a regular file (a symlink
  included), because the gate refuses all of them alike; the text form's
  problem line says which. `teardown.InspectBinary` makes the check, opening the
  file with `O_NOFOLLOW` and hashing the descriptor it opened, so the gate
  reuses it unchanged.
- `platform` is this machine's key. `running.isPin` is true when the running binary's bytes hash
  to this platform's `binary_sha256`, which only the real entry point can tell
  (`Env.Executable`), so it is false under `cli.Main`.
- The hint is printed in every state in which a pin was read, after the report
  and before the refusal, and only in the text form: `--json` carries
  `newerThanPin` instead. A file pin never drives it (`newerThanPin` is false):
  its release is a free label, so a file pin labelled `1.0.0` is not a claim
  about a version.

When the running build's version is newer than the pin's release (by the
comparer above), the human form adds on stderr:

```
The running build (<version>) is newer than the pinned release (<release>). Pin the new build with <pin command> to update it.
```

## How `worktree remove` uses it [gate]

The gate is the first act of `worktree remove`, before `Context` is built and
before any lock is taken. A lock's descriptor is close-on-exec and would not
survive the exec, and nothing has been mutated yet. It is not in
`RemoveWithContext`. `Env.Executable` and `Env.Exec` are the seams: nil from
`cli.Main` and from hand-built test Envs, which skips the gate, and wired only by
`cli.MainProcess`, which is where the gate fails closed
(`TestEnvBuilderRelocationWiring` pins the wiring).

The checks run in this order; every path comparison uses
`canonical(controlHome)`, symlinks resolved on both sides:

```
Gate(env, options):
  controlHome := canonical(checkedHome(env.Getenv))        -- failure: refuse
  running := canonical(executable); bytes := the running file's hash

  1. running is <controlHome>/teardown/<64-hex> and bytes == that name:   -- a pinned child
       if options.DevTeardown: refuse
       pin := ReadPin()                              -- absent, unreadable, newer, foreign: refuse
       platforms[GOOS_GOARCH].binary_sha256 != name: refuse
       print the child line; return
  2. running is <controlHome>/bin/<64-hex>, bytes == that name, options.DevTeardown:
       return                                         -- the relocated dev copy: silent
  3. options.DevTeardown:  notice; relocate through bin/; return
  4. pin := ReadPin()                                 -- none, foreign, newer: refuse
     entry := pin.platforms[GOOS_GOARCH]              -- absent or no binary_sha256: refuse
     stored := teardown/<entry.binary_sha256>
     the stored file must hash to entry.binary_sha256 -- else refuse
     print the parent notice; exec stored with the same argv and environment
                                                      -- error: refuse
```

- **Check 1 is a pinned child.** It reads the pin only to confirm membership: the
  entry for this `GOOS_GOARCH` must name the child's own hash. A stale file left
  in `teardown/` by a failed sweep or a manual copy does not run once the
  pin does not name it, and a pin replaced in the milliseconds between the exec
  and this check makes the child refuse safely (nothing is removed; run the
  command again). It requires a readable pin whose entry for this platform names
  it: an absent, unreadable, newer or foreign pin, or one that does not name it,
  all refuse, so running `teardown/<hash>` directly is no way around the pin (the
  `--dev-teardown` path is the only intended exception).
  The child line is printed only when it proceeds.
- **Check 2 is how `--dev-teardown` avoids a second notice.** Relocation
  re-executes the same argv from `bin/<hash>`, so the flag travels with it and no
  environment marker exists. A `bin/` copy run without the flag matches neither
  check 1 nor 2 and goes to check 4: a relocated copy is never trusted as the pin.
  Running a `bin/<hash>` copy by hand with `--dev-teardown` prints no notice; the
  user has chosen it twice.
- **Detection is by path and bytes.** A running binary whose bytes equal the pin,
  such as a package-managed `/usr/bin/berth`, still re-executes the stored copy,
  whose location and permissions berth controls.
- **Who speaks.** The parent prints its notice before the exec; the child prints
  its line after it and then runs `RemoveWithContext`, so the child also asks the
  confirmation (`confirm` lives in `RemoveWithContext`, remove.go).
- **It fails closed.** Every failure refuses with exit 1 as an expected failure
  (`clierr`), prints nothing on stdout and removes nothing.
- **Arguments and environment** pass through unchanged. Control-home resolution
  reads the same variables as every other command, through `env.Getenv`; the
  gate sets and reads no others. The pinned release parses the flags: one added
  later is rejected by it as unknown, and the remedy is to update the pin.
- **Exit codes** after a successful exec are the pinned build's.
- **`bin/` serves only `--dev-teardown`.** The pinned path never writes to it,
  since it would copy the very code the pin exists to avoid.

**The two lines** (stderr, through `env.Logger`, values via `text.Printable`):

```
Running the pinned teardown binary <path> (release <release>) to remove <target>.
Running as the pinned teardown binary <path> (version <version>).
```

Both lines are intended: the parent's says what it is about to run, and the
child's proves that it ran, with `brand.Version`. The "pinned code ran" test builds the pinned binary with
`-X github.com/sofired/berth/internal/brand.Version=pinned-test`, pins it,
invokes a differently versioned build, and asserts the second line, which only
the pinned binary can print.

**Refusals** share one template, `Refusing to remove <target>: <reason>. <next
step> Nothing was removed.`, where `<target>` is `the worktree <identifier>` or
`this worktree`:

```
Refusing to remove <target>: no pinned teardown binary exists, and this build is not one. Run <pin command>. To remove with this build's own code while you work on removal itself, add `--dev-teardown`. Nothing was removed.
Refusing to remove <target>: the pinned teardown binary <path> cannot be read (<error>) / has sha256 <found>, not the pinned <want>. Run <pin command> to install it again. Nothing was removed.
Refusing to remove <target>: the teardown pin has no binary for <platform>. Run <pin command> on this machine. Nothing was removed.
Refusing to remove <target>: the teardown pin <path> is not one this build wrote. Delete <control home>/teardown/ and run <pin command>. Nothing was removed.
Refusing to remove <target>: the teardown pin <path> could not be read (<error>). Run <pin command>. Nothing was removed.
Refusing to remove <target>: this teardown binary <path> is not the one the pin names, or the pin was replaced just now. Run this again; if it refuses again, run <pin command>. Nothing was removed.
Refusing to remove <target>: could not run the pinned teardown binary <path> (<error>). If the filesystem holding the control home (<control home>) cannot run programs, remount it with exec. Nothing was removed.
Refusing --dev-teardown: this is a pinned teardown binary. Nothing was removed.
```

A newer pin gets `state.md`'s refusal as it stands. When the running build is
newer than the pin, a refusal adds the hint sentence from `status`. A pinned
release older than the registrations in use refuses them with `state.md`'s
"Upgrade berth", which for a pinned child means updating the pin; the README says
so.

## The developer escape hatch [gate]

`berth worktree remove --dev-teardown` ("remove with this build's own code, not a
pinned teardown binary") runs this build's own teardown, relocated through `bin/`
so a deleted checkout cannot delete the running program. The parent prints, on
stderr, once, before the confirmation prompt; the relocated copy is silent
(check 2):

```
Removing with this build's own teardown (version <version>, sha256 <hash>), not a pinned teardown binary: code from this branch runs during the removal.
```

It must never become:

- a key in `settings.toml` or `berth.toml` (`berth.toml` is branch content, so the
  worktree would authorise itself);
- an environment variable;
- implied by a development version, `go run`, a missing pin, a missing terminal
  or `--yes`;
- remembered between invocations;
- passed down to a pinned child (check 1 refuses it).

A test walks the config schema, the settings keys and the environment names
`cmd/berth` reads, and fails if any of them names this behaviour.

## Packages

| Package | Holds |
| --- | --- |
| `internal/teardown` | The pin file (read, classify, write, merge), the sweep, the status report and the gate's decision. |
| `internal/releasefetch` | The fetcher interface, `checksums.txt` parsing, the cosign policy and its runner, archive extraction. |
| `internal/semver` | The version comparer. |
| `internal/commands` | `teardown pin`, `teardown status` and the gate's call from `Remove`. |
| `internal/cli` | The command tree and the HTTP client. |
| `internal/layout`, `internal/files`, `internal/stateformat` | `TeardownDir` and `PinPath`; `SecureTeardownDirectory`; `TeardownPin`. |

## Platforms

| Concern | Position |
| --- | --- |
| linux amd64, arm64 | Each has its own platform entry. A pin made on one has no `binary_sha256` for the other; the gate refuses there and names the fix. |
| macOS quarantine | A Go HTTP client does not set `com.apple.quarantine`, and a `--file` copy carries no xattr; the user's `--sha256` is the approval. berth never runs `xattr`. |
| macOS signing | The Go linker ad-hoc-signs darwin/arm64 binaries and the signature is in the Mach-O, so a copy executes. Releases are not Developer-ID signed or notarized. Any future signing step runs before archiving (`release.md`), so the digests describe the signed bytes. |
| noexec control home | Exec fails and the gate refuses (the last-but-one refusal above). `--dev-teardown` relocates into the same filesystem and does not help. `teardown pin` never executes the stored binary (it only checks its headers, `teardown.CheckExecutable`, before storing), so the EACCES or ENOEXEC of a filesystem that cannot run programs first appears at the gate (#146), which owns the mapping to that refusal. Executing the binary at pin time to find out earlier was rejected: it would add an exec with its own argv, environment and timeout to a command that otherwise runs only cosign, to report a failure the gate must report anyway, and the gate's tests are where the mapping is pinned. |
| Homebrew | Builds from the source archive, so its binary is not the release binary and is never the pin. A Homebrew user pins once, and again after an upgrade. |
| deb, rpm, apk | `/usr/bin/berth` is the release binary (checked in the snapshot, below). The user still runs `teardown pin`. |
| TOCTOU | The gate hashes the stored file and then executes its path. The directory is `0700` and user-owned; a process that can swap the file can edit the pin. Accepted. |

## Bootstrap

The first tag, `v0.1.0-rc.1`, is cut before the gate exists, so it cannot
contain it. A pinned child of a release without the gate runs the relocation of
its own bytes through `bin/` and then proceeds; that is accepted, because the
bytes are the pinned bytes and no unpinned code runs. The first release that
contains the gate is the one cut after #146 lands (`v0.1.0-rc.2`), and #147 pins
that release. `rc.1` is what the pin store, `teardown pin --release` and `status`
meet a real signature, real URLs and real macOS binaries on. Both are
pre-releases, which `install.sh`, Homebrew and `--latest` skip, so they are
pinned with an explicit `--release`.

## Testing

| Piece | How |
| --- | --- |
| Pin format, versions, refusals | Unit tests with `testdata` fixtures: round trip, newer, foreign, unknown key, bad fields, modes; the constant is pinned by `stateformat`'s test. |
| Update rules and the sweep | Unit tests: merge, replace, downgrade; the sweep keeps the pin's binaries and the grace window and touches only 64-hex regular files, never a symlink. |
| Download and extraction | `httptest` TLS server serving a release laid out per `release.md` (`releasefetch/releasetest`: four platform archives, a `checksums.txt` listing them, a placeholder bundle), with a client trusting its certificate; a 404, a redirect to `http`, size caps, a `--latest` that is overtaken by a newer release, hostile archives (links, absolute names, `..`, duplicates, a missing `berth`, devices, oversize and decompression bombs), a bad archive digest and a signature for another tag. Through the entry point: the built binary downloads through the proxy `HTTPS_PROXY` names. |
| cosign | A real cosign 3 with a throwaway key (see "CI"), skipped without one; and `fakerun`, which asserts the exact argv, the absolute path, the PATH-resolved path being the one run, the refusal of a path inside a worktree root, the checkout or the control home (including a sibling name that shares a prefix, which is not inside, and one directory reached by two spellings, which is: `TestLocateContainment` uses `/proc/self/root` on Linux, and no darwin case-difference test exists), the version check, the exact allowlisted environment of both cosign calls (`TestLocate`, `TestTeardownPinRelease`), the timeouts and the stderr in the refusal. |
| The gate | Unit tests with `Env.Executable` and `Env.Exec` fakes: the order of checks 1 to 4 (a pinned child with an absent, unreadable, newer, foreign or non-naming pin refuses), the exact refusal text, and that `Context` is never built on a refusal. |
| Pinned code ran | Integration test over the built binary, as described under "The two lines". No Docker for the assertion. |
| Snapshot | See "CI". |

### CI

The `release-snapshot` job has a SHA-pinned `sigstore/cosign-installer` step
(the commit `release.yml` pins) with `cosign-release: v3.0.6` (the version
`release.md` fixes), and pins the snapshot after "Verify the artifacts" (the
reproducibility step's `--clean` wipes `dist/`). A test in `internal/commands`
(`TestTeardownPinSnapshot`), given the `dist/` path and the snapshot's version in
`BERTH_SNAPSHOT_DIST` and `BERTH_SNAPSHOT_VERSION`, serves `dist/` over an
`httptest` TLS server (`releasefetch/releasetest`) and runs
`teardown pin --release` against it. It asserts all four `archive_sha256` values
equal `checksums.txt` and the host's `binary_sha256` equals the hash of the built
`berth`. The installer commit and `cosign-release:` are set in both `release.yml`
and `go.yml`; `TestCosignPinnedTheSameInBothWorkflows` (`internal/style`) fails if
they differ.

- **Hermetic signing.** The test signs a copy of `checksums.txt` with a
  throwaway key and verifies with a test-only policy, so a real cosign runs end
  to end with no public Rekor or TSA and no network. In cosign 3, `sign-blob` has
  no `--tlog-upload` (cosign 2 does): the transparency log and timestamping come
  from the signing config. Run against cosign v3.0.6 (step 4), this works:

  ```
  cosign generate-key-pair                     # COSIGN_PASSWORD="" for no password
  cosign signing-config create --no-default-fulcio --no-default-oidc \
      --no-default-rekor --no-default-tsa --out signing-config.json
  cosign sign-blob --key cosign.key --bundle checksums.txt.sigstore.json \
      --signing-config signing-config.json --yes checksums.txt
  cosign verify-blob --key cosign.pub --bundle checksums.txt.sigstore.json \
      --insecure-ignore-tlog=true checksums.txt
  ```

  The signing config (`signing-config create`, with every default service
  removed) names no Rekor or TSA, so signing makes no request, and `verify-blob`
  accepts the key-signed, tlog-less bundle only with `--insecure-ignore-tlog`
  (without it: `not enough verified log entries from transparency log: 0 < 1`).
  The test-only policy lives in the test files (`keyPolicy` in
  `internal/commands/teardown_cosign_test.go`); no key mode exists in production
  code. The real keyless identity is first exercised at #147.
- **No silent skip.** The `release-snapshot` job sets `BERTH_REQUIRE_COSIGN=1`,
  so a missing cosign, an unset `BERTH_SNAPSHOT_DIST` or `BERTH_SNAPSHOT_VERSION`,
  or a `dist/` without `checksums.txt` fails the tests instead of skipping them.
  The step also runs `go test -v` and checks its log: it fails unless
  `--- PASS:` appears for each of the four tests (`TestTeardownPinSnapshot`,
  `TestTeardownPinSnapshotTampering`, `TestTeardownPinWithRealCosign` and
  `TestTeardownPinWithRealCosignAndNoTrustRoot`), and on any `--- SKIP`, so a
  `-run` pattern that matches nothing fails too. Locally, without the variable,
  the tests skip.
- `packaging/verify-snapshot.sh` additionally asserts that the host's
  `/usr/bin/berth` in the deb (and the rpm where `rpm2cpio` and `cpio` are
  present) hashes to the archive's `berth`. Locally it says "skipped" where a
  reader is missing; with `CI` set (GitHub sets it) a missing reader fails, so the
  check cannot pass by not running. CONTRIBUTING.md's "Trying it without
  publishing" commands and its `release-signing` paragraph carry the pin flow and
  this check.
- The job runs the tests after "Verify the artifacts". They only read `dist/`, so
  the artifact upload that follows still sees every file.

**Only checkable after a tag:** the keyless identity against the real workflow,
the `releases/latest/download` redirect and asset URLs, execution on real
linux/arm64 and macOS arm64 hardware, and that a release contains the gate.

## Order of work

| Step | Depends on | Done when |
| --- | --- | --- |
| 1. This note, with the `state.md`, `release.md` and `AGENTS.md` entries and the pinned `cosign-release` | #29 | Merged |
| 2. Pin store: `layout.TeardownDir`/`PinPath`, `files.SecureTeardownDirectory`, `internal/teardown`, `internal/semver`, `stateformat.TeardownPin`, the sweep, the `state.md` refusal-table row | 1 | Round trip, newer, foreign, unknown key, update rules, modes, sweep and concurrent writers are tested |
| 3. `teardown pin --file` and `teardown status`, with the confirmation, goldens and README section | 2 | A pin can be made from a local binary and `status` verifies it |
| 4. `teardown pin --release X.Y.Z` / `--latest`: `internal/releasefetch`, cosign, extraction, per-platform digests | 3 | Works against `httptest` and the snapshot's `dist/`; a wrong-tag signature, a bad digest, a missing or old cosign, a 404 and a hostile archive each refuse. The flag rules become: `--file` requires `--sha256` and `--release`; a download takes `--release` or `--latest`. So "Missing required flag: --file." (and its plural) changes to ask for a source, and `teardownPinCommand` and the goldens that quote it, the missing-flag ones included, change to `--latest` or `--release X.Y.Z`. The `httptest` base URL must end `/releases/download/v<release>/` (the pin store refuses any other), and its fixtures carry all four platforms' archive digests. The noexec mapping (an EACCES or ENOEXEC when the stored binary is first executed, naming the mount) belongs to the gate: pin never executes the stored binary (below). Delivered in step 4: the download forms, `teardownPinCommand` and the goldens that quote it |
| 5. CI: the cosign install, the snapshot pin flow, the package-hash check, CONTRIBUTING's list | 4 | Green on a pull request, with cosign required rather than skipped (the hermetic sign and verify flags were run against cosign v3.0.6 in step 4, and "CI" above matches them) |
| 6. **#146** The fail-closed gate and `--dev-teardown`; the README's relocation paragraph; every suite that runs `worktree remove` through a binary pins first | 3, 4, the first tag | Removal without a pin or the flag refuses with the exact text; with a pin it runs the stored copy. Verify and execute through one descriptor or a copy: `teardown.InspectBinary` hashes the file it opened, but an exec of the path afterwards could run swapped bytes |
| 6b. Cut `v0.1.0-rc.2` | 6 merged | A release containing the gate is published |
| 7. **#147** Smoke test after a release | 6b | Passes on all three runners |

**#147** runs on `workflow_run` of the Release workflow (when it succeeded) and on
`workflow_dispatch` with a tag, not as part of the release. It uses SHA-pinned
actions and `permissions: contents: read`; takes the tag from
`github.event.workflow_run.head_branch`, checked against the version pattern
before use; and runs on `ubuntu-latest`, `ubuntu-24.04-arm` and `macos-latest`
with scratch `BERTH_HOME` and `BERTH_CONTROL_HOME`. It runs
`teardown pin --release 0.1.0-rc.2 --yes` (explicit, since the tag is a
pre-release), `status`, and a `worktree remove` with no terminal and no `--yes`, in a scratch checkout. It
stops at the gate's two lines: it asserts both, and accepts the refusal that
follows, so nothing is removed. The removal's `Inventory` calls Docker, which fails on a
macOS runner without it; the test accepts that refusal too.

**Strings step 6 changes** (goldens reviewed with #146; `<pin command>` is rendered
as the literal command, `--latest` or `--release X.Y.Z`, chosen once per
process): the last plan line of
`teardown pin`, from "`worktree remove` does not run this binary yet." to
"`worktree remove` will run this binary instead of the build you invoke.";
the help text of `teardown` (whose description ends "`worktree remove` does not
run the pinned binary yet.") and of `teardown status` (which says it reports
nothing about removal); the README's "Teardown pin" section, which says the
gate does not exist, and its roadmap entry; the help text of `worktree remove`
(adds `--dev-teardown`); `status` gains the line
`worktree remove: will run this binary` (intact) or `worktree remove: would
refuse` (otherwise) and keeps its exit codes; and the removal refusals, the two
lines and the warning above appear. `AGENTS.md` ("Running things safely") and the
README describe `--dev-teardown` at the same time.
