import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { BRAND } from '../../src/brand.ts';
import {
  composeProjectName,
  resolveControlHome,
  worktreeHost,
  worktreeLayout
} from '../../src/layout.ts';

const REPOSITORY = 'a08b4892-5111-49f1-a47a-5ab988c7f676';
const WORKTREE = '2b41d9d4-ed88-49c1-be9c-5439bdf6a431';

describe('control home resolution', () => {
  it('prefers an explicit override, then the environment, then the default', () => {
    expect(resolveControlHome({ [BRAND.homeEnv]: '/env/home' }, '/explicit')).toBe('/explicit');
    expect(resolveControlHome({ [BRAND.homeEnv]: '/env/home' })).toBe('/env/home');
    expect(resolveControlHome({})).toMatch(new RegExp(`${BRAND.defaultHomeDir}$`));
  });
});

describe('worktree layout', () => {
  it('keeps every path inside the control home', () => {
    const layout = worktreeLayout('/control', REPOSITORY, WORKTREE);
    for (const [key, value] of Object.entries(layout) as [string, string][]) {
      expect(path.relative('/control', value), key).not.toMatch(/^\.\./);
    }
  });

  it('separates locks from state so lock directories can be tiny and long-lived', () => {
    const layout = worktreeLayout('/control', REPOSITORY, WORKTREE);
    expect(layout.lockPath).toBe(`/control/locks/worktrees/${REPOSITORY}/${WORKTREE}`);
    expect(layout.developmentLockPath).toBe(`${layout.lockPath}.dev`);
    expect(layout.home).toBe(`/control/repositories/${REPOSITORY}/instances/${WORKTREE}`);
    expect(layout.recordPath).toBe(`/control/repositories/${REPOSITORY}/worktrees/${WORKTREE}.json`);
    expect(layout.quarantinePath).toBe(`/control/repositories/${REPOSITORY}/instances/.deleting-${WORKTREE}`);
  });
});

describe('derived names', () => {
  it('produces a stable, short, prefixed Compose project name', () => {
    const name = composeProjectName(REPOSITORY, WORKTREE);
    expect(name).toBe(composeProjectName(REPOSITORY, WORKTREE));
    expect(name).toMatch(new RegExp(`^${BRAND.name}-[0-9a-f]{24}$`));
    expect(composeProjectName(REPOSITORY, 'other')).not.toBe(name);
  });

  it('routes a worktree under the project domain', () => {
    expect(worktreeHost('feature-login', 'acme.localhost')).toBe('feature-login.acme.localhost');
  });
});
