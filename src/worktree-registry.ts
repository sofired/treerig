import { createHash, randomUUID } from 'node:crypto';
import type { Dirent } from 'node:fs';
import { chmod, link, lstat, mkdir, readFile, readdir, realpath, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { z } from 'zod';
import { BRAND } from './brand.ts';
import { LABEL_NAMESPACE } from './config/schema.ts';
import { CliError, invariant } from './errors.ts';
import { atomicJson, pathExists, secureRegistrationDirectories } from './files.ts';
import { repositoryLayout, worktreeHost, worktreeLayout } from './layout.ts';
import type { Logger } from './logger.ts';
import {
  OPERATION_LOCK_TIMEOUT_MS,
  REGISTRY_LOCK_RETRY_MS,
  REGISTRY_LOCK_TIMEOUT_MS,
  registryLockTarget,
  withScopedLock,
  worktreeLockTarget,
  type ScopedLockOptions
} from './operation-lock.ts';
import type { CommandRunner } from './process-runner.ts';

// Identity is two UUIDs written into the Git directory: one for the repository
// (in the common dir, shared by every worktree) and one for this worktree (in
// its private git dir). Paths and branch names are descriptive and may change;
// the UUIDs are durable. The primary checkout is a worktree too, so both cases
// share one implementation.
//
// The registry is one JSON record per worktree. The domain in a record is a
// cache of a value whose truth is the checkout's committed config file, which
// changes by `git pull` with no central moment to reconcile. `ensure()` is
// therefore a reconcile: it compares the record to the config it was called
// with and repairs the record under the registry lock. Reads never mutate.

const uuidSchema = z.string().uuid();
const SLUG = /^[a-z0-9][a-z0-9-]*$/;
/** DNS labels are at most 63 characters; slugs stay under that with room for a suffix. */
const MAX_SLUG_LENGTH = 63;

const recordSchema = z.object({
  version: z.literal(1),
  repositoryId: uuidSchema,
  worktreeId: uuidSchema,
  slug: z.string().regex(SLUG),
  domain: z.string().min(1),
  /** Absent in records written before it was tracked. */
  labelNamespace: z.string().regex(LABEL_NAMESPACE).optional(),
  initialization: z.enum(['pending', 'complete'])
}).strict();

export type WorktreeInitialization = 'pending' | 'complete';

export interface RegisteredWorktree {
  repositoryId: string;
  worktreeId: string;
  /** Unique within `domain`; the worktree routes as `<slug>.<domain>`. */
  slug: string;
  /** The project domain the slug was allocated under. */
  domain: string;
  /**
   * The Docker label namespace this worktree's resources carry. Kept here,
   * in the record that removal deletes last, so a removal that was
   * interrupted can still find what it left behind.
   */
  labelNamespace?: string;
}

/** What `ensure()` returns: the record, plus where it was before if it moved. */
export interface EnsuredWorktree extends RegisteredWorktree {
  previousHost?: string;
  /** The namespace the record carried before this call rewrote it. */
  previousLabelNamespace?: string;
}

export interface WorktreeIdentity {
  root: string;
  branch: string;
  gitCommonDir: string;
  gitDir: string;
  repositoryId: string;
  worktreeId: string;
}

/** A registered worktree together with where it currently lives. */
export interface WorktreeRegistration extends RegisteredWorktree {
  root: string;
  branch: string;
}

export interface EnsuredRegistration extends WorktreeRegistration {
  previousHost?: string;
  previousLabelNamespace?: string;
}

export interface ListedWorktree {
  root: string;
  branch: string;
}

interface StoredRecord {
  initialization: WorktreeInitialization;
  registration: RegisteredWorktree;
}

/** A registry file that could not be read as a record. Reported, never thrown. */
export interface RegistryProblem {
  file: string;
  reason: string;
}

/** Hostname → the two or more worktrees claiming it. */
export type HostnameConflicts = Map<string, RegisteredWorktree[]>;

export interface RegistryScan {
  records: RegisteredWorktree[];
  problems: RegistryProblem[];
  conflicts: HostnameConflicts;
}

export interface WorktreeRegistryOptions {
  controlHome: string;
  root: string;
  /** The project domain from config; slugs are allocated unique within it. */
  domain: string;
  /** Recorded on every registration this registry writes or reconciles. */
  labelNamespace?: string;
  runner: CommandRunner;
  logger: Logger;
  /** The name the CLI was invoked as; appears in recovery hints. */
  commandName?: string;
  /** The command line as invoked, arguments space-joined; names this process to whoever waits on a lock it holds. */
  invocation?: string;
}

interface WorktreeInspectionOptions {
  createMarkers?: boolean;
}

const REPOSITORY_MARKER = path.join(BRAND.markerDir, 'repository-id');
const WORKTREE_MARKER = path.join(BRAND.markerDir, 'worktree-id');

/* ------------------------------------------------------------ identity ---- */

/** `realpath` that tolerates a trailing segment which does not exist yet. */
export async function canonicalPath(candidate: string): Promise<string> {
  let existing = path.resolve(candidate);
  const missing: string[] = [];
  while (true) {
    try {
      return path.join(await realpath(existing), ...missing);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
      const parent = path.dirname(existing);
      if (parent === existing) return path.resolve(candidate);
      missing.unshift(path.basename(existing));
      existing = parent;
    }
  }
}

async function readUuid(file: string): Promise<string | undefined> {
  if (!(await pathExists(file))) return undefined;
  const metadata = await lstat(file);
  if (
    metadata.isSymbolicLink() ||
    !metadata.isFile() ||
    await canonicalPath(file) !== path.resolve(file)
  ) {
    throw new CliError(`Unsafe identity marker: ${file}`);
  }
  const value = (await readFile(file, 'utf8')).trim();
  const parsed = uuidSchema.safeParse(value);
  if (!parsed.success) throw new CliError(`Invalid identity marker: ${file}`);
  return parsed.data;
}

async function readOrCreateUuid(file: string): Promise<string> {
  const existing = await readUuid(file);
  if (existing) return existing;

  await mkdir(path.dirname(file), { recursive: true, mode: 0o700 });
  const value = randomUUID();
  const temporary = `${file}.tmp.${process.pid}.${randomUUID()}`;
  await writeFile(temporary, `${value}\n`, { flag: 'wx', mode: 0o600 });
  try {
    // The exclusive hard link selects one identity across concurrent creators.
    await link(temporary, file);
    await chmod(file, 0o600);
    return value;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'EEXIST') throw error;
    const winner = await readUuid(file);
    invariant(winner, `Identity marker was not created correctly: ${file}`);
    return winner;
  } finally {
    await rm(temporary, { force: true });
  }
}

