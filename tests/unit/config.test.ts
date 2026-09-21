import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { BRAND } from '../../src/brand.ts';
import { findConfigFile, loadNearestConfig, parseConfig } from '../../src/config/load.ts';
import { CliError } from '../../src/errors.ts';

const temporary: string[] = [];

afterEach(async () => {
  await Promise.all(
    temporary.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))
  );
});

const MINIMAL = `
[project]
name = "acme"
domain = "acme.localhost"

[compose]
file = "docker/compose.yaml"
`;

describe('config parsing', () => {
  it('accepts a minimal file and fills defaults', () => {
    const config = parseConfig(MINIMAL, 'test.toml');
    expect(config.project.name).toBe('acme');
    expect(config.service).toEqual([]);
    expect(config.hooks).toEqual({});
  });

  it('defaults a service to the host root and keeps index metadata', () => {
    const config = parseConfig(`${MINIMAL}
[[service]]
name = "api"
[service.index]
label = "API"
`, 'test.toml');
    expect(config.service[0]).toEqual({
      name: 'api',
      subdomain: '',
      index: { label: 'API', purpose: '' }
    });
  });

  it('rejects two services claiming the same subdomain', () => {
    const contents = `${MINIMAL}
[[service]]
name = "api"
[[service]]
name = "web"
`;
    expect(() => parseConfig(contents, 'test.toml')).toThrow('the host root is already claimed');
  });

  it('lets two services share a host when their paths differ, and rejects the same path twice', () => {
    const shared = `${MINIMAL}
[[service]]
name = "web"
[[service]]
name = "api"
path = "/api"
port = 3000
`;
    const config = parseConfig(shared, 'test.toml');
    expect(config.service.map((s) => [s.name, s.path ?? '', s.port ?? null])).toEqual([['web', '', null], ['api', '/api', 3000]]);

    expect(() => parseConfig(`${shared}[[service]]\nname = "api2"\npath = "/api"\n`, 'test.toml'))
      .toThrow('the host root at path "/api" is already claimed');
    expect(() => parseConfig(`${MINIMAL}[[service]]\nname = "a"\nport = 70000\n`, 'test.toml')).toThrow(/port/);
  });

  it('keeps what reaches Traefik rules and hostnames well-formed', () => {
    const service = (body: string) => `${MINIMAL}[[service]]\nname = "s"\n${body}`;
    expect(() => parseConfig(service('path = "/api`)||Host(`evil"\n'), 'test.toml')).toThrow('must be a URL path starting with /');
    expect(() => parseConfig(service('path = "/a%zz"\n'), 'test.toml')).toThrow('% must be followed by two hex digits');
    expect(() => parseConfig(service('path = "/search?q=1"\n'), 'test.toml')).toThrow('must be a URL path starting with /');
    expect(() => parseConfig(service('path = "/api$HOME"\n'), 'test.toml')).toThrow('backticks, $, ? or #');   // Compose would interpolate it
    expect(parseConfig(service('path = "/v1/things:list/@scope/c++%2Fx"\n'), 'test.toml').service[0]?.path).toBe('/v1/things:list/@scope/c++%2Fx');
    expect(() => parseConfig(service('subdomain = "-mail"\n'), 'test.toml')).toThrow('not starting or ending with a hyphen');
    expect(() => parseConfig(service('subdomain = "mail-"\n'), 'test.toml')).toThrow('not starting or ending with a hyphen');
    expect(() => parseConfig(service(`subdomain = "${'a'.repeat(64)}"\n`), 'test.toml')).toThrow('at most 63');
    expect(() => parseConfig(MINIMAL.replace('acme.localhost', `${'a'.repeat(64)}.localhost`), 'test.toml')).toThrow('each label must be at most 63');
    expect(() => parseConfig(MINIMAL.replace('acme.localhost', `${'ab.'.repeat(40)}localhost`), 'test.toml')).toThrow('at most 125 characters, so that <subdomain>.<slug>.<domain> fits');
  });

  it('rejects duplicate service names', () => {
    const contents = `${MINIMAL}
[[service]]
name = "api"
subdomain = "a"
[[service]]
name = "api"
subdomain = "b"
`;
    expect(() => parseConfig(contents, 'test.toml')).toThrow('duplicate service name "api"');
  });

  it('rejects unknown keys so typos do not silently disable features', () => {
    expect(() => parseConfig(`${MINIMAL}\n[hooks]\nprepair = "./x"\n`, 'test.toml'))
      .toThrow(/hooks/);
  });

  it('reports invalid TOML distinctly from invalid configuration', () => {
    expect(() => parseConfig('[project\nname = ', 'test.toml')).toThrow('is not valid TOML');
  });

  it('refuses a control home in the committed config, since that path is per-machine', () => {
    expect(() => parseConfig(MINIMAL.replace('domain', 'control-home = "~/acme"\ndomain'), 'test.toml'))
      .toThrow(/control-home/);
  });

  it('validates the label namespace shape', () => {
    expect(() => parseConfig(MINIMAL.replace('domain', 'label-namespace = "NotADomain"\ndomain'), 'test.toml'))
      .toThrow('reverse domain');
  });
});

describe('config discovery', () => {
  it('walks upward from a nested directory to the nearest config file', async () => {
    const root = await mkdtemp(path.join(tmpdir(), 'berth-config-'));
    temporary.push(root);
    const nested = path.join(root, 'apps', 'api', 'src');
    await mkdir(nested, { recursive: true });
    const file = path.join(root, BRAND.configFile);
    await writeFile(file, MINIMAL);

    expect(await findConfigFile(nested)).toBe(file);
    const loaded = await loadNearestConfig(nested);
    expect(loaded.file).toBe(file);
    expect(loaded.root).toBe(root);
  });

  it('fails with a hint when no config file exists', async () => {
    const root = await mkdtemp(path.join(tmpdir(), 'berth-noconfig-'));
    temporary.push(root);

    await expect(loadNearestConfig(root)).rejects.toBeInstanceOf(CliError);
    await expect(loadNearestConfig(root)).rejects.toThrow(`No ${BRAND.configFile} found`);
  });
});
