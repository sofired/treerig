import { BRAND } from '../brand.ts';
import { atomicWrite, secureControlDirectories } from '../files.ts';
import { controlLayout } from '../layout.ts';
import type { Logger } from '../logger.ts';
import { routerLockTarget, withScopedLock } from '../operation-lock.ts';
import type { CommandRunner } from '../process-runner.ts';
import { indexUrl } from '../router/stack.ts';
import type { RegisteredWorktree, WorktreeIdentity } from '../worktree-registry.ts';
import { readIndexCard, type IndexCard } from './card.ts';
import { renderIndexHtml } from './render.ts';
import { buildSnapshot, entryKey, parseContainerInventory, type ContainerRecord, type IndexSnapshot } from './snapshot.ts';

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
}

const INDEX_LOCK_TIMEOUT_MS = 60_000;

export class CheckoutIndexService implements CheckoutIndex {
  readonly url: string;
  private readonly options: CheckoutIndexOptions;

  constructor(options: CheckoutIndexOptions) {
    this.options = options;
    this.url = indexUrl(options.routerPort);
  }

  /** One `docker ps` per label namespace in use; any failure makes the whole inventory unknown. */
  private async containers(namespaces: ReadonlySet<string>): Promise<ContainerRecord[] | undefined> {
    const records: ContainerRecord[] = [];
    for (const namespace of namespaces) {
      const format = [
        '{{.State}}',
        '{{.Status}}',
        `{{.Label "${namespace}.repository-uuid"}}`,
        `{{.Label "${namespace}.worktree-uuid"}}`,
        '{{.Label "com.docker.compose.service"}}'
      ].join('\t');
      try {
        const result = await this.options.runner.run('docker', [
          'ps', '--all', '--filter', `label=${namespace}.managed=true`, '--format', format
        ], { reject: false });
        if (result.exitCode !== 0) return undefined;
        records.push(...parseContainerInventory(result.stdout));
      } catch {
        return undefined;
      }
    }
    return records;
  }

  async snapshot(): Promise<IndexSnapshot> {
    const { registry, controlHome } = this.options;
    const registrations = await registry.listAll();
    const cards = new Map<string, IndexCard>();
    for (const record of registrations) {
      const card = await readIndexCard(controlHome, record.repositoryId, record.worktreeId);
      if (card) cards.set(entryKey(record), card);
    }
    // Git may be unavailable, or one sibling's marker unreadable; the index
    // still renders, but without a repository to compare against nothing is
    // called missing — "run worktree remove" is advice for a certainty, not a guess.
    let identities: WorktreeIdentity[] = [];
    let currentRepositoryId: string | undefined;
    let identityError: string | undefined;
    try {
      currentRepositoryId = await registry.identities.repositoryId();
      if (currentRepositoryId !== undefined) identities = await registry.identities.activeIdentities();
    } catch (error) {
      identities = [];
      const reason = error instanceof Error ? error.message : String(error);
      identityError = `Git could not list the worktrees of ${registry.identities.root}: ${reason}`;
      this.options.logger.warn(currentRepositoryId === undefined
        ? `${identityError}. The index cannot tell which of its checkouts are missing.`
        : `${identityError}. The index shows its checkouts as unknown.`);
    }
    const namespaces = new Set([BRAND.labelNamespace, ...[...cards.values()].map((card) => card.labelNamespace)]);
    const containers = await this.containers(namespaces);
    return buildSnapshot({
      registrations,
      cards,
      identities,
      ...(currentRepositoryId === undefined ? {} : { currentRepositoryId }),
      ...(containers === undefined ? {} : { containers }),
      ...(identityError === undefined ? {} : { identityError }),
      generatedAt: (this.options.now ?? (() => new Date()))().toISOString()
    });
  }

  async refresh(): Promise<IndexSnapshot> {
    const layout = controlLayout(this.options.controlHome);
    // Before the lock, so a directory that is not ours is refused before a lock file lands in it.
    await secureControlDirectories(layout);
    return withScopedLock(routerLockTarget(this.options.controlHome), 'shared router', this.options.logger, async () => {
      // Under the lock, so directory setup is serialised with the router's own `write()`.
      await secureControlDirectories(layout);
      const snapshot = await this.snapshot();
      // 0644 on purpose: readable by the cap-dropped index container through its bind
      // mount; the 0700 parents keep it off-limits to other users on disk.
      await atomicWrite(layout.indexFile, renderIndexHtml(snapshot), 0o644);
      return snapshot;
    }, { acquireTimeoutMs: INDEX_LOCK_TIMEOUT_MS });
  }

  async refreshBestEffort(context: string): Promise<void> {
    try {
      await this.refresh();
    } catch (error) {
      this.options.logger.warn(`Could not refresh the checkout index after ${context}: ${error instanceof Error ? error.message : String(error)}`);
    }
  }
}