async function assertIdentityMarkerPath(base: string, marker: string): Promise<void> {
  invariant(
    path.dirname(path.dirname(marker)) === base &&
      await canonicalPath(marker) === marker,
    `Unsafe identity marker path: ${marker}`
  );
}

export class WorktreeIdentityService {
  /** The checkout this service answers for. */
  readonly root: string;
  private readonly runner: CommandRunner;

  constructor(root: string, runner: CommandRunner) {
    this.root = root;
    this.runner = runner;
  }

  private async gitValue(worktree: string, args: readonly string[]): Promise<string> {
    const result = await this.runner.run('git', ['-C', worktree, ...args]);
    return result.stdout.trim();
  }

  async inspect(
    worktree = this.root,
    options: WorktreeInspectionOptions = {}
  ): Promise<WorktreeIdentity | undefined> {
    const root = await canonicalPath(
      await this.gitValue(worktree, ['rev-parse', '--show-toplevel'])
    );
    const branch = await this.gitValue(root, ['branch', '--show-current']);
    const gitDir = await canonicalPath(
      await this.gitValue(root, ['rev-parse', '--path-format=absolute', '--git-dir'])
    );
    const gitCommonDir = await canonicalPath(
      await this.gitValue(root, ['rev-parse', '--path-format=absolute', '--git-common-dir'])
    );
    const repositoryMarker = path.join(gitCommonDir, REPOSITORY_MARKER);
    const worktreeMarker = path.join(gitDir, WORKTREE_MARKER);
    await assertIdentityMarkerPath(gitCommonDir, repositoryMarker);
    await assertIdentityMarkerPath(gitDir, worktreeMarker);
    const createMarkers = options.createMarkers ?? false;
    const repositoryId = createMarkers
      ? await readOrCreateUuid(repositoryMarker)
      : await readUuid(repositoryMarker);
    const worktreeId = createMarkers
      ? await readOrCreateUuid(worktreeMarker)
      : await readUuid(worktreeMarker);
    if (!worktreeId) return undefined;
    invariant(repositoryId && worktreeId, `Incomplete identity markers for ${root}.`);
    return { root, branch, gitCommonDir, gitDir, repositoryId, worktreeId };
  }

