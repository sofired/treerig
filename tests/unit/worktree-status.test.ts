import { readdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { lifecycleStages, status, up, urls } from '../../src/commands/worktree.ts';
import { registryDirectory } from '../../src/worktree-registry.ts';
import { MemoryRuntime } from '../../src/runtime/memory.ts';
import { commandWorld, type CommandWorld } from '../helpers/command-world.ts';

// What `status` puts on stdout, and what it refuses to put there: the service
// names come from the branch's own compose file, which no schema constrains.

let world: CommandWorld;
afterEach(async () => {
  await world?.dispose();
});

/** Rewrites the one record's domain; the record schema allows any non-empty string. */
async function retag(controlHome: string, repositoryId: string, domain: string): Promise<void> {
  const directory = registryDirectory(controlHome, repositoryId);
  const [file] = (await readdir(directory)).filter((name) => name.endsWith('.json'));
  const target = path.join(directory, file ?? '');
  const record = JSON.parse(await readFile(target, 'utf8')) as { domain: string };
  await writeFile(target, `${JSON.stringify({ ...record, domain })}\n`);
}

describe('which hook stages up asks for', () => {
  it('seeds only the first time, and never asks for a stage nothing invokes', () => {
    expect(lifecycleStages(true)).toEqual(['prepare', 'seed']);
    expect(lifecycleStages(false)).toEqual(['prepare']);
  });
});

describe('worktree status', () => {
  it('will not let a compose file drive the terminal through a service row', async () => {
    world = await commandWorld();
    await up(world.context());
    const context = world.context();
    // A compose file naming a service this way is unusual, not impossible, and
    // nothing between it and the terminal validates it.
    const hostile = { ...context, runtime: new MemoryRuntime({ services: ['api\u001b[2K\u202ex'] }) };

    await status(hostile);

    // The exact row: filtered, still legible, still padded into its column.
    expect(context.logger.printed).toContain('  api\uFFFD[2K\uFFFDx  absent');
  });

  it('cannot drive the terminal through a domain no schema constrains', async () => {
    // A registry record's domain is deliberately looser than the config's
    // hostname rule, because removal has to find a record it cannot parse.
    world = await commandWorld();
    const { registration } = await up(world.context());
    await retag(world.controlHome, registration.repositoryId, 'acme\u001b[2K\u202ex.localhost');

    const context = world.context();
    await status(context);
    await urls(context);

    expect(context.logger.printed[0]).toBe(`${registration.slug}.acme\uFFFD[2K\uFFFDx.localhost`);
    expect(context.logger.printed).toContain(`api   http://${registration.slug}.acme\uFFFD[2K\uFFFDx.localhost`);
  });
});
