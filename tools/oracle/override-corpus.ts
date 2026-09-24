// Oracle for internal/backend's Compose override-document port. Regenerate with:
//
//   export PATH=$HOME/.local/opt/node-v24.14.1-linux-x64/bin:$PATH
//   node tools/oracle/override-corpus.ts > internal/backend/testdata/override-corpus.json
//
// ComposeRuntime.overrideDocument is private; this script reaches it the way
// Node's own type-stripping already lets any caller reach a "private" method
// at runtime, since TypeScript's privacy is a compile-time check only. Each
// case builds a ComposeRuntime over its own routes/domain/labelNamespace
// (overrideDocument reads those off `this.options`, not its arguments), and
// hands it a `config` shaped exactly like an already-decoded
// `docker compose config --format json` document — the runtime never re-reads
// the file for this, so no real Compose file or daemon is needed here.
//
// The corpus exists because the override document must be byte-identical to
// what the TypeScript build writes: this records overrideDocument's output as
// `up` itself would serialize it (JSON.stringify(document, null, 2) + '\n'),
// including hostile route names, so the Go port's own ordered JSON encoder
// (internal/backend/orderedjson.go) can be checked against it directly.
import { ComposeRuntime, type ComposeRuntimeOptions, type RouteSpec } from '../../src/runtime/compose.ts';
import { silentLogger } from '../../src/logger.ts';
import type { CommandRunner, RunResult } from '../../src/process-runner.ts';

const REF = { repositoryId: 'a08b4892-5111-49f1-a47a-5ab988c7f676', worktreeId: '2b41d9d4-ed88-49c1-be9c-5439bdf6a431', slug: 'feature-login' };

/** Never actually called: overrideDocument is pure and touches neither. */
const unusedRunner: CommandRunner = {
  run(): Promise<RunResult> { throw new Error('not used by overrideDocument'); },
  which(): Promise<boolean> { throw new Error('not used by overrideDocument'); }
};

interface Case {
  name: string;
  ref?: typeof REF;
  domain?: string;
  labelNamespace?: string;
  routerNetwork?: string;
  routes?: RouteSpec[];
  config: object;
}

const DEFAULT_OPTIONS: Omit<ComposeRuntimeOptions, 'domain' | 'labelNamespace' | 'routerNetwork' | 'routes'> = {
  composeFile: '/work/acme/docker/compose.yaml',
  projectRoot: '/work/acme',
  configFile: '/work/acme/berth.toml',
  projectName: 'acme',
  controlHome: '/control',
  runner: unusedRunner,
  logger: silentLogger
};