  async ensure(worktree = this.root): Promise<WorktreeIdentity> {
    const identity = await this.inspect(worktree, { createMarkers: true });
    invariant(identity, `Could not create identity markers for ${worktree}.`);
    return identity;
  }

  async repositoryId(worktree = this.root): Promise<string | undefined> {
    const root = await canonicalPath(
      await this.gitValue(worktree, ['rev-parse', '--show-toplevel'])
    );
    const gitCommonDir = await canonicalPath(
      await this.gitValue(root, ['rev-parse', '--path-format=absolute', '--git-common-dir'])
    );
    const marker = path.join(gitCommonDir, REPOSITORY_MARKER);
    await assertIdentityMarkerPath(gitCommonDir, marker);
    return readUuid(marker);
  }

  async removeWorktreeMarker(identity: WorktreeIdentity): Promise<void> {
    const marker = path.join(identity.gitDir, WORKTREE_MARKER);
    await assertIdentityMarkerPath(identity.gitDir, marker);
    const current = await readUuid(marker);
    if (!current) return;
    invariant(
      current === identity.worktreeId,
      `Worktree identity changed before cleanup completed: ${identity.root}`
    );
    await rm(marker);
  }

  async list(): Promise<ListedWorktree[]> {
    const result = await this.runner.run('git', [
      '-C', this.root, 'worktree', 'list', '--porcelain', '-z'
    ]);
    const worktrees: ListedWorktree[] = [];
    for (const block of result.stdout.split('\0\0')) {
      const fields = block.split('\0').filter(Boolean);
      const worktree = fields.find((field) => field.startsWith('worktree '))?.slice(9);
      if (!worktree) continue;
      const branch = fields
        .find((field) => field.startsWith('branch refs/heads/'))
        ?.slice('branch refs/heads/'.length) ?? '';
      worktrees.push({ root: await canonicalPath(worktree), branch });
    }
    return worktrees;
  }

  async activeIdentities(): Promise<WorktreeIdentity[]> {
    const identities: WorktreeIdentity[] = [];
    for (const worktree of await this.list()) {
      if (!(await pathExists(worktree.root))) continue;
      const identity = await this.inspect(worktree.root);
      if (identity) identities.push(identity);
    }
    return identities;
  }
}

/* --------------------------------------------------------------- slugs ---- */

/**
 * A DNS-safe label derived from the branch, or the directory name when HEAD is
 * detached. Truncation happens before trimming so the result can never end in
 * a hyphen.
 */
export function branchSlug(identity: Pick<WorktreeIdentity, 'branch' | 'root'>): string {
  const source = identity.branch || path.basename(identity.root);
  return source
    .toLowerCase()
    .replaceAll(/[^a-z0-9]+/g, '-')
    .slice(0, 48)
    .replaceAll(/^-+|-+$/g, '') || 'worktree';
}

/**
 * Chooses a slug that is free within `domain` among `others`, which must not
 * include the worktree being allocated. Preference order:
 *
 *   1. `preferred` — the slug this worktree already holds;
 *   2. the branch slug;
 *   3. the branch slug plus a hash suffix that grows until it is free.
 *
 * Slugs are sticky. A worktree keeps the slug it holds — suffix included —
 * whenever that slug is free in the target domain, and a suffix is never
 * healed automatically even when the bare name is free again. "The bare name
 * is free" is exactly the transient state a domain rename passes through, and
 * healing at that moment is how two clones end up swapping hostnames under
 * their users. Reclaiming a bare name is an explicit rename, not a side effect.
 *
 * The suffix derives from the worktree's own identity, so whichever suffix a
 * worktree is given, it is the same one every time.
 */
