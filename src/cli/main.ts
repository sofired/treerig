#!/usr/bin/env node
import { Command } from 'commander';
import { BRAND, cliName } from '../brand.ts';
import { formatIndexText } from '../checkout-index/render.ts';
import { createCommandContext, type CommandContext } from '../commands/context.ts';
import { list } from '../commands/list.ts';
import { remove } from '../commands/remove.ts';
import { down, status, up, urls } from '../commands/worktree.ts';
import { loadConfig, loadNearestConfig } from '../config/load.ts';
import { CliError } from '../errors.ts';
import { createConsoleLogger } from '../logger.ts';
import { resolveControlHome, resolveRouterPort } from '../layout.ts';
import { ExecaRunner } from '../process-runner.ts';
import { RouterService } from '../router/router.ts';

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
    routerPort: resolveRouterPort(process.env),
    commandName,
    logger,
    runner: new ExecaRunner()
  });
  // The router is machine-wide: no config file needed.
  const router = (): RouterService => new RouterService({
    controlHome: resolveControlHome(process.env),
    port: resolveRouterPort(process.env),
    runner: new ExecaRunner(),
    logger
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
  program.command('index')
    .description('refresh the machine-wide checkout index and print it')
    .action(async () => {
      const current = await context();
      logger.output(formatIndexText(await current.index.refresh(), current.index.url));
    });
  program.command('doctor').description('validate prerequisites and local state').action(notYet('doctor'));

  const routerCommand = program.command('router').description('manage the shared local router');
  routerCommand.command('up')
    .description('start the shared router and verify its hardened configuration')
    .action(async () => {
      const service = router();
      await service.ensure();
      logger.info(`Router is up on 127.0.0.1:${service.port} (network ${service.network}); index at ${service.indexUrl}.`);
    });
  routerCommand.command('status')
    .description('report whether the shared router is running as expected')
    .action(async () => {
      const status = await router().status();
      logger.output(status.running
        ? `running on 127.0.0.1:${status.port}, network ${status.network}`
        : `not running (would use 127.0.0.1:${status.port}, network ${status.network})`);
      for (const problem of status.problems) logger.output(`  problem: ${problem}`);
      if (status.running && status.problems.length > 0) process.exitCode = 1;
    });
  routerCommand.command('down')
    .description('stop the shared router; worktree services keep running but are no longer routed')
    .action(async () => {
      await router().down();
    });

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
