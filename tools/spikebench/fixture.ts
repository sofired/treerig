// Registers the benchmark's worktrees the way the TypeScript build does, so
// both builds are measured against identical, genuinely registered state.
//
//   node tools/spikebench/fixture.ts <controlHome> <extraRecords> <worktree>...
//
// Each <worktree> is a git checkout with berth.toml at its root; it is
// registered with WorktreeRegistry.ensure (markers, slug, record, under the
// real locks). <extraRecords> more records are written for two other
// repositories, so `worktree list --all` has a machine's worth to read.
import { randomUUID } from 'node:crypto';
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { loadConfig } from '../../src/config/load.ts';
import { silentLogger } from '../../src/logger.ts';
import { ExecaRunner } from '../../src/process-runner.ts';
import { WorktreeRegistry } from '../../src/worktree-registry.ts';

const [controlHome, extraText, ...worktrees] = process.argv.slice(2);
if (controlHome === undefined || extraText === undefined || worktrees.length === 0) {
  throw new Error('usage: fixture.ts <controlHome> <extraRecords> <worktree>...');
}

for (const root of worktrees) {
  const { config } = await loadConfig(path.join(root, 'berth.toml'));
  const registry = new WorktreeRegistry({
    controlHome,
    root,
    domain: config.project.domain,
    labelNamespace: config.project['label-namespace'] ?? 'dev.berth',
    runner: new ExecaRunner(),
    logger: silentLogger
  });
  await registry.ensure();
}

const extra = Number(extraText);
for (const [index, domain] of ['beta.localhost', 'gamma.localhost'].entries()) {
  const repositoryId = randomUUID();
  const directory = path.join(controlHome, 'repositories', repositoryId, 'worktrees');
  await mkdir(directory, { recursive: true, mode: 0o700 });
  const count = index === 0 ? Math.ceil(extra / 2) : Math.floor(extra / 2);
  for (let n = 0; n < count; n++) {
    const worktreeId = randomUUID();
    const record = {
      version: 1,
      repositoryId,
      worktreeId,
      slug: `feature-${index}-${n}`,
      domain,
      labelNamespace: 'dev.berth',
      initialization: n % 3 === 0 ? 'pending' : 'complete'
    };
    await writeFile(path.join(directory, `${worktreeId}.json`), `${JSON.stringify(record, null, 2)}\n`, { mode: 0o600 });
  }
}