export function allocateSlug(
  identity: WorktreeIdentity,
  domain: string,
  others: readonly RegisteredWorktree[],
  preferred?: string
): string {
  const taken = new Set(
    others.filter((record) => record.domain === domain).map((record) => record.slug)
  );
  if (preferred !== undefined && !taken.has(preferred)) return preferred;

  const base = branchSlug(identity);
  if (!taken.has(base)) return base;

  const digest = createHash('sha256')
    .update(`${identity.repositoryId}:${identity.worktreeId}`)
    .digest('hex');
  // Longest suffix is 60 hex chars: 2 chars of base + '-' + 60 = 63.
  for (let length = 12; length <= MAX_SLUG_LENGTH - 3; length += 4) {
    const suffix = digest.slice(0, length);
    const candidate = `${base.slice(0, MAX_SLUG_LENGTH - 1 - suffix.length)}-${suffix}`;
    if (!taken.has(candidate)) return candidate;
  }
  throw new CliError(`Could not allocate a unique slug for ${identity.root}.`);
}

/* ---------------------------------------------------------------- scan ---- */

export function registryDirectory(controlHome: string, repositoryId: string): string {
  return repositoryLayout(controlHome, uuidSchema.parse(repositoryId)).registryDir;
}

export function worktreeHome(controlHome: string, repositoryId: string, worktreeId: string): string {
  return worktreeLayout(
    controlHome,
    uuidSchema.parse(repositoryId),
    uuidSchema.parse(worktreeId)
  ).home;
}

/**
 * Reads every registry on the machine. Never throws for a bad record: a file
 * that cannot be read is a `problem`, and a hostname claimed twice is a
 * `conflict`. Conflicting records are still returned — they still occupy
 * their hostname, and a lookup that lands on one must see it.
 */
export async function scanRegistrations(controlHome: string): Promise<RegistryScan> {
  const repositories = path.join(path.resolve(controlHome), 'repositories');
  const records: RegisteredWorktree[] = [];
  const problems: RegistryProblem[] = [];

  let entries: Dirent[];
  try {
    entries = await readdir(repositories, { withFileTypes: true });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') {
      return { records, problems, conflicts: new Map() };
    }
    throw error;
  }

  for (const entry of entries) {
    if (!entry.isDirectory() || !uuidSchema.safeParse(entry.name).success) continue;
    const scan = await new WorktreeStore(controlHome, entry.name).scan();
    records.push(...scan.records);
    problems.push(...scan.problems);
  }

  const byHost = new Map<string, RegisteredWorktree[]>();
  for (const record of records) {
    const host = worktreeHost(record.slug, record.domain);
    byHost.set(host, [...(byHost.get(host) ?? []), record]);
  }
  const conflicts: HostnameConflicts = new Map(
    [...byHost].filter(([, claimants]) => claimants.length > 1)
  );
  return { records, problems, conflicts };
}

/* --------------------------------------------------------------- store ---- */

/** Registrations for one repository: one JSON record per worktree. */
export class WorktreeStore {
  readonly controlHome: string;
  readonly repositoryId: string;
  readonly directory: string;
  private readonly commandName: string;
  private readonly invocation: string;

  constructor(controlHome: string, repositoryId: string, commandName: string = BRAND.name, invocation: string = commandName) {
    this.controlHome = path.resolve(controlHome);
    this.repositoryId = uuidSchema.parse(repositoryId);
    this.directory = registryDirectory(this.controlHome, this.repositoryId);
    this.commandName = commandName;
    this.invocation = invocation;
  }

  private layout(worktreeId: string) {
    return worktreeLayout(this.controlHome, this.repositoryId, uuidSchema.parse(worktreeId));
  }

  /**
   * The registry lock guards only JSON files, so its critical section is a few
   * milliseconds; it can afford to poll quickly and wait patiently. Recovery
   * is still explicit — see `clearStaleRegistryLock`.
   */
  private lockOptions(): ScopedLockOptions {
    return {
      acquireTimeoutMs: REGISTRY_LOCK_TIMEOUT_MS,
      retryIntervalMs: REGISTRY_LOCK_RETRY_MS,
      holder: this.invocation
    };
  }

