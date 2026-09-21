import { readIndexCard } from '../checkout-index/card.ts';
import { worktreeHost } from '../layout.ts';
import type { RegisteredWorktree } from '../worktree-registry.ts';
import type { CommandContext } from './context.ts';

// Read-only. Orphan detection needs Git, so it is only offered for the
// repository the command runs in; records from other repositories are
// listed as the registry knows them.

export type WorktreeState = 'ready' | 'pending' | 'orphaned';

export interface ListedRegistration extends RegisteredWorktree {
  host: string;
  state: WorktreeState;
  /** Live from Git for this repository's worktrees; otherwise as last recorded by `up`. */
  root?: string;
  branch?: string;
  current: boolean;
}

export interface ListOptions {
  /** Every repository and domain on this machine, not just this repository. */
  all?: boolean;
  json?: boolean;
}

export async function list(context: CommandContext, options: ListOptions = {}): Promise<ListedRegistration[]> {
  const { registry, logger } = context;
  const { identities } = registry;
  const repositoryId = await identities.repositoryId();
  const current = repositoryId === undefined ? undefined : await identities.inspect();
  const active = new Map(
    repositoryId === undefined
      ? []
      : (await identities.activeIdentities()).map((identity) => [identity.worktreeId, identity] as const)
  );
  const records = options.all ? await registry.listAll() : await registry.list();

  const rows: ListedRegistration[] = [];
  for (const record of records) {
    const local = record.repositoryId === repositoryId;
    const identity = local ? active.get(record.worktreeId) : undefined;
    const initialization = await registry.store(record.repositoryId).initialization(record.worktreeId, record.slug);
    const seen = identity ?? await readIndexCard(context.controlHome, record.repositoryId, record.worktreeId);
    rows.push({
      ...record,
      host: worktreeHost(record.slug, record.domain),
      state: local && !identity ? 'orphaned' : initialization === 'pending' ? 'pending' : 'ready',
      ...(seen ? { root: seen.root, branch: seen.branch } : {}),
      current: current?.worktreeId === record.worktreeId
    });
  }
  rows.sort((left, right) => left.domain.localeCompare(right.domain) || left.slug.localeCompare(right.slug));

  if (options.json) {
    logger.output(JSON.stringify(rows, null, 2));
  } else {
    for (const line of formatTable(rows, options.all === true)) logger.output(line);
  }
  return rows;
}

export function formatTable(rows: readonly ListedRegistration[], all: boolean): string[] {
  if (rows.length === 0) {
    return [all ? 'No worktrees are registered on this machine.' : 'No worktrees are registered for this repository.'];
  }
  const columns = [
    { title: 'HOST', value: (row: ListedRegistration) => row.host },
    { title: 'STATE', value: (row: ListedRegistration) => row.state },
    { title: 'BRANCH', value: (row: ListedRegistration) => row.branch ?? '-' },
    { title: 'ROOT', value: (row: ListedRegistration) => row.root ?? '-' }
  ];
  const widths = columns.map((column) =>
    Math.max(column.title.length, ...rows.map((row) => column.value(row).length))
  );
  const line = (marker: string, cells: readonly string[]): string =>
    `${marker} ${cells.map((cell, index) => cell.padEnd(widths[index] ?? 0)).join('  ')}`.trimEnd();
  return [
    line(' ', columns.map((column) => column.title)),
    ...rows.map((row) => line(row.current ? '*' : ' ', columns.map((column) => column.value(row))))
  ];
}
