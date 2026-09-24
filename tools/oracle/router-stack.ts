// Emits TypeScript's protocol-3 router stack for fixed inputs, so Go's
// stack builder can be checked against it byte for byte apart from the
// ledgered protocol-4 changes (WP3, #39): the protocol label and record
// value bumped from 3 to 4, and the dynamic/index mount sources moved from
// under a control home to the machine-wide router state directory. See
// internal/router/stack_oracle_test.go for how the two are reconciled, and
// docs/port/divergences.md for the ledger entries.
//
//   node tools/oracle/router-stack.ts > internal/router/testdata/stack-oracle.json
import { routerStack } from '../../src/router/stack.ts';

const document = routerStack({
  projectName: 'berth-router',
  network: 'berth-router',
  port: 80,
  dynamicDir: '/home/dev/.berth/router/dynamic',
  indexDir: '/home/dev/.berth/router/index',
  labelNamespace: 'dev.berth'
});

process.stdout.write(`${JSON.stringify(document, null, 2)}\n`);
