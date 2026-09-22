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
  /** The branch checked out; empty for a detached HEAD, absent when nothing knows. */
  branch?: string;
  root?: string;
  status: IndexStatus;
  services: IndexServiceLink[];
}

/** Git could not be asked which worktrees exist; what that costs the page. */
export interface IdentityFailure {
  reason: string;
  /**
   * True when the repository was identified and only its worktree list failed,
   * so its own rows are Unknown rather than Missing. False when the repository
   * itself could not be identified: then no row on the page is its own, and
   * none of them was going to be called missing anyway.
   */
  rowsUnknown: boolean;
}

export interface IndexSnapshot {
  entries: IndexEntry[];
  generatedAt: string;
  /** Set when Git could not say which worktrees of the current repository exist. */
  identityError?: IdentityFailure;
  /** False when Docker could not be asked, so no row's services could be checked. */
  dockerReachable: boolean;
  /** Why, when it could not be: the page is read by people who did not run the command. */
  dockerError?: string;
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
  /** What Docker said when it could not be asked; shown to whoever reads the page. */
  dockerError?: string;
  /** The port the router publishes; a service URL on any other is not this checkout's. */
  routerPort: number;
  /** Git could not be asked about this repository; when it can name the rows, they are unknown rather than missing. */
  identityError?: IdentityFailure;
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

    const host = worktreeHost(record.slug, record.domain);
    // A card is written into a home every lifecycle hook is handed, so where a
    // service lives is not something it gets to say: a link that is not under
    // this checkout's own host is dropped rather than published to everyone.
    const declared = (card?.services ?? []).filter((service) => {
      const url = URL.parse(service.url);
      if (url === null || url.protocol !== 'http:') return false;
      // The router's own port, too: every worktree host is loopback, so another
      // port is another listener on this machine entirely.
      if ((url.port === '' ? 80 : Number(url.port)) !== input.routerPort) return false;
      return url.hostname === host || url.hostname.endsWith(`.${host}`);
    });
    let status: IndexStatus;
    if (local && input.identityError?.rowsUnknown === true) status = 'unknown';
    else if (local && !identity) status = 'missing';
    else if (owned === undefined) status = 'unknown';
    else {
      // Counted per service the card names — which is every service that opted
      // into the index — union the containers that exist. A service that never
      // got a container at all is exactly the one a `Running` pill would lie
      // about. A checkout that names none and has none reads Stopped, which is
      // what a registration that has never been up is.
      const names = new Set([
        ...declared.map((service) => service.name),
        ...owned.map((container) => container.service)
      ]);
      // Every replica of a service carries the same Compose label. A service
      // answers only when all of its containers do — one healthy replica out of
      // three is not "every service answers" — but one that answers at all is
      // not "none do" either, which is what the legend promises Stopped means.
      const shape = [...names].map((name) => {
        const containers = owned.filter((container) => container.service === name);
        return {
          all: containers.length > 0 && containers.every((container) => container.available),
          any: containers.some((container) => container.available)
        };
      });
      status = shape.length > 0 && shape.every((service) => service.all) ? 'running'
        : shape.some((service) => service.any) ? 'partial'
        : 'stopped';
    }

    const services = declared.map((service) => ({
      ...service,
      available: status !== 'missing' &&
        (owned?.some((container) => container.service === service.name && container.available) ?? false)
    }));
    // `??`, not `||`: a detached HEAD reports an empty branch, which is an
    // answer, and must not fall through to the card's stale one.
    const branch = identity?.branch ?? card?.branch;
    // A root is a path, so empty and absent mean the same thing here; a branch
    // is not, which is why the line above has to distinguish them.
    const root = identity?.root ?? card?.root;
    return {
      repositoryId: record.repositoryId,
      worktreeId: record.worktreeId,
      project: card?.project ?? record.domain,
      domain: record.domain,
      slug: record.slug,
      host,
      ...(branch === undefined ? {} : { branch }),
      ...(root ? { root } : {}),
      status,
      services
    };
  });
  // Sorted on the key the page groups on, so a table's rows cannot interleave
  // with another's. A fixed locale, so two machines given the same input write
  // the same page — nothing pins that in this suite, since Node settles ICU's
  // default locale before a test can change it.
  // `||`, not `??`: a detached checkout has no branch to sort by, so it sorts
  // by the slug that names it instead of sorting ahead of everything.
  entries.sort((left, right) =>
    left.domain.localeCompare(right.domain, 'en') ||
    // Two projects may share a domain, and so share a table: keep each one's
    // rows together, with its gone checkouts after its live ones. The slug
    // ends it, so the order is total rather than falling through to whatever
    // order the registry happened to be read in.
    left.project.localeCompare(right.project, 'en') ||
    Number(left.status === 'missing') - Number(right.status === 'missing') ||
    (left.branch || left.slug).localeCompare(right.branch || right.slug, 'en') ||
    left.slug.localeCompare(right.slug, 'en')
  );
  return {
    entries,
    generatedAt: input.generatedAt,
    dockerReachable: input.containers !== undefined,
    ...(input.dockerError === undefined ? {} : { dockerError: input.dockerError }),
    ...(input.identityError === undefined ? {} : { identityError: input.identityError })
  };
}

/**
 * The page to write before anything has been read: no checkouts, and Docker
 * not asked. `dockerReachable: false` is the honest value — the router writes
 * this while bringing itself up, long before a container has been listed.
 */
export function placeholderSnapshot(generatedAt: string): IndexSnapshot {
  return { entries: [], generatedAt, dockerReachable: false };
}

/**
 * What a row says it is on: the branch, a detached HEAD, or that nothing has
 * recorded one. The two synthesized answers are parenthesized because
 * `detached` and `unknown` are both legal branch names — parentheses, not
 * styling, are what tells a placeholder from a ref.
 */
export function describeBranch(branch: string | undefined): { text: string; muted: boolean } {
  if (branch === undefined) return { text: '(no branch)', muted: true };
  if (branch === '') return { text: '(detached)', muted: true };
  return { text: branch, muted: false };
}
