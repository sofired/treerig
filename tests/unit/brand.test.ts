import { describe, expect, it } from 'vitest';
import { BRAND, cliName } from '../../src/brand.ts';

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