  recordPath(worktreeId: string): string {
    return this.layout(worktreeId).recordPath;
  }

  /**
   * Reads one record, converting every way it can be wrong into a `CliError`
   * so a stray file never surfaces as a raw parser exception. `ENOENT` is left
   * alone: a file that vanished mid-scan is not a problem, and callers decide.
   */
  private async readStoredRecord(file: string): Promise<StoredRecord> {
    const named = uuidSchema.safeParse(path.basename(file, '.json'));
    if (!named.success) throw new CliError(`Registry file is not named by a worktree UUID: ${file}`);

    // The tool only ever writes regular files here. A symlink is either
    // tampering or a sync client, and either way it is a record we cannot
    // vouch for — so it is reported, never followed and never ignored.
    const metadata = await lstat(file);
    if (metadata.isSymbolicLink() || !metadata.isFile()) {
      throw new CliError(`Registry record must be a regular file, not a symbolic link: ${file}`);
    }

    const text = await readFile(file, 'utf8');
    let raw: unknown;
    try {
      raw = JSON.parse(text);
    } catch {
      throw new CliError(`Registry record is not valid JSON: ${file}`);
    }
    const parsed = recordSchema.safeParse(raw);
    if (!parsed.success) {
      throw new CliError(`Registry record has an unrecognized version or shape: ${file}`);
    }
    invariant(
      parsed.data.repositoryId === this.repositoryId && parsed.data.worktreeId === named.data,
      `Worktree registration identity does not match its path: ${file}`
    );
    return {
      initialization: parsed.data.initialization,
      registration: {
        repositoryId: this.repositoryId,
        worktreeId: named.data,
        slug: parsed.data.slug,
        domain: parsed.data.domain,
        ...(parsed.data.labelNamespace === undefined ? {} : { labelNamespace: parsed.data.labelNamespace })
      }
    };
  }

  /** Every readable record in this repository, plus the files that were not. */
  async scan(): Promise<{ records: RegisteredWorktree[]; problems: RegistryProblem[] }> {
    const records: RegisteredWorktree[] = [];
    const problems: RegistryProblem[] = [];
    let entries: Dirent[];
    try {
      entries = await readdir(this.directory, { withFileTypes: true });
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') return { records, problems };
      throw error;
    }
    for (const entry of entries) {
      // Directories are skipped; everything else named like a record — including
      // a symlink — goes through readStoredRecord so it is reported if it is wrong.
      if (entry.isDirectory() || !entry.name.endsWith('.json')) continue;
      const file = path.join(this.directory, entry.name);
      try {
        records.push((await this.readStoredRecord(file)).registration);
      } catch (error) {
        // Removed between readdir and readFile: another process's remove() won.
        if ((error as NodeJS.ErrnoException).code === 'ENOENT') continue;
        problems.push({ file, reason: (error as Error).message });
      }
    }
    return { records, problems };
  }

  async list(): Promise<RegisteredWorktree[]> {
    const { records } = await this.scan();
    return records.sort((left, right) => left.slug.localeCompare(right.slug));
  }

  async find(worktreeId: string): Promise<RegisteredWorktree | undefined> {
    try {
      return (await this.readStoredRecord(this.recordPath(worktreeId))).registration;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
      throw error;
    }
  }

