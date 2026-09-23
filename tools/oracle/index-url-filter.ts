// Oracle for buildSnapshot's card-service URL filter (serviceBelongsToHost
// in internal/checkoutindex/snapshot.go): runs the real buildSnapshot over
// cards whose service URLs exercise every WHATWG URL.parse normalization the
// TypeScript filter depends on — scheme and hostname case-folding, default-
// port dropping, a port's leading zeros, a trailing dot on the hostname, and
// an IPv6 literal — so Go's own URL handling can be checked against the same
// cases instead of against a description of WHATWG's behaviour. Regenerate
// with:
//
//   export PATH=$HOME/.local/opt/node-v24.14.1-linux-x64/bin:$PATH
//   node tools/oracle/index-url-filter.ts > internal/checkoutindex/testdata/url-filter.json
import { buildSnapshot, entryKey } from '../../src/checkout-index/snapshot.ts';
import type { RegisteredWorktree } from '../../src/worktree-registry.ts';
import type { IndexCard } from '../../src/checkout-index/card.ts';

const REPO = '11111111-1111-4111-8111-111111111111';
const WT = '22222222-2222-4222-8222-222222222221';

const record: RegisteredWorktree = { repositoryId: REPO, worktreeId: WT, slug: 'feature-a', domain: 'acme.localhost' };
const host = `${record.slug}.${record.domain}`;

function card(url: string): IndexCard {
  return {
    version: 1, project: 'acme', domain: record.domain, slug: record.slug, host,
    root: '/work/feature-a', branch: 'feature/a', labelNamespace: 'dev.berth',
    services: [{ name: 'svc', url, label: 'Service', purpose: '' }],
    updatedAt: '2026-01-01T00:00:00.000Z'
  };
}

interface Case {
  name: string;
  url: string;
  routerPort: number;
}

const cases: Case[] = [
  { name: 'exact host, matching port', url: `http://${host}:8080/`, routerPort: 8080 },
  { name: 'uppercase scheme', url: `HTTP://${host}:8080/`, routerPort: 8080 },
  { name: 'uppercase host', url: `http://${host.toUpperCase()}:8080/`, routerPort: 8080 },
  { name: 'mixed-case scheme and host, with a path', url: `HtTp://${host.toUpperCase()}:8080/console`, routerPort: 8080 },
  { name: 'default port dropped by WHATWG, matches routerPort 80', url: `http://${host}:80/`, routerPort: 80 },
  { name: 'port omitted, matches routerPort 80', url: `http://${host}/`, routerPort: 80 },
  { name: 'default port dropped by WHATWG, does not match routerPort 8080', url: `http://${host}:80/`, routerPort: 8080 },
  { name: 'explicit non-default port matches routerPort', url: `http://${host}:8080/`, routerPort: 8080 },
  { name: 'explicit non-default port does not match routerPort', url: `http://${host}:9999/`, routerPort: 8080 },
  { name: 'port with a leading zero normalizes the same as without one', url: `http://${host}:065/`, routerPort: 65 },
  { name: 'subdomain of host', url: `http://mail.${host}:8080/`, routerPort: 8080 },
  { name: 'uppercase subdomain of host', url: `http://MAIL.${host}:8080/`, routerPort: 8080 },
  { name: 'unrelated host', url: 'http://evil.localhost:8080/', routerPort: 8080 },
  { name: 'lookalike host is not a subdomain', url: `http://evil${host}:8080/`, routerPort: 8080 },
  { name: 'https scheme is rejected regardless of port', url: `https://${host}:8080/`, routerPort: 8080 },
  { name: 'https default port dropped by WHATWG, still rejected: not http', url: `https://${host}/`, routerPort: 443 },
  { name: 'non-http(s) scheme is rejected', url: `ftp://${host}:8080/`, routerPort: 8080 },
  { name: 'trailing dot on the hostname does not match', url: `http://${host}.:8080/`, routerPort: 8080 },
  { name: 'an IPv6 literal host never matches a DNS-shaped host', url: 'http://[::1]:8080/', routerPort: 8080 }
];

const results = cases.map((c) => {
  const snapshot = buildSnapshot({
    registrations: [record],
    cards: new Map([[entryKey(record), card(c.url)]]),
    identities: [],
    routerPort: c.routerPort,
    generatedAt: '2026-01-01T00:00:00.000Z'
  });
  const entry = snapshot.entries.find((e) => e.slug === record.slug);
  const kept = (entry?.services.length ?? 0) > 0;
  return { name: c.name, url: c.url, routerPort: c.routerPort, kept, keptUrl: kept ? (entry?.services[0]?.url ?? null) : null };
});

process.stdout.write(JSON.stringify({ host, cases: results }, null, 2) + '\n');
