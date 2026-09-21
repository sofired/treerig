import { release } from 'node:os';
import { BRAND } from './brand.ts';
import { CliError } from './errors.ts';
import type { CommandRunner } from './process-runner.ts';

interface BrowserHost {
  environment: NodeJS.ProcessEnv;
  kernelRelease: string;
  platform: NodeJS.Platform;
}

type BrowserHostOverrides = Partial<BrowserHost>;

function isWsl(host: BrowserHost): boolean {
  return host.platform === 'linux' && Boolean(
    host.environment.WSL_DISTRO_NAME?.trim() ||
    host.environment.WSL_INTEROP?.trim() ||
    /microsoft/i.test(host.kernelRelease)
  );
}

function browserCandidates(host: BrowserHost, commandName: string): readonly string[] {
  if (host.platform === 'darwin') return ['open'];
  if (isWsl(host)) return ['wslview', 'explorer.exe'];
  if (host.platform === 'linux') return ['xdg-open'];
  throw new CliError(
    `Opening a browser is not supported on ${host.platform}. Run '${commandName} urls' instead.`
  );
}

function resolveHost(overrides: BrowserHostOverrides): BrowserHost {
  return {
    environment: overrides.environment ?? process.env,
    kernelRelease: overrides.kernelRelease ?? release(),
    platform: overrides.platform ?? process.platform
  };
}

/**
 * Picks the platform's URL opener, including the WSL cases (`wslview`, then
 * `explorer.exe`) that generic "open a browser" helpers usually miss.
 * `commandName` appears in the hint users see when no opener exists.
 */
export async function selectBrowserOpener(
  runner: Pick<CommandRunner, 'which'>,
  hostOverrides: BrowserHostOverrides = {},
  commandName: string = BRAND.name
): Promise<string> {
  const host = resolveHost(hostOverrides);
  const candidates = browserCandidates(host, commandName);
  for (const candidate of candidates) {
    if (await runner.which(candidate)) return candidate;
  }

  const hostName = isWsl(host) ? 'WSL' : host.platform;
  throw new CliError(
    `No browser opener is available for ${hostName} (tried ${candidates.join(', ')}). ` +
    `Run '${commandName} urls' and open the desired URL manually.`
  );
}

export async function openBrowser(
  runner: CommandRunner,
  url: string,
  hostOverrides: BrowserHostOverrides = {},
  commandName: string = BRAND.name
): Promise<void> {
  const opener = await selectBrowserOpener(runner, hostOverrides, commandName);
  await runner.run(opener, [url]);
}
