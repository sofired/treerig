import { afterEach, describe, expect, it, vi } from 'vitest';
import { createConsoleLogger, silentLogger } from '../../src/logger.ts';

// Which stream a line goes to is a contract: anything the tool says about
// itself has to stay out of output someone is piping.

describe('the console logger', () => {
  const out: string[] = [];
  const err: string[] = [];
  const capture = (): void => {
    // Every argument, not just the first: a line assembled from several would
    // otherwise be recorded as its first word and every assertion still pass.
    vi.spyOn(console, 'log').mockImplementation((...parts: unknown[]) => void out.push(parts.map(String).join(' ')));
    vi.spyOn(console, 'error').mockImplementation((...parts: unknown[]) => void err.push(parts.map(String).join(' ')));
  };

  afterEach(() => {
    vi.restoreAllMocks();
    out.length = 0;
    err.length = 0;
  });

  it('keeps everything it says about itself off stdout', () => {
    capture();
    const logger = createConsoleLogger('berth');
    logger.info('Waiting for the shared router lock.');
    logger.warn('Could not refresh the checkout index.');
    logger.error('Docker is not available.');
    logger.output('{"project":"acme"}');

    expect(out).toEqual(['{"project":"acme"}']);
    expect(err).toEqual([
      '[berth] Waiting for the shared router lock.',
      '[berth] WARNING: Could not refresh the checkout index.',
      '[berth] ERROR: Docker is not available.'
    ]);
  });

  it('answers to the name it was invoked as', () => {
    capture();
    createConsoleLogger('duke').info('Ready.');
    expect(err).toEqual(['[duke] Ready.']);
  });

  it('will not let another process drive the terminal through a message', () => {
    capture();
    const logger = createConsoleLogger('berth');
    // Whatever Git, Docker or a lock record said reaches these three.
    logger.warn('fatal: \u001b[2Knot a git repository');
    logger.error('held by \u202eevil');

    expect(err).toEqual([
      '[berth] WARNING: fatal: \uFFFD[2Knot a git repository',
      '[berth] ERROR: held by \uFFFDevil'
    ]);
    // Output is this tool's own composition, and is passed through as written.
    logger.output('{"branch":"feature/\u202ex"}');
    expect(out).toEqual(['{"branch":"feature/\u202ex"}']);
  });

  it('keeps a message that is several lines several lines', () => {
    capture();
    const logger = createConsoleLogger('berth');
    // A list of config problems, a captured stderr, a remedy to copy: the line
    // breaks are this tool's, and only what is inside a line is suspect.
    logger.error(`berth.toml is not valid:\n  project.name: required\n  compose.file: \u001b[2Kmissing`);

    // Indented after the first: an unindented continuation, which the text
    // below the prefix controls, could otherwise read as a line berth wrote.
    expect(err).toEqual([
      '[berth] ERROR: berth.toml is not valid:\n    project.name: required\n    compose.file: \uFFFD[2Kmissing'
    ]);
  });

  it('will not let borrowed text forge a line of its own', () => {
    capture();
    createConsoleLogger('berth').error('docker said:\n[berth] ERROR: everything is fine');
    expect(err).toEqual(['[berth] ERROR: docker said:\n  [berth] ERROR: everything is fine']);
  });

  it('leaves output exactly as it was composed, newlines and all', () => {
    capture();
    createConsoleLogger('berth').output('line one\nline two');
    expect(out).toEqual(['line one\nline two']);
  });

  it('will not let a separator that is not a newline forge a line either', () => {
    capture();
    // U+2028 is a line terminator no newline split sees.
    createConsoleLogger('berth').error('docker said:\u2028[berth] ERROR: everything is fine');
    expect(err).toEqual(['[berth] ERROR: docker said:\uFFFD[berth] ERROR: everything is fine']);
    expect(err.join('')).not.toContain('\u2028');
  });

  it('discards everything when it is silent', () => {
    capture();
    silentLogger.info('a');
    silentLogger.warn('b');
    silentLogger.error('c');
    silentLogger.output('d');
    expect([...out, ...err]).toEqual([]);
    // And the capture that saw none of it is live.
    createConsoleLogger('berth').info('x');
    expect(err).toEqual(['[berth] x']);
  });
});
