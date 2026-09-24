// Brings a worktree up the way the TypeScript build does — registration,
// home, index card, and its Compose project with every ownership label — but
// without the shared router, so a test never touches a developer's real
// router. The Go build then has to remove what this left behind.
//
//   node tools/oracle/legacy-up.ts <controlHome> <checkout>
//
// Prints {"repositoryId","worktreeId","slug","domain","labelNamespace"} on stdout.
import path from 'node:path';
import { writeIndexCard } from '../../src/checkout-index/card.ts';
import { loadConfig } from '../../src/config/load.ts';
import { secureWorktreeDirectories } from '../../src/files.ts';
import { worktreeHost, worktreeLayout } from '../../src/layout.ts';
import { silentLogger } from '../../src/logger.ts';
import { ExecaRunner } from '../../src/process-runner.ts';
import { ComposeRuntime } from '../../src/runtime/compose.ts';
import { WorktreeRegistry } from '../../src/worktree-registry.ts';

const [controlHome, root] = process.argv.slice(2);
if (controlHome === undefined || root === undefined) throw new Error('usage: legacy-up.ts <controlHome> <checkout>');

const loaded = await loadConfig(path.join(root, 'berth.toml'));
const { config } = loaded;
const labelNamespace = config.project['label-namespace'] ?? 'dev.berth';
const runner = new ExecaRunner();
const registry = new WorktreeRegistry({
  controlHome, root, domain: config.project.domain, labelNamespace, runner, logger: silentLogger
});
const registered = await registry.ensure();
const { repositoryId, worktreeId, slug, domain } = registered;
await secureWorktreeDirectories(worktreeLayout(controlHome, repositoryId, worktreeId));
await writeIndexCard(controlHome, repositoryId, worktreeId, {
  version: 1,
  project: config.project.name,
  domain,
  slug,
  host: worktreeHost(slug, domain),
  root: registered.root,
  branch: registered.branch,
  labelNamespace,
  services: [],
  updatedAt: new Date().toISOString()
});
const runtime = new ComposeRuntime({
  composeFile: path.resolve(loaded.root, config.compose.file),
  projectRoot: loaded.root,
  configFile: loaded.file,
  projectName: config.project.name,
  domain,
  controlHome,
  labelNamespace,
  routes: [],
  routerNetwork: 'unused',
  runner,
  logger: silentLogger
});
await runtime.up({ repositoryId, worktreeId, slug }, []);
await registry.store(repositoryId).completeInitialization(worktreeId, slug, silentLogger);
process.stdout.write(`${JSON.stringify({ repositoryId, worktreeId, slug, domain, labelNamespace })}\n`);