const cases: Case[] = [
  {
    name: 'plain service, no routes, no volumes or networks declared',
    config: { services: { api: { image: 'busybox' } }, volumes: {}, networks: {} }
  },
  {
    name: 'build labels reach only the services that build',
    config: {
      services: {
        api: { image: 'busybox' },
        builder: { build: { context: '/work/acme/docker/builder' } }
      },
      volumes: {}, networks: {}
    }
  },
  {
    name: 'default network is injected once, when a service uses it and it is not already declared',
    config: { services: { api: { image: 'busybox' }, worker: { image: 'busybox' } }, volumes: {}, networks: {} }
  },
  {
    name: 'default network is not injected again when already declared',
    config: { services: { api: { image: 'busybox' } }, volumes: {}, networks: { default: {} } }
  },
  {
    name: 'default network is not injected when no service uses it',
    config: { services: { api: { image: 'busybox', networks: ['backend'] } }, volumes: {}, networks: { backend: {} } }
  },
  {
    name: 'an externally declared volume (boolean form) is excluded',
    config: { services: {}, volumes: { data: { external: true }, cache: {} }, networks: {} }
  },
  {
    name: 'an externally declared volume (named object form) is excluded',
    config: { services: {}, volumes: { data: { external: { name: 'shared-external' } } }, networks: {} }
  },
  {
    name: 'external: false is not external',
    config: { services: {}, volumes: { data: { external: false } }, networks: {} }
  },
  {
    name: 'external: null is external, matching `!== false` exactly',
    config: { services: {}, volumes: { data: { external: null } }, networks: {} }
  },
  {
    name: 'a route with a subdomain, an explicit path, and an explicit port',
    routes: [{ service: 'api', subdomain: 'admin', path: '/v2', port: 9091 }],
    routerNetwork: 'berth-router',
    config: { services: { api: { image: 'busybox' } }, volumes: {}, networks: {} }
  },
  {
    name: 'a route with no subdomain routes at the bare worktree host',
    routes: [{ service: 'api', subdomain: '' }],
    routerNetwork: 'berth-router',
    config: { services: { api: { image: 'busybox', expose: ['8080/tcp'] } }, volumes: {}, networks: {} }
  },
  {
    name: 'a route falls back to the first exposed port when none is given',
    routes: [{ service: 'api', subdomain: '' }],
    routerNetwork: 'berth-router',
    config: { services: { api: { image: 'busybox', expose: ['8080/tcp', '9090/tcp'] } }, volumes: {}, networks: {} }
  },
  {
    name: 'a route falls back to the first ports[] entry that declares a target',
    routes: [{ service: 'api', subdomain: '' }],
    routerNetwork: 'berth-router',
    config: {
      services: { api: { image: 'busybox', ports: [{ mode: 'ingress', protocol: 'tcp' }, { target: 9090 }] } },
      volumes: {}, networks: {}
    }
  },
  {
    name: 'routing an undefined service is refused',
    routes: [{ service: 'ghost', subdomain: '' }],
    routerNetwork: 'berth-router',
    config: { services: { api: { image: 'busybox' } }, volumes: {}, networks: {} }
  },
  {
    name: 'routing a service with no derivable port is refused',
    routes: [{ service: 'quiet', subdomain: '' }],
    routerNetwork: 'berth-router',
    config: { services: { quiet: { image: 'busybox' } }, volumes: {}, networks: {} }
  },
  {
    name: 'a router network that collides with an already-declared network is overwritten in place, not duplicated',
    routes: [{ service: 'api', subdomain: '' }],
    routerNetwork: 'backend',
    config: {
      services: { api: { image: 'busybox', expose: ['80'], networks: ['backend', 'frontend'] } },
      volumes: {}, networks: { backend: {}, frontend: {} }
    }
  },
  {
    name: 'a service\'s array-form networks are preserved in order alongside the router network',
    routes: [{ service: 'api', subdomain: '' }],
    routerNetwork: 'berth-router',
    config: {
      services: { api: { image: 'busybox', expose: ['80'], networks: ['zeta', 'alpha', 'middle'] } },
      volumes: {}, networks: { zeta: {}, alpha: {}, middle: {} }
    }
  },
  {
    name: 'a service\'s object-form networks are preserved in key order alongside the router network',
    routes: [{ service: 'api', subdomain: '' }],
    routerNetwork: 'berth-router',
    config: {
      services: { api: { image: 'busybox', expose: ['80'], networks: { zeta: {}, alpha: {}, middle: {} } } },
      volumes: {}, networks: { zeta: {}, alpha: {}, middle: {} }
    }
  },
  {
    name: 'hostile route names: quotes, backslashes, angle brackets, ampersands, and non-ASCII are not HTML-escaped',
    ref: { ...REF, slug: `weird"'\\<>&slug` },
    domain: 'acme.localhost',
    routes: [{ service: 'api', subdomain: `sub<script>&"'\\`, path: `/p"'<>&\\ün` }],
    routerNetwork: 'berth-router',
    config: { services: { api: { image: 'busybox', expose: ['80'] } }, volumes: {}, networks: {} }
  },
  {
    name: 'a custom label namespace changes only the ownership and ROUTED_BY_LABEL values, never the label keys',
    labelNamespace: 'dev.custom-namespace',
    routes: [{ service: 'api', subdomain: '' }],
    routerNetwork: 'berth-router',
    config: { services: { api: { image: 'busybox', expose: ['80'] } }, volumes: {}, networks: {} }
  },
  {
    name: 'no routes at all: the router network branch is skipped entirely',
    config: {
      services: { api: { image: 'busybox' } },
      volumes: { data: {} },
      networks: { backend: {} }
    }
  },
  {
    // Built with fromCharCode, deliberately, rather than a \u escape typed
    // literally in this file: that literal text is exactly the shape a
    // JSON-based tool transport can silently decode to the real character
    // on the way to disk, which would defeat the point of this case.
    name: 'a service and route name carrying the JavaScript line separator (U+2028) is not escaped',
    ref: { ...REF, slug: 'feature' + String.fromCharCode(0x2028) + 'login' },
    routes: [{ service: 'api' + String.fromCharCode(0x2028) + 'svc', subdomain: 'sub' + String.fromCharCode(0x2029) + 'dom' }],
    routerNetwork: 'berth-router',
    config: {
      services: { ['api' + String.fromCharCode(0x2028) + 'svc']: { image: 'busybox', expose: ['80'] } },
      volumes: {}, networks: {}
    }
  }
];

// The Go test decodes `config` (and this whole file) with its own
// order-preserving JSON decoder and drives its own overrideDocument with the
// same ref/domain/labelNamespace/routerNetwork/routes recorded here — so
// each case's input lives in exactly one place, this file, rather than
// being duplicated (and risking drift) in Go source.
const results = cases.map((c) => {
  const domain = c.domain ?? 'acme.localhost';
  const labelNamespace = c.labelNamespace ?? 'dev.berth';
  const routerNetwork = c.routerNetwork ?? 'berth-router';
  const routes = c.routes ?? [];
  const ref = c.ref ?? REF;
  const options: ComposeRuntimeOptions = { ...DEFAULT_OPTIONS, domain, labelNamespace, routerNetwork, routes };
  const runtime = new ComposeRuntime(options);
  const outcome = (() => {
    try {
      // overrideDocument is private; reached here as any, deliberately — see
      // the header comment.
      const document = (runtime as unknown as { overrideDocument(ref: typeof REF, config: object): object })
        .overrideDocument(ref, c.config);
      return { bytes: `${JSON.stringify(document, null, 2)}\n` };
    } catch (error) {
      return { error: error instanceof Error ? error.message : String(error) };
    }
  })();
  return { name: c.name, ref, domain, labelNamespace, routerNetwork, routes, config: c.config, ...outcome };
});

process.stdout.write(JSON.stringify(results, null, 2) + '\n');