  /**
   * Registers the worktree, or reconciles its record with the domain it was
   * called with. The whole decision — read the record, scan every other
   * registry, allocate, write — happens under the registry lock, so a rename
   * and a concurrent first registration cannot both mint the same hostname.
   *
   * A registered worktree whose domain already matches never scans; that is
   * the path every `dev` takes, and it stays cheap and immune to problems
   * elsewhere in the registry. Allocation refuses only when a record it
   * cannot read might be occupying a hostname it cannot see.
   */
  async ensure(
    worktreeId: string,
    domain: string,
    allocate: (others: readonly RegisteredWorktree[], preferred?: string) => string,
    logger: Logger,
    initialization: () => Promise<WorktreeInitialization>,
    labelNamespace?: string
  ): Promise<EnsuredWorktree> {
    await secureRegistrationDirectories(this.layout(worktreeId));
    return withScopedLock(
      registryLockTarget(this.controlHome),
      'global worktree registry',
      logger,
      async () => {
        const file = this.recordPath(worktreeId);
        let stored: StoredRecord | undefined;
        try {
          stored = await this.readStoredRecord(file);
        } catch (error) {
          if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
        }
        if (stored && stored.registration.domain === domain) {
          if (labelNamespace === undefined || stored.registration.labelNamespace === labelNamespace) {
            return stored.registration;
          }
          // Same hostname, different labels: rewrite in place; nothing to allocate.
          await atomicJson(file, recordSchema.parse({
            version: 1,
            repositoryId: this.repositoryId,
            worktreeId,
            slug: stored.registration.slug,
            domain,
            labelNamespace,
            initialization: stored.initialization
          }));
          const previous = stored.registration.labelNamespace;
          return { ...stored.registration, labelNamespace, ...(previous === undefined ? {} : { previousLabelNamespace: previous }) };
        }

        const scan = await scanRegistrations(this.controlHome);
        if (scan.problems.length > 0) {
          const listing = scan.problems.map((problem) => `  ${problem.file}: ${problem.reason}`).join('\n');
          throw new CliError(
            `Refusing to allocate a hostname while the registry has unreadable records:\n${listing}\n` +
            `Run '${this.commandName} doctor' to inspect them.`
          );
        }
        const others = scan.records.filter((record) => record.worktreeId !== worktreeId);
        const slug = allocate(others, stored?.registration.slug);
        // A caller that does not know the namespace must not erase one that is recorded.
        const namespace = labelNamespace ?? stored?.registration.labelNamespace;
        const record = recordSchema.parse({
          version: 1,
          repositoryId: this.repositoryId,
          worktreeId,
          slug,
          domain,
          ...(namespace === undefined ? {} : { labelNamespace: namespace }),
          // Only computed when a record is actually written; the fast path never asks.
          initialization: stored?.initialization ?? await initialization()
        });
        await atomicJson(file, record);

        const ensured: EnsuredWorktree = {
          repositoryId: this.repositoryId,
          worktreeId,
          slug,
          domain,
          ...(namespace === undefined ? {} : { labelNamespace: namespace })
        };
        if (stored) {
          ensured.previousHost = worktreeHost(stored.registration.slug, stored.registration.domain);
          const previous = stored.registration.labelNamespace;
          if (previous !== undefined && previous !== namespace) ensured.previousLabelNamespace = previous;
        }
        return ensured;
      },
      this.lockOptions()
    );
  }

  async initialization(worktreeId: string, expectedSlug: string): Promise<WorktreeInitialization> {
    const file = this.recordPath(worktreeId);
    invariant(await pathExists(file), `Worktree registration is missing: ${file}`);
    const stored = await this.readStoredRecord(file);
    invariant(
      stored.registration.slug === expectedSlug,
      `Worktree ${expectedSlug} no longer owns registration ${file}`
    );
    return stored.initialization;
  }

  /**
   * Rewrites the initialization state under the registry lock, leaving every
   * other field as it is. Idempotent, and refuses a record the caller no
   * longer owns.
   */
  private async setInitialization(
    worktreeId: string,
    expectedSlug: string,
    logger: Logger,
    state: WorktreeInitialization
  ): Promise<void> {
    await secureRegistrationDirectories(this.layout(worktreeId));
    await withScopedLock(
      registryLockTarget(this.controlHome),
      'global worktree registry',
      logger,
      async () => {
        const file = this.recordPath(worktreeId);
        invariant(await pathExists(file), `Worktree registration is missing: ${file}`);
        const stored = await this.readStoredRecord(file);
        invariant(
          stored.registration.slug === expectedSlug,
          `Worktree ${expectedSlug} no longer owns registration ${file}`
        );
        if (stored.initialization === state) return;
        await atomicJson(file, recordSchema.parse({
          version: 1,
          repositoryId: stored.registration.repositoryId,
          worktreeId: stored.registration.worktreeId,
          slug: stored.registration.slug,
          domain: stored.registration.domain,
          ...(stored.registration.labelNamespace === undefined ? {} : { labelNamespace: stored.registration.labelNamespace }),
          initialization: state
        }));
      },
      this.lockOptions()
    );
  }

