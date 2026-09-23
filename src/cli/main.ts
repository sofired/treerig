#!/usr/bin/env node
import { Command } from 'commander';
import { BRAND, cliName } from '../brand.ts';
import { formatIndexText } from '../checkout-index/render.ts';
import { createCommandContext, type CommandContext } from '../commands/context.ts';
import { doctor, doctorExitCode, doctorSummary, findCheckout, formatDoctorReport } from '../commands/doctor.ts';
import { list } from '../commands/list.ts';
import { remove } from '../commands/remove.ts';
import { unlock } from '../commands/unlock.ts';
import { down, status, up, urls } from '../commands/worktree.ts';
import { loadConfig, loadNearestConfig } from '../config/load.ts';
import type { HookStage } from '../config/schema.ts';
import { INVOKED_STAGES } from '../hooks/protocol.ts';
import { CliError } from '../errors.ts';
import { createConsoleLogger } from '../logger.ts';
import { printable, printableLines, safeJson } from '../text.ts';
import { resolveControlHome, resolveRouterPort } from '../layout.ts';
import { ExecaRunner } from '../process-runner.ts';
import { routerStatusReport, RouterService } from '../router/router.ts';

const VERSION = '0.1.0';

// The one place argv is read for a name. Everything downstream receives it as
// a value, so an installation symlinked as `duke` says `duke` in every message.
const commandName = cliName(process.argv);
const logger = createConsoleLogger(commandName);

function notYet(command: string): () => never {
  return () => {
    throw new CliError(
      `'${commandName} ${command}' is not implemented yet; see the Roadmap in README.md.`,
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
      const configured = Object.keys(loaded.config.hooks) as HookStage[];
      // Counted separately, because a report about what a file does must not
      // count the hooks nothing invokes yet among the ones that run.
      const inert = configured.filter((stage) => !INVOKED_STAGES.includes(stage));
      const live = configured.length - inert.length;
      // A validator's report is what was asked for, so it goes to stdout the
      // way every other reporting tool's does.
      logger.output(printable(`${loaded.file} is valid.`));
      logger.output(printable(
        `project ${loaded.config.project.name} · domain ${loaded.config.project.domain} · ` +
        `${services} service(s) · ${live} hook(s)`
      ));
      if (inert.length > 0) logger.output(`accepted but not invoked yet: ${inert.join(', ')}`);
      logger.output(printable(`control home: ${resolveControlHome(process.env)}`));
    });

  config.command('show')
    .description('print the resolved configuration as JSON')
    .action(async () => {
      const loaded = await load(program.opts<{ config?: string }>().config);
      logger.output(safeJson(loaded.config));
    });

  // What a waiter on one of this process's locks is told it is waiting for.
  // It is persisted in lock records: redact here any flag that ever carries a secret.
  const invocation = [commandName, ...argv.slice(2)].join(' ');
  const context = async (): Promise<CommandContext> => createCommandContext({
    loaded: await load(program.opts<{ config?: string }>().config),
    controlHome: resolveControlHome(process.env),
    routerPort: resolveRouterPort(process.env),
    commandName,
    invocation,
    logger,
    runner: new ExecaRunner()
  });
  // The router is machine-wide: no config file needed.
  const router = (runner = new ExecaRunner()): RouterService => new RouterService({
    controlHome: resolveControlHome(process.env),
    port: resolveRouterPort(process.env),
    runner,
    logger,
    invocation,
    commandName
  });

  program.command('dev').description('start this worktree\'s environment and follow its logs (not yet implemented)').action(notYet('dev'));
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
  program.command('doctor')
    .description('check prerequisites, the registry, the router, and this checkout\'s verify hook')
    .action(async () => {
      const runner = new ExecaRunner();
      let source: RouterService | Error;
      try {
        // One runner for the whole command, the router's queries included.
        source = router(runner);
      } catch (error) {
        source = error instanceof Error ? error : new Error(String(error));
      }
      const sections = await doctor({
        commandName,
        controlHome: resolveControlHome(process.env),
        runner,
        logger,
        router: source,
        checkout: await findCheckout(program.opts<{ config?: string }>().config, process.cwd()),
        invocation
      });
      // The report is the answer, so it is stdout's; how it went is one line on stderr.
      for (const line of formatDoctorReport(sections)) logger.output(line);
      const summary = doctorSummary(sections);
      logger[summary.level](summary.message);
      process.exitCode = doctorExitCode(sections);
    });

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
      const report = routerStatusReport(await router().status());
      for (const line of report.output) logger.output(printable(line));
      for (const line of report.errors) logger.error(line);
      if (report.failed) process.exitCode = 1;
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
    .option('--label-namespace <namespace>', 'supply the Docker label namespace when nothing on disk records it (a worktree of another repository)')
    .option('--yes', 'skip the confirmation phrase')
    .action(async (identifier: string | undefined, options: { domain?: string; labelNamespace?: string; yes?: boolean }) => {
      await remove(await context(), { identifier, domain: options.domain, labelNamespace: options.labelNamespace, approved: options.yes });
    });
  worktree.command('unlock [identifier]')
    .description('clear a dead holder\'s lock on a worktree, once the holder and every command it started on this machine are checked gone')
    .option('--domain <domain>', 'narrow a slug registered under more than one domain')
    .option('--holder-unverifiable', 'accept a holder this machine cannot check (another machine or container, or no record), once you have made sure it is gone')
    .action(async (identifier: string | undefined, options: { domain?: string; holderUnverifiable?: boolean }) => {
      await unlock(await context(), { identifier, domain: options.domain, allowUnverifiable: options.holderUnverifiable });
    });

  await program.parseAsync([...argv]);
}

async function load(explicit: string | undefined) {
  return explicit ? loadConfig(explicit) : loadNearestConfig(process.cwd());
}

/** A failure that hid others — a hook that failed while its lock could not be released, say — shows all of them. */
function report(error: Error): void {
  logger.error(error.message);
  const describe = (item: unknown): string => item instanceof Error ? item.message : String(item);
  if (error.cause !== undefined) logger.error(`  caused by: ${describe(error.cause)}`);
  const suppressed = (error as { suppressed?: unknown[] }).suppressed ?? [];
  for (const item of suppressed) {
    if (item !== error.cause) logger.error(`  also: ${describe(item)}`);
  }
}

try {
  await main(process.argv);
} catch (error) {
  // Every failure ends as one prefixed line and an exit code; a stack trace
  // is for whoever asked for it.
  const debug = ['1', 'true', 'yes', 'on'].includes((process.env[`${BRAND.envPrefix}_DEBUG`] ?? '').trim().toLowerCase());
  if (error instanceof CliError) {
    report(error);
    process.exitCode = error.exitCode;
  } else {
    if (error instanceof Error) report(error);
    else logger.error(String(error));
    if (!debug) logger.error(`  (set ${BRAND.envPrefix}_DEBUG=1 to see the stack trace)`);
    process.exitCode = 1;
  }
  // Per line, so the frames keep their shape and nothing inside one drives the terminal.
  if (debug) console.error(printableLines(error instanceof Error ? error.stack ?? error.message : String(error)));
}
