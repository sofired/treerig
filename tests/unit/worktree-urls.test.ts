import { describe, expect, it } from 'vitest';
import { worktreeUrls } from '../../src/commands/worktree.ts';
import { parseConfig } from '../../src/config/load.ts';

const config = parseConfig(`
[project]
name = "acme"
domain = "acme.localhost"
[compose]
file = "compose.yaml"
[[service]]
name = "api"
[service.index]
label = "API"
purpose = "REST surface"
[[service]]
name = "mail"
subdomain = "mail"
[[service]]
name = "docs"
subdomain = "docs"
path = "/latest"
`, 'berth.toml');

describe('worktreeUrls', () => {
  it('routes the empty subdomain at the host root and prefixes the others, in config order', () => {
    expect(worktreeUrls(config, { slug: 'feature-login', domain: 'acme.localhost' })).toEqual([
      { service: 'api', url: 'http://feature-login.acme.localhost', label: 'API', purpose: 'REST surface' },
      { service: 'mail', url: 'http://mail.feature-login.acme.localhost' },
      { service: 'docs', url: 'http://docs.feature-login.acme.localhost/latest' }
    ]);
  });

  it('uses the registered domain, not the config domain, so a stale record routes where it is actually registered', () => {
    const [api] = worktreeUrls(config, { slug: 'main', domain: 'old.localhost' });
    expect(api?.url).toBe('http://main.old.localhost');
  });

  it('appends the router port when it is not 80', () => {
    expect(worktreeUrls(config, { slug: 'main', domain: 'acme.localhost' }, 8080).map((u) => u.url)).toEqual([
      'http://main.acme.localhost:8080',
      'http://mail.main.acme.localhost:8080',
      'http://docs.main.acme.localhost:8080/latest'
    ]);
  });

  it('returns nothing for a project with no routed services', () => {
    const bare = parseConfig('[project]\nname = "x"\ndomain = "x.localhost"\n[compose]\nfile = "c.yaml"\n', 'berth.toml');
    expect(worktreeUrls(bare, { slug: 'main', domain: 'x.localhost' })).toEqual([]);
  });
});
