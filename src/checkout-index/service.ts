import { BRAND } from '../brand.ts';
import { CliError } from '../errors.ts';
import { DockerUnavailableError, dockerSaid, runDocker } from '../docker.ts';
import { assertNotGoManaged, atomicWrite, secureControlDirectories } from '../files.ts';
import { controlLayout } from '../layout.ts';
import type { Logger } from '../logger.ts';
import { routerLockTarget, withScopedLock } from '../operation-lock.ts';
import type { CommandRunner } from '../process-runner.ts';
import { indexUrl } from '../router/stack.ts';
import { printable } from '../text.ts';
import type { RegisteredWorktree, WorktreeIdentity } from '../worktree-registry.ts';
import { readIndexCard, recordedLabelNamespace, type IndexCard } from './card.ts';
import { renderIndexHtml } from './render.ts';
import { buildSnapshot, entryKey, parseContainerInventory, type IdentityFailure, type ContainerRecord, type IndexSnapshot } from './snapshot.ts';

// Regenerates the page. Runs after every lifecycle verb, best-effort: a
// stale index is an inconvenience, a failed `up` because of it would not be.

export interface CheckoutIndex {
  readonly url: string;
  refresh(): Promise<IndexSnapshot>;
  refreshBestEffort(context: string): Promise<void>;
}

/** The slice of the registry the index reads. */
export interface IndexRegistry {
  listAll(): Promise<RegisteredWorktree[]>;
  identities: {
    /** The checkout the index is refreshed from. */
    readonly root: string;
    activeIdentities(): Promise<WorktreeIdentity[]>;
    repositoryId(): Promise<string | undefined>;
  };
}

export interface CheckoutIndexOptions {
  controlHome: string;
  routerPort: number;
  registry: IndexRegistry;
  runner: CommandRunner;
  logger: Logger;
  now?: () => Date;
  /** The command line as invoked, arguments space-joined; names this process to whoever waits on the router lock. */
  invocation?: string;
}

const INDEX_LOCK_TIMEOUT_MS = 60_000;
/**
 * The index is best-effort: a daemon that will not answer must not hold the
 * router lock. Long enough not to give up on a daemon that is merely busy —
 * `up` gives the same one 45 seconds to bring a stack up — and well inside the
 * 60-second budget another command has to acquire the lock.
 */
const INVENTORY_TIMEOUT_MS = 30_000;

export class CheckoutIndexService implements CheckoutIndex {
  readonly url: string;
  private readonly options: CheckoutIndexOptions;

  constructor(options: CheckoutIndexOptions) {
    this.options = options;
    this.url = indexUrl(options.routerPort);
  }

  /** One `docker ps` per label namespace in use; any failure makes the whole inventory unknown. */
  private async containers(namespaces: ReadonlySet<string>): Promise<ContainerRecord[] | string> {
    // Together: each is an independent read, and they all run while this
    // process holds the shared router lock.
    const answers = await Promise.all([...namespaces].map((namespace) => this.inventory(namespace)));
    const records: ContainerRecord[] = [];
    for (const answer of answers) {
      // Said once, however many namespaces were asked: it is one story.
      if (typeof answer === 'string') {
        const reason = printable(answer);
        this.options.logger.warn(`The index shows service state as Unknown because containers could not be listed: ${reason}`);
        return reason;
      }
      records.push(...answer);
    }
    return records;
  }

  /** One namespace's containers, or why it could not answer. */
  private async inventory(namespace: string): Promise<ContainerRecord[] | string> {
    const format = [
      '{{.State}}',
      '{{.Status}}',
      `{{.Label "${namespace}.repository-uuid"}}`,
      `{{.Label "${namespace}.worktree-uuid"}}`,
      '{{.Label "com.docker.compose.service"}}'
    ].join('\t');
    let result;
    try {
      result = await runDocker(this.options.runner, [
        'ps', '--all', '--filter', `label=${namespace}.managed=true`, '--format', format
      ], { reject: false, timeoutMs: INVENTORY_TIMEOUT_MS });
    } catch (error) {
      // Docker being unavailable is a story about Docker; a cancellation, or
      // anything else, belongs to whoever asked.
      if (error instanceof DockerUnavailableError) return error.message;
      throw error;
    }
    if (result.timedOut) return `\`docker ps\` did not answer within ${INVENTORY_TIMEOUT_MS / 1000}s`;
    if (result.exitCode !== 0) {
      return dockerSaid(result.stderr, `\`docker ps\` exited ${result.exitCode} without saying why`);
    }
    try {
      return parseContainerInventory(result.stdout);
    } catch (error) {
      // A line this tool cannot read is a story about Docker too: the page says
      // Unknown rather than the command failing outright.
      if (error instanceof CliError) return error.message;
      throw error;
    }
  }

