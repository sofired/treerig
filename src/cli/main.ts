#!/usr/bin/env node
import { Command } from 'commander';
import { BRAND, cliName } from '../brand.ts';
import { createCommandContext, type CommandContext } from '../commands/context.ts';
import { list } from '../commands/list.ts';
import { remove } from '../commands/remove.ts';
import { down, status, up, urls } from '../commands/worktree.ts';
import { loadConfig, loadNearestConfig } from '../config/load.ts';
import { CliError } from '../errors.ts';
import { createConsoleLogger } from '../logger.ts';
import { resolveControlHome } from '../layout.ts';
import { ExecaRunner } from '../process-runner.ts';

const VERSION = '0.1.0';

// The one place argv is read for a name. Everything downstream receives it as
// a value, so a binary installed as `duke` says `duke` in every message.
const commandName = cliName(process.argv);
const logger = createConsoleLogger(commandName);

function notYet(command: string): () => never {
  return () => {
    throw new CliError(
      `'${commandName} ${command}' is not implemented yet. ` +
      'The router, checkout index, and cleanup are the next ports.',
      2
    );
  };
}

async function main(argv: readonly string[]): Promise<void> {
  const program = new Command()
    .name(commandName)
    .description('Give every Git worktree a durable identity, an isolated home, and a routed URL.')
    .version(VERSION)
    .option('--config <path>', `path to ${BRAND.configFile} (default: nearest above cwd)`);

  const config = program.command('config').description('inspect the project configuration');

  config.command('validate')
    .description(`parse and validate ${BRAND.configFile}`)
    .action(async () => {
      const loaded = await load(program.opts<{ config?: string }>().config);
      const services = loaded.config.service.length;
      const hooks = Object.keys(loaded.config.hooks).length;
      logger.info(`${loaded.file} is valid.`);
      logger.info(`project ${loaded.config.project.name} · domain ${loaded.config.project.domain} · ${services} service(s) · ${hooks} hook(s)`);
      logger.info(`control home: ${resolveControlHome(process.env)}`);
    });

  config.command('show')
    .description('print the resolved configuration as JSON')
    .action(async () => {
      const loaded = await load(program.opts<{ config?: string }>().config);
      logger.output(JSON.stringify(loaded.config, null, 2));
    });

  const context = async (): Promise<CommandContext> => createCommandContext({
    loaded: await load(program.opts<{ config?: string }>().config),
    controlHome: resolveControlHome(process.env),
    commandName,
    logger,
    runner: new ExecaRunner()
  });

  program.command('dev').description('start this worktree\'s environment and follow its logs').action(notYet('dev'));
  program.command('up [services...]')
    .description('register this worktree, start its services, and run its hooks')
    .action(async (services: string[]) => {
      await up(await context(), { services });
    });
  program.command('down')
    .description('stop this worktree\'s services; data is kept')
    .action(async () => {
      await down(await context());
    });
  program.command('status')
    .description('show service status for this worktree')
    .action(async () => {
      await status(await context());
    });
  program.command('urls')
    .description('print this worktree\'s routed URLs')
    .action(async () => {
      await urls(await context());
    });
  program.command('doctor').description('validate prerequisites and local state').action(notYet('doctor'));

  const worktree = program.command('worktree').description('manage registered worktrees');
  worktree.command('list')
    .description('list registered worktrees')
    .option('--all', 'every repository and domain on this machine')
    .option('--json', 'machine-readable output')
    .action(async (options: { all?: boolean; json?: boolean }) => {
      await list(await context(), options);
    });
  worktree.command('remove [identifier]')
    .description('remove a worktree\'s environment: containers, volumes, networks, images, home, and registration')
    .option('--domain <domain>', 'narrow a slug registered under more than one domain')
    .option('--yes', 'skip the confirmation phrase')
    .action(async (identifier: string | undefined, options: { domain?: string; yes?: boolean }) => {
      await remove(await context(), { identifier, domain: options.domain, approved: options.yes });
    });
  worktree.command('unlock [slug]').description('clear stale locks for a worktree').action(notYet('worktree unlock'));

  await program.parseAsync([...argv]);
}

async function load(explicit: string | undefined) {
  return explicit ? loadConfig(explicit) : loadNearestConfig(process.cwd(), commandName);
}

try {
  await main(process.argv);
} catch (error) {
  if (error instanceof CliError) {
    logger.error(error.message);
    process.exitCode = error.exitCode;
  } else {
    throw error;
  }
}
