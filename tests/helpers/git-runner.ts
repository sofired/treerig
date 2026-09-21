import path from 'node:path';
import type { CommandRunner, RunResult } from '../../src/process-runner.ts';
import type { WorktreeIdentity } from '../../src/worktree-registry.ts';

/** Answers the handful of git queries the registry makes, from an in-memory model. */
export class GitRunner implements CommandRunner {
  readonly identities = new Map<string, Omit<WorktreeIdentity, 'repositoryId' | 'worktreeId'>>();
  active: string[] = [];

  add(root: string, branch: string, common: string, gitName = path.basename(root)): void {
    this.identities.set(root, {
      root,
      branch,
      gitCommonDir: common,
      gitDir: path.join(common, 'worktrees', gitName)
    });
    this.active.push(root);
  }

  setBranch(root: string, branch: string): void {
    const current = this.identities.get(root);
    if (current) this.identities.set(root, { ...current, branch });
  }

  run(command: string, args: readonly string[] = []): Promise<RunResult> {
    if (command !== 'git') return Promise.resolve({ stdout: '', stderr: '', exitCode: 0 });
    if (args.includes('worktree') && args.includes('list')) {
      const stdout = this.active.map((root) => {
        const item = this.identities.get(root);
        return `worktree ${root}\0HEAD deadbeef\0branch refs/heads/${item?.branch ?? ''}\0\0`;
      }).join('');
      return Promise.resolve({ stdout, stderr: '', exitCode: 0 });
    }
    const root = args[args.indexOf('-C') + 1] ?? '';
    const item = this.identities.get(root);
    if (!item) return Promise.reject(new Error(`Unknown Git worktree: ${root}`));
    let stdout = `${item.branch}\n`;
    if (args.includes('--show-toplevel')) stdout = `${item.root}\n`;
    else if (args.includes('--git-common-dir')) stdout = `${item.gitCommonDir}\n`;
    else if (args.includes('--git-dir')) stdout = `${item.gitDir}\n`;
    return Promise.resolve({ stdout, stderr: '', exitCode: 0 });
  }

  which(): Promise<boolean> {
    return Promise.resolve(true);
  }
}
