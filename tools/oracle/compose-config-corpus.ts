// Oracle for internal/backend's decode of `docker compose config --format
// json`, the port of ComposeRuntime's private readModel and the
// composeConfigSchema it validates against. Regenerate with:
//
//   export PATH=$HOME/.local/opt/node-v24.14.1-linux-x64/bin:$PATH
//   node tools/oracle/compose-config-corpus.ts > internal/backend/testdata/compose-config-corpus.json
//
// readModel is private; reached here the same way overrideDocument is in
// override-corpus.ts (Node's type stripping does not enforce TypeScript's
// compile-time privacy), fed through a runner double that answers `docker
// compose config` with each case's document, so the real zod schema
// (composeConfigSchema) runs unmodified. Every case here is a malformed
// shape the schema's own nested checks catch — a build that is not an
// object, an expose entry that is neither string nor number, a ports target
// that is not numeric, or an external value zod's union has no room for
// (notably null) — which decodeOrderedComposeModel must refuse exactly the
// same way, before overrideDocument ever sees the data.
import { ComposeRuntime, type ComposeRuntimeOptions } from '../../src/runtime/compose.ts';
import { silentLogger } from '../../src/logger.ts';
import type { CommandRunner, RunResult } from '../../src/process-runner.ts';

const REF = { repositoryId: 'a08b4892-5111-49f1-a47a-5ab988c7f676', worktreeId: '2b41d9d4-ed88-49c1-be9c-5439bdf6a431', slug: 'feature-login' };

function runnerReturning(stdout: string): CommandRunner {
  return {
    run(): Promise<RunResult> { return Promise.resolve({ stdout, stderr: '', exitCode: 0 }); },
    which(): Promise<boolean> { return Promise.resolve(true); }
  };
}

interface Case {
  name: string;
  config: object;
}

const cases: Case[] = [
  {
    name: 'a well-formed document is accepted, as a control case',
    config: {
      services: { api: { image: 'busybox', build: { context: '.' }, expose: ['80'], ports: [{ target: 80 }] } },
      volumes: { data: { external: true } },
      networks: { backend: { external: { name: 'x' } } }
    }
  },
  {
    name: 'a service build that is not an object is refused',
    config: { services: { api: { build: './builder' } }, volumes: {}, networks: {} }
  },
  {
    name: 'a service expose entry that is neither string nor number is refused',
    config: { services: { api: { expose: [{ weird: true }] } }, volumes: {}, networks: {} }
  },
  {
    name: 'a service ports entry with a non-numeric target is refused',
    config: { services: { api: { ports: [{ target: '8080' }] } }, volumes: {}, networks: {} }
  },
  {
    name: 'a service ports field that is not an array is refused',
    config: { services: { api: { ports: {} } }, volumes: {}, networks: {} }
  },
  {
    name: 'a service expose field that is not an array is refused',
    config: { services: { api: { expose: '8080' } }, volumes: {}, networks: {} }
  },
  {
    name: 'a volume external of null is refused, unlike false',
    config: { services: {}, volumes: { data: { external: null } }, networks: {} }
  },
  {
    name: 'a network external that is a string is refused',
    config: { services: {}, volumes: {}, networks: { backend: { external: 'yes' } } }
  },
  {
    name: 'a network external that is a number is refused',
    config: { services: {}, volumes: {}, networks: { backend: { external: 1 } } }
  },
  {
    name: 'a service entry that is not an object is refused',
    config: { services: { api: 'busybox' }, volumes: {}, networks: {} }
  },
  {
    name: 'a top-level services section that is not an object is refused',
    config: { services: [], volumes: {}, networks: {} }
  }
];

const DEFAULT_OPTIONS: Omit<ComposeRuntimeOptions, 'runner'> = {
  composeFile: '/work/acme/docker/compose.yaml',
  projectRoot: '/work/acme',
  configFile: '/work/acme/berth.toml',
  projectName: 'acme',
  domain: 'acme.localhost',
  controlHome: '/control',
  labelNamespace: 'dev.berth',
  routes: [],
  routerNetwork: 'berth-router',
  logger: silentLogger
};

async function run(): Promise<void> {
  const output = [];
  for (const c of cases) {
    const runtime = new ComposeRuntime({ ...DEFAULT_OPTIONS, runner: runnerReturning(JSON.stringify(c.config)) });
    try {
      // readModel is private; reached here as any, deliberately — see the
      // header comment.
      await (runtime as unknown as { readModel(ref: typeof REF): Promise<unknown> }).readModel(REF);
      output.push({ name: c.name, config: c.config, ok: true });
    } catch (error) {
      output.push({ name: c.name, config: c.config, error: error instanceof Error ? error.message : String(error) });
    }
  }
  process.stdout.write(JSON.stringify(output, null, 2) + '\n');
}

await run();
