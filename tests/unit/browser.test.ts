import { describe, expect, it } from 'vitest';
import { openBrowser, selectBrowserOpener } from '../../src/browser.ts';
import type { CommandRunner, RunResult } from '../../src/process-runner.ts';

class BrowserRunner implements CommandRunner {
  readonly available: Set<string>;
  readonly probes: string[] = [];
  command = '';
  args: readonly string[] = [];

  constructor(available: readonly string[]) {
    this.available = new Set(available);
  }

  run(command: string, args: readonly string[] = []): Promise<RunResult> {
    this.command = command;
    this.args = args;
    return Promise.resolve({ exitCode: 0, stderr: '', stdout: '' });
  }

  which(command: string): Promise<boolean> {
    this.probes.push(command);
    return Promise.resolve(this.available.has(command));
  }
}

describe('browser opener', () => {
  it('selects the native macOS opener when it is available', async () => {
    const runner = new BrowserRunner(['open']);

    await expect(selectBrowserOpener(runner, {
      environment: {},
      kernelRelease: 'Darwin',
      platform: 'darwin'
    })).resolves.toBe('open');
    expect(runner.probes).toEqual(['open']);
  });

  it('selects xdg-open on desktop Linux', async () => {
    const runner = new BrowserRunner(['xdg-open']);

    await expect(selectBrowserOpener(runner, {
      environment: {},
      kernelRelease: '6.8.0-generic',
      platform: 'linux'
    })).resolves.toBe('xdg-open');
    expect(runner.probes).toEqual(['xdg-open']);
  });

  it('prefers wslview on WSL', async () => {
    const runner = new BrowserRunner(['wslview', 'explorer.exe', 'xdg-open']);

    await expect(selectBrowserOpener(runner, {
      environment: { WSL_DISTRO_NAME: 'Ubuntu' },
      kernelRelease: '6.6.87.2-microsoft-standard-WSL2',
      platform: 'linux'
    })).resolves.toBe('wslview');
    expect(runner.probes).toEqual(['wslview']);
  });

  it('uses explorer.exe when WSL does not provide wslview', async () => {
    const runner = new BrowserRunner(['explorer.exe']);

    await expect(selectBrowserOpener(runner, {
      environment: {},
      kernelRelease: '6.6.87.2-microsoft-standard-WSL2',
      platform: 'linux'
    })).resolves.toBe('explorer.exe');
    expect(runner.probes).toEqual(['wslview', 'explorer.exe']);
  });

  it('directs the developer to the urls command when no opener is available', async () => {
    const runner = new BrowserRunner([]);

    await expect(selectBrowserOpener(runner, {
      environment: { WSL_INTEROP: '/run/WSL/1_interop' },
      kernelRelease: '6.8.0-generic',
      platform: 'linux'
    })).rejects.toThrow(
      "No browser opener is available for WSL (tried wslview, explorer.exe). Run 'berth urls'"
    );
  });

  it('names the command the user actually invoked in the hint', async () => {
    const runner = new BrowserRunner([]);

    await expect(selectBrowserOpener(runner, {
      environment: {},
      kernelRelease: '6.8.0-generic',
      platform: 'linux'
    }, 'duke')).rejects.toThrow("Run 'duke urls'");
  });

  it('passes the URL as one literal process argument', async () => {
    const runner = new BrowserRunner(['xdg-open']);
    const url = 'http://feature.example.localhost/path?next=$(open bad)&name=one two';

    await openBrowser(runner, url, {
      environment: {},
      kernelRelease: '6.8.0-generic',
      platform: 'linux'
    });

    expect(runner.command).toBe('xdg-open');
    expect(runner.args).toEqual([url]);
  });
});