  async completeInitialization(worktreeId: string, expectedSlug: string, logger: Logger): Promise<void> {
    await this.setInitialization(worktreeId, expectedSlug, logger, 'complete');
  }

  /**
   * The inverse: a removal is about to destroy data, so if it is interrupted
   * the next `up` must seed again rather than start on empty volumes as if
   * nothing had happened.
   */
  async markPending(worktreeId: string, expectedSlug: string, logger: Logger): Promise<void> {
    await this.setInitialization(worktreeId, expectedSlug, logger, 'pending');
  }

  async remove(worktreeId: string, expectedSlug: string, logger: Logger): Promise<boolean> {
    await secureRegistrationDirectories(this.layout(worktreeId));
    return withScopedLock(
      registryLockTarget(this.controlHome),
      'global worktree registry',
      logger,
      async () => {
        const current = await this.find(worktreeId);
        if (!current) return false;
        invariant(
          current.slug === expectedSlug,
          `Worktree ${expectedSlug} changed concurrently; its registration was retained.`
        );
        await rm(this.recordPath(worktreeId), { force: true });
        return true;
      },
      this.lockOptions()
    );
  }
}

/* ------------------------------------------------------------ registry ---- */

function registration(identity: WorktreeIdentity, record: RegisteredWorktree): WorktreeRegistration {
  return {
    repositoryId: identity.repositoryId,
    worktreeId: identity.worktreeId,
    slug: record.slug,
    domain: record.domain,
    ...(record.labelNamespace === undefined ? {} : { labelNamespace: record.labelNamespace }),
    root: identity.root,
    branch: identity.branch
  };
}

export class WorktreeRegistry {
  readonly controlHome: string;
  readonly identities: WorktreeIdentityService;
  private readonly root: string;
  private readonly domain: string;
  private readonly labelNamespace: string | undefined;
  private readonly logger: Logger;
  private readonly commandName: string;
  private readonly invocation: string;

  constructor(options: WorktreeRegistryOptions) {
    this.controlHome = path.resolve(options.controlHome);
    this.root = options.root;
    this.domain = options.domain;
    this.labelNamespace = options.labelNamespace;
    this.logger = options.logger;
    this.commandName = options.commandName ?? BRAND.name;
    this.invocation = options.invocation ?? this.commandName;
    this.identities = new WorktreeIdentityService(options.root, options.runner);
  }

  store(repositoryId: string): WorktreeStore {
    return new WorktreeStore(this.controlHome, repositoryId, this.commandName, this.invocation);
  }

  /**
   * The one mutating verb. Registers the worktree if needed, reconciles its
   * record with the current config domain, and reports a hostname change so
   * the caller can re-route. A worktree whose home already exists on disk is
   * adopted as `complete`; otherwise it is `pending` until initialization ends.
   */
  async ensure(worktree = this.root): Promise<EnsuredRegistration> {
    const identity = await this.identities.ensure(worktree);
    await secureRegistrationDirectories(
      worktreeLayout(this.controlHome, identity.repositoryId, identity.worktreeId)
    );
    // Named the way every later wait names it — by slug — once a record exists.
    const known = await this.store(identity.repositoryId).find(identity.worktreeId).catch(() => undefined);
    return withScopedLock(
      worktreeLockTarget(this.controlHome, identity.repositoryId, identity.worktreeId),
      known ? `worktree ${known.slug}` : `worktree ${identity.root}`,
      this.logger,
      async () => {
        const currentIdentity = await this.identities.inspect(worktree);
        invariant(
          currentIdentity?.repositoryId === identity.repositoryId &&
            currentIdentity.worktreeId === identity.worktreeId,
          `Worktree identity changed while registering ${identity.root}; retry the command.`
        );
        const record = await this.store(identity.repositoryId).ensure(
          identity.worktreeId,
          this.domain,
          (others, preferred) => allocateSlug(identity, this.domain, others, preferred),
          this.logger,
          async () => await pathExists(worktreeHome(
            this.controlHome,
            identity.repositoryId,
            identity.worktreeId
          )) ? 'complete' : 'pending',
          this.labelNamespace
        );
        const ensured: EnsuredRegistration = registration(currentIdentity, record);
        if (record.previousLabelNamespace !== undefined) {
          ensured.previousLabelNamespace = record.previousLabelNamespace;
          // `up` only lets this happen once nothing is left under the old namespace, so nothing is orphaned.
          this.logger.info(
            `Now labelling this worktree's resources under ${record.labelNamespace ?? ''} (was ${record.previousLabelNamespace}; nothing remained under it).`
          );
        }
        if (record.previousHost !== undefined) {
          ensured.previousHost = record.previousHost;
          this.logger.warn(
            `This worktree moved from ${record.previousHost} to ${worktreeHost(record.slug, record.domain)}. ` +
            `Run '${this.commandName} up' to route it at its new hostname.`
          );
        }
        return ensured;
      },
      { acquireTimeoutMs: OPERATION_LOCK_TIMEOUT_MS, holder: this.invocation }
    );
  }

