import { readIndexCard } from '../checkout-index/card.ts';
import { printable } from '../checkout-index/render.ts';
import { describeBranch } from '../checkout-index/snapshot.ts';
import { worktreeHost } from '../layout.ts';
import type { RegisteredWorktree } from '../worktree-registry.ts';
import type { CommandContext } from './context.ts';

// Read-only. Telling a registration from a checkout that is gone needs Git, so
// it is only offered for the repository the command runs in; records from other
// repositories are listed as the registry knows them.

/** `missing` is the index's word for the same thing: registered, but the checkout is gone. */
export type WorktreeState = 'ready' | 'pending' | 'missing';

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
      state: local && !identity ? 'missing' : initialization === 'pending' ? 'pending' : 'ready',
      ...(seen ? { root: seen.root, branch: seen.branch } : {}),
      current: current?.worktreeId === record.worktreeId
    });
  }
  // A fixed locale, as the index sorts with.
  rows.sort((left, right) => left.domain.localeCompare(right.domain, 'en') || left.slug.localeCompare(right.slug, 'en'));

  if (options.json) {
    // Escaped rather than filtered: the values stay exactly what they are, and
    // nothing a branch name carries can drive the terminal printing them.
    logger.output(JSON.stringify(rows, null, 2).replaceAll(
      /[\u007f-\uffff]/g,
      (character) => `\\u${character.charCodeAt(0).toString(16).padStart(4, '0')}`
    ));
  } else {
    for (const line of formatTable(rows, options.all === true)) logger.output(line);
  }
  return rows;
}

export function formatTable(rows: readonly ListedRegistration[], all: boolean): string[] {
  if (rows.length === 0) {
    return [all ? 'No worktrees are registered on this machine.' : 'No worktrees are registered for this repository.'];
  }
  // `printable`, before the widths are measured: a branch or a path is whatever
  // a clone put there, and this table is printed to a terminal.
  const columns = [
    { title: 'HOST', value: (row: ListedRegistration) => printable(row.host) },
    { title: 'STATE', value: (row: ListedRegistration) => row.state },
    // The index's word for a detached HEAD, so one state has one name; `-` for
    // nothing recorded, which is this table's spelling and what ROOT says beside it.
    { title: 'BRANCH', value: (row: ListedRegistration) => row.branch === undefined ? '-' : printable(describeBranch(row.branch).text) },
    { title: 'ROOT', value: (row: ListedRegistration) => row.root === undefined ? '-' : printable(row.root) }
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
