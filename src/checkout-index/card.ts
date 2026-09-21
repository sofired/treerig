import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { z } from 'zod';
import { atomicJson } from '../files.ts';
import { worktreeLayout } from '../layout.ts';

// What a worktree publishes about itself for the machine-wide index. The
// registry record is deliberately minimal and crash-safe; the card is the
// human-facing complement, written by `up` into the worktree's own home and
// gone when the home goes. Anything the index cannot read is simply absent.

export const INDEX_CARD_FILE = 'index.json';

const cardSchema = z.object({
  version: z.literal(1),
  project: z.string().min(1),
  domain: z.string().min(1),
  slug: z.string().min(1),
  host: z.string().min(1),
  /** Last seen; the index prefers live Git identity when it has it. */
  root: z.string(),
  branch: z.string(),
  labelNamespace: z.string().min(1),
  services: z.array(z.object({
    name: z.string().min(1),
    url: z.string().min(1),
    label: z.string().min(1),
    purpose: z.string()
  }).strict()),
  updatedAt: z.string()
}).strict();

export type IndexCard = z.infer<typeof cardSchema>;

export function indexCardPath(controlHome: string, repositoryId: string, worktreeId: string): string {
  return path.join(worktreeLayout(controlHome, repositoryId, worktreeId).home, INDEX_CARD_FILE);
}

export async function writeIndexCard(controlHome: string, repositoryId: string, worktreeId: string, card: IndexCard): Promise<void> {
  await atomicJson(indexCardPath(controlHome, repositoryId, worktreeId), cardSchema.parse(card));
}

/** Missing, unreadable, or unrecognized cards all read as "no card". */
export async function readIndexCard(controlHome: string, repositoryId: string, worktreeId: string): Promise<IndexCard | undefined> {
  let text: string;
  try {
    text = await readFile(indexCardPath(controlHome, repositoryId, worktreeId), 'utf8');
  } catch {
    return undefined;
  }
  try {
    const parsed = cardSchema.safeParse(JSON.parse(text));
    return parsed.success ? parsed.data : undefined;
  } catch {
    return undefined;
  }
}