  /** Read-only. Warns when the record disagrees with config; never repairs it. */
  async current(worktree = this.root): Promise<WorktreeRegistration | undefined> {
    const identity = await this.identities.inspect(worktree);
    if (!identity) return undefined;
    const record = await this.store(identity.repositoryId).find(identity.worktreeId);
    if (!record) return undefined;
    if (record.domain !== this.domain) {
      this.logger.warn(
        `This worktree is registered as ${worktreeHost(record.slug, record.domain)}, but ` +
        `${BRAND.configFile} now says ${this.domain}. Run '${this.commandName} up' to reconcile.`
      );
    }
    return registration(identity, record);
  }

  async list(): Promise<RegisteredWorktree[]> {
    const repositoryId = await this.identities.repositoryId(this.root);
    return repositoryId ? this.store(repositoryId).list() : [];
  }

  /** Every readable record on the machine; unreadable files are warned about, not thrown. */
  async listAll(): Promise<RegisteredWorktree[]> {
    const scan = await scanRegistrations(this.controlHome);
    for (const problem of scan.problems) {
      this.logger.warn(`Skipping unreadable registry record ${problem.file}: ${problem.reason}`);
    }
    return scan.records;
  }

  /** The full picture for `doctor`: records, unreadable files, and hostname conflicts. */
  async scanAll(): Promise<RegistryScan> {
    return scanRegistrations(this.controlHome);
  }

  /**
   * Looks a worktree up by its UUID (unique everywhere) or its slug. A slug is
   * only unique within a domain, so `domain` narrows the match. A slug found
   * under several domains is an error asking for one; a hostname claimed by
   * several worktrees is an error naming them. Never a guess.
   */
  async findGlobal(identifier: string, domain?: string): Promise<RegisteredWorktree | undefined> {
    const { records } = await scanRegistrations(this.controlHome);
    const byId = records.find((record) => record.worktreeId === identifier);
    if (byId) return byId;

    const bySlug = records.filter((record) =>
      record.slug === identifier && (domain === undefined || record.domain === domain)
    );
    if (bySlug.length <= 1) return bySlug[0];

    const domains = [...new Set(bySlug.map((record) => record.domain))].sort();
    if (domains.length === 1) {
      const claimants = bySlug.map((record) => record.worktreeId).join(', ');
      throw new CliError(
        `Hostname ${worktreeHost(identifier, domains[0] ?? '')} is registered to more than one worktree (${claimants}); ` +
        `address one by UUID or run '${this.commandName} doctor'.`
      );
    }
    throw new CliError(`Slug ${identifier} exists under ${domains.join(', ')}; specify a domain.`);
  }

  /** Registrations whose worktree Git no longer lists, or whose root is gone. */
  async pruneCandidates(): Promise<RegisteredWorktree[]> {
    const repositoryId = await this.identities.repositoryId(this.root);
    if (!repositoryId) return [];
    const active = new Set(
      (await this.identities.activeIdentities())
        .filter((identity) => identity.repositoryId === repositoryId)
        .map((identity) => identity.worktreeId)
    );
    return (await this.store(repositoryId).list()).filter(
      (record) => !active.has(record.worktreeId)
    );
  }
}
