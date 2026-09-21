import { invariant } from './errors.ts';

/** A filesystem-safe UTC timestamp, e.g. `20260919T153000Z`. */
export function timestamp(): string {
  return new Date().toISOString().replaceAll(/[-:]/g, '').replace(/\.\d{3}Z$/, 'Z');
}

export function validateId(id: string): void {
  invariant(
    /^[A-Za-z0-9._-]+$/.test(id),
    'Backup IDs may contain only letters, numbers, periods, underscores, and hyphens.'
  );
}
