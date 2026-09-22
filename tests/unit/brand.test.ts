import { access } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { BRAND, cliName } from '../../src/brand.ts';

describe('the names BRAND carries', () => {
  it('names an example config that is actually in the tree', async () => {
    // The missing-config error points people at this file; a rename that left
    // it behind would put a dead pointer in the one message meant to unblock.
    await expect(access(fileURLToPath(new URL(`../../${BRAND.exampleFile}`, import.meta.url))))
      .resolves.toBeUndefined();
  });
});

describe('cliName', () => {
  it('follows the installed binary or symlink name', () => {
    expect(cliName(['node', '/usr/local/bin/duke'])).toBe('duke');
    expect(cliName(['node', '/opt/tools/wt'])).toBe('wt');
  });

  it('falls back to the product name when running the source directly', () => {
    expect(cliName(['node', '/repo/src/cli/main.ts'])).toBe(BRAND.name);
    expect(cliName(['node'])).toBe(BRAND.name);
  });

  it('strips a script extension from a launcher name', () => {
    expect(cliName(['node', '/somewhere/duke.mjs'])).toBe('duke');
  });
});
