import { CliError } from '../errors.ts';
import { worktreeHost } from '../layout.ts';
import type { RegisteredWorktree, WorktreeIdentity } from '../worktree-registry.ts';
import type { IndexCard } from './card.ts';

// Pure: registry records + cards + Git identities + a container inventory in,
// one sorted snapshot out. No I/O here, so every state a row can be in is
// reachable from a unit test.

export type IndexStatus = 'running' | 'partial' | 'stopped' | 'missing' | 'unknown';

export interface IndexServiceLink {
  name: string;
  label: string;
  purpose: string;
  url: string;
  available: boolean;
}

export interface IndexEntry {
  repositoryId: string;
  worktreeId: string;
  project: string;
  domain: string;
  slug: string;
  host: string;
  branch?: string;
  root?: string;
  status: IndexStatus;
  services: IndexServiceLink[];
}

export interface IndexSnapshot {
  entries: IndexEntry[];
  generatedAt: string;
  /** Set when Git could not say which worktrees of the current repository exist. */
  identityError?: string;
}

/** One managed container, as `docker ps` reports it. */
export interface ContainerRecord {
  repositoryId: string;
  worktreeId: string;
  service: string;
  /** Running, and not reporting itself unhealthy or still starting. */
  available: boolean;
}

/** Parses the tab-separated `docker ps` format the index service asks for. */
export function parseContainerInventory(output: string): ContainerRecord[] {
  return output.split('\n').filter(Boolean).map((line) => {
    const [state, status, repositoryId, worktreeId, service, ...extra] = line.split('\t');
    if (extra.length > 0 || !state || status === undefined || !repositoryId || !worktreeId || !service) {
      throw new CliError('Docker returned an unrecognized container inventory line.');
    }
    return {
      repositoryId,
      worktreeId,
      service,
      available: state === 'running' && !/\((?:unhealthy|health: starting)\)/i.test(status)
    };
  });
}

export interface SnapshotInput {
  registrations: readonly RegisteredWorktree[];
  /** Keyed by `${repositoryId}:${worktreeId}`. */
  cards: ReadonlyMap<string, IndexCard>;
  /** Live worktrees of the repository the command runs in. */
  identities: readonly WorktreeIdentity[];
  currentRepositoryId?: string;
  /** Undefined when Docker could not be asked. */
  containers?: readonly ContainerRecord[];
  /** Git could not list this repository's worktrees; its rows are unknown, never missing. */
  identityError?: string;
  generatedAt: string;
}

export const entryKey = (record: Pick<RegisteredWorktree, 'repositoryId' | 'worktreeId'>): string =>
  `${record.repositoryId}:${record.worktreeId}`;

export function buildSnapshot(input: SnapshotInput): IndexSnapshot {
  const identities = new Map(input.identities.map((identity) => [entryKey(identity), identity]));
  const entries = input.registrations.map((record): IndexEntry => {
    const key = entryKey(record);
    const card = input.cards.get(key);
    const identity = identities.get(key);
    const local = record.repositoryId === input.currentRepositoryId;
    const owned = input.containers?.filter((container) =>
      container.repositoryId === record.repositoryId && container.worktreeId === record.worktreeId
    );

    let status: IndexStatus;
    if (local && input.identityError !== undefined) status = 'unknown';
    else if (local && !identity) status = 'missing';
    else if (owned === undefined) status = 'unknown';
    else {
      const available = owned.filter((container) => container.available).length;
      status = available === 0 ? 'stopped' : available === owned.length ? 'running' : 'partial';
    }

    const services = (card?.services ?? []).map((service) => ({
      ...service,
      available: status !== 'missing' &&
        (owned?.some((container) => container.service === service.name && container.available) ?? false)
    }));
    const branch = identity?.branch ?? card?.branch;
    const root = identity?.root ?? card?.root;
    return {
      repositoryId: record.repositoryId,
      worktreeId: record.worktreeId,
      project: card?.project ?? record.domain,
      domain: record.domain,
      slug: record.slug,
      host: worktreeHost(record.slug, record.domain),
      ...(branch ? { branch } : {}),
      ...(root ? { root } : {}),
      status,
      services
    };
  });
  entries.sort((left, right) =>
    left.project.localeCompare(right.project) ||
    Number(left.status === 'missing') - Number(right.status === 'missing') ||
    (left.branch ?? left.slug).localeCompare(right.branch ?? right.slug)
  );
  return {
    entries,
    generatedAt: input.generatedAt,
    ...(input.identityError === undefined ? {} : { identityError: input.identityError })
  };
}
