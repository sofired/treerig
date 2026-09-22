import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { parse, TomlError } from 'smol-toml';
import { BRAND } from '../brand.ts';
import { CliError } from '../errors.ts';
import { pathExists } from '../files.ts';
import { configSchema, type ProjectConfig } from './schema.ts';

export interface LoadedConfig {
  config: ProjectConfig;
  /** Absolute path of the file that was loaded. */
  file: string;
  /** Directory containing the config file; relative paths resolve from here. */
  root: string;
}

/**
 * Walks upward from `startDir` looking for the config file. Stops at the
 * filesystem root. Returns undefined rather than throwing so callers can give a
 * command-specific hint.
 */
export async function findConfigFile(startDir: string): Promise<string | undefined> {
  let current = path.resolve(startDir);
  while (true) {
    const candidate = path.join(current, BRAND.configFile);
    if (await pathExists(candidate)) return candidate;
    const parent = path.dirname(current);
    if (parent === current) return undefined;
    current = parent;
  }
}

export function parseConfig(contents: string, file: string): ProjectConfig {
  let raw: unknown;
  try {
    raw = parse(contents);
  } catch (error) {
    const detail = error instanceof TomlError ? error.message : String(error);
    throw new CliError(`${file} is not valid TOML: ${detail}`);
  }
  const result = configSchema.safeParse(raw);
  if (!result.success) {
    const issues = result.error.issues
      .map((issue) => `  ${issue.path.join('.') || '(root)'}: ${issue.message}`)
      .join('\n');
    throw new CliError(`${file} is not a valid ${BRAND.configFile}:\n${issues}`);
  }
  return result.data;
}

export async function loadConfig(file: string): Promise<LoadedConfig> {
  const resolved = path.resolve(file);
  let contents: string;
  try {
    contents = await readFile(resolved, 'utf8');
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') {
      throw new CliError(`No ${BRAND.configFile} at ${resolved}.`);
    }
    throw error;
  }
  return {
    config: parseConfig(contents, resolved),
    file: resolved,
    root: path.dirname(resolved)
  };
}

/**
 * Loads the nearest config file above `startDir`, or fails with a hint that
 * names something that exists: there is no verb yet that writes one.
 */
export async function loadNearestConfig(startDir = process.cwd()): Promise<LoadedConfig> {
  const file = await findConfigFile(startDir);
  if (!file) {
    throw new CliError(
      `No ${BRAND.configFile} found in ${startDir} or any parent directory. ` +
      `Create it at the root of the repository you want to manage, or point at an existing one with --config <path>. ` +
      `The Quick start in ${BRAND.name}'s README has a minimal template, and ${BRAND.exampleFile} in ` +
      `${BRAND.name}'s own source tree is the annotated version.`
    );
  }
  return loadConfig(file);
}
