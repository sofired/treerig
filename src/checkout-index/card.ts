import { readFile, stat } from 'node:fs/promises';
import path from 'node:path';
import { z } from 'zod';
import { LABEL_NAMESPACE } from '../config/schema.ts';
import { atomicJson } from '../files.ts';
import { worktreeLayout } from '../layout.ts';

// What a worktree publishes about itself for the machine-wide index. The
// registry record is deliberately minimal and crash-safe; the card is the
// human-facing complement, written by `up` into the worktree's own home and
// gone when the home goes. Anything the index cannot read is simply absent.

export const INDEX_CARD_FILE = 'index.json';
/** Generous for the shape above, and far short of anything that could hurt. */
const MAX_CARD_BYTES = 128 * 1024;

// Everything here is bounded. A card is written into the worktree's own home,
// which every lifecycle hook is handed, and the page built from it is read by
// every operator on the machine: an unbounded field is an unbounded page.
//
// The bounds are above anything the tool itself produces, because `up` writes
// this card and a card it cannot write would stop a worktree coming up. Config
// bounds `label` and `purpose` at the same length, where a refusal names the
// field and the file; a path or a ref is whatever the filesystem and Git allow.
const TEXT = z.string().max(512);
const PATH_TEXT = z.string().max(4096);

const cardSchema = z.object({
  version: z.literal(1),
  project: TEXT.min(1),
  domain: TEXT.min(1),
  slug: TEXT.min(1),
  host: TEXT.min(1),
  /** Last seen; the index prefers live Git identity when it has it. */
  root: PATH_TEXT,
  branch: PATH_TEXT,
  /** Validated like the config field: it ends up inside Go templates handed to `docker`. */
  labelNamespace: z.string().max(253).regex(LABEL_NAMESPACE),
  services: z.array(z.object({
    name: TEXT.min(1),
    /**
     * It becomes an `href` on the index page, so only the characters RFC 3986
     * allows may enter — which is a superset of what `worktreeUrls` builds and
     * of `URL_PATH`, and excludes every control, bidi and homoglyph character
     * along with the quotes and angle brackets. Userinfo is refused outright:
     * `https://trusted@elsewhere/` reads as one host and resolves to another.
     *
     * Which host it may point at is not decided here: every field in this file
     * is self-attested, so only the registration can say where a checkout
     * lives. `buildSnapshot` drops what does not belong to it.
     */
    url: z.string().max(2048).regex(/^https?:\/\/[A-Za-z0-9\-._~%!$&'()*+,;=:@/?#[\]]+$/).refine(
      (value) => {
        try {
          const parsed = new URL(value);
          return parsed.username === '' && parsed.password === '';
        } catch {
          return false;
        }
      },
      'must be a URL with no user information'
    ),
    label: TEXT.min(1),
    purpose: TEXT
  }).strict()).max(64),
  updatedAt: TEXT
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
  return readIndexCardAt(indexCardPath(controlHome, repositoryId, worktreeId));
}

/**
 * The label namespace a worktree's resources were recorded under: the
 * registration says, or the index card does, wherever the home currently is.
 * Undefined only for a worktree registered before either recorded it.
 */
export async function recordedLabelNamespace(
  controlHome: string,
  worktree: { repositoryId: string; worktreeId: string; labelNamespace?: string }
): Promise<string | undefined> {
  if (worktree.labelNamespace !== undefined) return worktree.labelNamespace;
  const { home: live, quarantinePath } = worktreeLayout(controlHome, worktree.repositoryId, worktree.worktreeId);
  // Only the namespace, so a card the full schema refuses — an unknown key, a
  // field past its bound, a URL that is not one — still says which labels its
  // containers carry. Without it they go unlisted, and the row reads Stopped
  // while they are running.
  for (const home of [live, quarantinePath]) {
    const namespace = await readLabelNamespace(path.join(home, INDEX_CARD_FILE));
    if (namespace !== undefined) return namespace;
  }
  return undefined;
}

/** The same, for a card that is not where it usually is — inside a quarantined home, say. */
export async function readIndexCardAt(file: string): Promise<IndexCard | undefined> {
  let text: string;
  try {
    // Read at all only if it is the size of a card. Anything larger is not one,
    // and reading it would put a repository in charge of this process's memory.
    const { size } = await stat(file);
    if (size > MAX_CARD_BYTES) return undefined;
    text = await readFile(file, 'utf8');
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

const namespaceSchema = z.object({ labelNamespace: z.string().max(253).regex(LABEL_NAMESPACE) });

/** The one field a card is still useful for when the rest of it does not parse. */
async function readLabelNamespace(file: string): Promise<string | undefined> {
  try {
    const { size } = await stat(file);
    if (size > MAX_CARD_BYTES) return undefined;
    const parsed = namespaceSchema.safeParse(JSON.parse(await readFile(file, 'utf8')));
    return parsed.success ? parsed.data.labelNamespace : undefined;
  } catch {
    return undefined;
  }
}