  async snapshot(): Promise<IndexSnapshot> {
    const { registry, controlHome } = this.options;
    const registrations = await registry.listAll();
    // Read together: this runs while the shared router lock is held.
    const cards = new Map<string, IndexCard>();
    const read = await Promise.all(registrations.map(async (record) =>
      [record, await readIndexCard(controlHome, record.repositoryId, record.worktreeId)] as const
    ));
    for (const [record, card] of read) {
      if (card) cards.set(entryKey(record), card);
    }
    // Git may be unavailable, or one sibling's marker unreadable; the index
    // still renders, but without a repository to compare against nothing is
    // called missing — "run worktree remove" is advice for a certainty, not a guess.
    let identities: WorktreeIdentity[] = [];
    let currentRepositoryId: string | undefined;
    let identityError: IdentityFailure | undefined;
    try {
      currentRepositoryId = await registry.identities.repositoryId();
      if (currentRepositoryId !== undefined) identities = await registry.identities.activeIdentities();
    } catch (error) {
      identities = [];
      const reason = printable(error instanceof Error ? error.message : String(error));
      // Which call failed decides both what to say and what it costs the page.
      const said = currentRepositoryId === undefined
        ? `Git could not identify the repository at ${registry.identities.root}: ${reason}`
        : `Git could not list the worktrees of ${registry.identities.root}: ${reason}`;
      identityError = { reason: said, rowsUnknown: currentRepositoryId !== undefined };
      this.options.logger.warn(identityError.rowsUnknown
        ? `${said}. The index shows its checkouts as unknown.`
        : `${said}. The index cannot tell which of its checkouts are missing.`);
    }
    // One question, one answer: `recordedLabelNamespace` reads the registration,
    // the card, and a quarantined home. The registration outlives the home, so
    // a worktree whose card is gone still says which namespace to ask about —
    // miss one and its containers go unlisted, which reads as Stopped.
    const recorded = await Promise.all(registrations.map(async (record) =>
      // The card this method already read answers for most of them; only a
      // record older than namespace tracking, whose home is gone or
      // quarantined, needs the helper to go back to disk.
      record.labelNamespace ?? cards.get(entryKey(record))?.labelNamespace ??
        await recordedLabelNamespace(controlHome, record)
    ));
    const namespaces = new Set([
      BRAND.labelNamespace,
      ...recorded.flatMap((namespace) => namespace === undefined ? [] : [namespace])
    ]);
    const inventory = await this.containers(namespaces);
    const containers = typeof inventory === 'string' ? undefined : inventory;
    return buildSnapshot({
      routerPort: this.options.routerPort,
      ...(typeof inventory === 'string' ? { dockerError: inventory } : {}),
      registrations,
      cards,
      identities,
      ...(currentRepositoryId === undefined ? {} : { currentRepositoryId }),
      ...(containers === undefined ? {} : { containers }),
      ...(identityError === undefined ? {} : { identityError }),
      generatedAt: (this.options.now ?? (() => new Date()))().toISOString()
    });
  }

  /** Rebuilds the snapshot from current state and writes it to the served page, under the shared router lock. */
  async refresh(): Promise<IndexSnapshot> {
    const layout = controlLayout(this.options.controlHome);
    await assertNotGoManaged(this.options.controlHome);
    // Before the lock, so a directory that is not ours is refused before a lock file lands in it.
    await secureControlDirectories(layout);
    return withScopedLock(routerLockTarget(this.options.controlHome), 'shared router', this.options.logger, async () => {
      // The lock above may have waited on another holder; checked again here,
      // right before directory setup runs under it.
      await assertNotGoManaged(this.options.controlHome);
      // Under the lock, so directory setup is serialised with the router's own `write()`.
      await secureControlDirectories(layout);
      const snapshot = await this.snapshot();
      // `snapshot()` asks Docker about every namespace in use, up to 30
      // seconds each; checked again here, immediately before the write it
      // gates, rather than only before that wait began.
      await assertNotGoManaged(this.options.controlHome);
      // 0644 on purpose: readable by the cap-dropped index container through its bind
      // mount; the 0700 parents keep it off-limits to other users on disk.
      await atomicWrite(layout.indexFile, renderIndexHtml(snapshot), 0o644);
      return snapshot;
    }, { acquireTimeoutMs: INDEX_LOCK_TIMEOUT_MS, holder: this.options.invocation });
  }

  async refreshBestEffort(context: string): Promise<void> {
    try {
      await this.refresh();
    } catch (error) {
      this.options.logger.warn(`Could not refresh the checkout index after ${context}: ${error instanceof Error ? error.message : String(error)}`);
    }
  }
}
