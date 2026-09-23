// Oracle for internal/config's port of src/config/load.ts + schema.ts.
// Regenerate with:
//
//   export PATH=$HOME/.local/opt/node-v24.14.1-linux-x64/bin:$PATH
//   node tools/oracle/config-corpus.ts > internal/config/testdata/config-corpus.json
//
// Every case below runs through the real parseConfig (real smol-toml, real
// zod schema). A valid case records safeJson(result) — the exact bytes
// `config show` prints; an invalid one records the CliError message. Go's
// TOML library reports a different detail string than smol-toml for a syntax
// error, so cases whose failure is in the TOML itself (rather than the
// schema) are marked tomlSyntaxError and the Go test only checks the
// "<file> is not valid TOML:" prefix, not the detail after it.
import { parseConfig } from '../../src/config/load.ts';
import { CliError } from '../../src/errors.ts';
import { safeJson } from '../../src/text.ts';

interface Case {
  name: string;
  toml: string;
  tomlSyntaxError?: boolean;
}

const MINIMAL = `[project]
name = "acme"
domain = "acme.localhost"

[compose]
file = "docker/compose.yaml"
`;

const cases: Case[] = [
  { name: 'minimal file fills every default', toml: MINIMAL },

  {
    name: 'service defaults to host root and keeps index metadata',
    toml: `${MINIMAL}
[[service]]
name = "api"
[service.index]
label = "API"
`
  },

  {
    name: 'index.purpose defaults to empty string when given',
    toml: `${MINIMAL}
[[service]]
name = "api"
[service.index]
label = "API"
purpose = "the API"
`
  },

  {
    name: 'two services share a host when their paths differ',
    toml: `${MINIMAL}
[[service]]
name = "web"
[[service]]
name = "api"
path = "/api"
port = 3000
`
  },

  {
    name: 'duplicate host root claim',
    toml: `${MINIMAL}
[[service]]
name = "api"
[[service]]
name = "web"
`
  },

  {
    name: 'duplicate path claim on the host root',
    toml: `${MINIMAL}
[[service]]
name = "web"
[[service]]
name = "api"
path = "/api"
[[service]]
name = "api2"
path = "/api"
`
  },

  {
    name: 'duplicate claim on a named subdomain',
    toml: `${MINIMAL}
[[service]]
name = "a"
subdomain = "app"
[[service]]
name = "b"
subdomain = "app"
`
  },

  {
    name: 'duplicate service name',
    toml: `${MINIMAL}
[[service]]
name = "api"
subdomain = "a"
[[service]]
name = "api"
subdomain = "b"
`
  },

  {
    name: 'duplicate name and duplicate route reported for the same service',
    toml: `${MINIMAL}
[[service]]
name = "api"
[[service]]
name = "api"
`
  },

  {
    name: 'path with backtick and Host() injection rejected',
    toml: `${MINIMAL}
[[service]]
name = "s"
path = "/api\`)||Host(\`evil"
`
  },
  {
    name: 'path with bad percent-escape rejected',
    toml: `${MINIMAL}
[[service]]
name = "s"
path = "/a%zz"
`
  },
  {
    name: 'path with query string rejected',
    toml: `${MINIMAL}
[[service]]
name = "s"
path = "/search?q=1"
`
  },
  {
    name: 'path with dollar sign rejected (Compose would interpolate it)',
    toml: `${MINIMAL}
[[service]]
name = "s"
path = "/api$HOME"
`
  },
  {
    name: 'path with RFC 3986 punctuation and percent-escapes accepted',
    toml: `${MINIMAL}
[[service]]
name = "s"
path = "/v1/things:list/@scope/c++%2Fx"
`
  },
  {
    name: 'path of just the root slash accepted',
    toml: `${MINIMAL}
[[service]]
name = "s"
path = "/"
`
  },

  {
    name: 'subdomain starting with a hyphen rejected',
    toml: `${MINIMAL}
[[service]]
name = "s"
subdomain = "-mail"
`
  },
  {
    name: 'subdomain ending with a hyphen rejected',
    toml: `${MINIMAL}
[[service]]
name = "s"
subdomain = "mail-"
`
  },
  {
    name: 'subdomain over 63 characters rejected',
    toml: `${MINIMAL}
[[service]]
name = "s"
subdomain = "${'a'.repeat(64)}"
`
  },
  {
    name: 'subdomain of exactly 63 characters accepted',
    toml: `${MINIMAL}
[[service]]
name = "s"
subdomain = "${'a'.repeat(63)}"
`
  },

  {
    name: 'domain label over 63 characters rejected',
    toml: MINIMAL.replace('acme.localhost', `${'a'.repeat(64)}.localhost`)
  },
  {
    name: 'domain over 125 characters rejected',
    toml: MINIMAL.replace('acme.localhost', `${'ab.'.repeat(40)}localhost`)
  },
  {
    name: 'domain of exactly 125 characters accepted',
    toml: MINIMAL.replace('acme.localhost', `${'a'.repeat(61)}.${'b'.repeat(63)}`)
  },
  {
    name: 'domain with underscore rejected as not a valid hostname',
    toml: MINIMAL.replace('acme.localhost', 'ac_me.localhost')
  },

  {
    name: 'unknown top-level key rejected',
    toml: `${MINIMAL}
bogus = 1
`
  },
  {
    name: 'two unknown top-level keys listed in file order',
    toml: `zeta = 1
alpha = 2
${MINIMAL}`
  },
  {
    name: 'unknown key under [project] rejected',
    toml: `[project]
name = "acme"
domain = "acme.localhost"
bogus = 1

[compose]
file = "docker/compose.yaml"
`
  },
  {
    name: 'unknown key under [compose] rejected',
    toml: `[project]
name = "acme"
domain = "acme.localhost"

[compose]
file = "docker/compose.yaml"
bogus = 1
`
  },
  {
    name: 'unknown key on a service element rejected',
    toml: `${MINIMAL}
[[service]]
name = "api"
bogus = 1
`
  },
  {
    name: 'unknown key under service.index rejected',
    toml: `${MINIMAL}
[[service]]
name = "api"
[service.index]
label = "API"
extra = 1
`
  },
  {
    name: 'unknown keys inside [hooks] rejected',
    toml: `${MINIMAL}
[hooks]
prepair = "./x"
`
  },
  {
    name: 'control-home rejected: that path is per-machine, not committed',
    toml: `[project]
name = "acme"
control-home = "~/acme"
domain = "acme.localhost"

[compose]
file = "docker/compose.yaml"
`
  },

  {
    name: 'label-namespace accepted when it looks like a reverse domain',
    toml: `[project]
name = "acme"
domain = "acme.localhost"
label-namespace = "dev.example"

[compose]
file = "docker/compose.yaml"
`
  },
  {
    name: 'label-namespace rejected when it is not a reverse domain',
    toml: `[project]
name = "acme"
domain = "acme.localhost"
label-namespace = "NotADomain"

[compose]
file = "docker/compose.yaml"
`
  },

  {
    name: 'port over 65535 rejected',
    toml: `${MINIMAL}
[[service]]
name = "a"
port = 70000
`
  },
  {
    name: 'port of zero rejected',
    toml: `${MINIMAL}
[[service]]
name = "a"
port = 0
`
  },
  {
    name: 'port of exactly 1 accepted',
    toml: `${MINIMAL}
[[service]]
name = "a"
port = 1
`
  },
  {
    name: 'port of exactly 65535 accepted',
    toml: `${MINIMAL}
[[service]]
name = "a"
port = 65535
`
  },
  {
    name: 'fractional port rejected',
    toml: `${MINIMAL}
[[service]]
name = "a"
port = 80.5
`
  },
  {
    name: 'port written as 80.0 accepted as the integer 80',
    toml: `${MINIMAL}
[[service]]
name = "a"
port = 80.0
`
  },
  {
    name: 'port of 1e20, integral but beyond a safe integer, rejected',
    toml: `${MINIMAL}
[[service]]
name = "a"
port = 1e20
`
  },
  {
    name: 'port of -1e20 rejected as below a safe integer and below 1',
    toml: `${MINIMAL}
[[service]]
name = "a"
port = -1e20
`
  },
  {
    name: 'port of inf rejected',
    toml: `${MINIMAL}
[[service]]
name = "a"
port = inf
`
  },
  {
    name: 'port given as a string rejected',
    toml: `${MINIMAL}
[[service]]
name = "a"
port = "80"
`
  },

  {
    name: 'missing project.name rejected',
    toml: `[project]
domain = "acme.localhost"

[compose]
file = "docker/compose.yaml"
`
  },
  {
    name: 'missing [project] table rejected',
    toml: `[compose]
file = "docker/compose.yaml"
`
  },
  {
    name: 'missing [compose] table rejected',
    toml: `[project]
name = "acme"
domain = "acme.localhost"
`
  },
  {
    name: 'missing compose.file rejected',
    toml: `[project]
name = "acme"
domain = "acme.localhost"

[compose]
`
  },
  {
    name: 'empty compose.file rejected',
    toml: `[project]
name = "acme"
domain = "acme.localhost"

[compose]
file = ""
`
  },

  {
    name: 'project.name of the wrong type rejected',
    toml: `[project]
name = 5
domain = "acme.localhost"

[compose]
file = "docker/compose.yaml"
`
  },
  {
    name: 'project.domain of the wrong type rejected',
    toml: `[project]
name = "acme"
domain = 5

[compose]
file = "docker/compose.yaml"
`
  },
  {
    name: 'project.name with uppercase or symbols rejected',
    toml: `[project]
name = "Not_A-Slug!"
domain = "acme.localhost"

[compose]
file = "docker/compose.yaml"
`
  },

  {
    name: 'several issues at once, unrecognized key reported last',
    toml: `[project]
name = "BAD NAME"
domain = 5

[compose]

[[service]]
name = "BAD"

bogus = true
`
  },

  {
    name: 'a non-fatal issue does not suppress the duplicate-service check',
    toml: `[project]
name = "BAD NAME"
domain = "acme.localhost"

[compose]
file = "x"

[[service]]
name = "api"
[[service]]
name = "api"
`
  },
  {
    name: 'a missing required field suppresses the duplicate-service check',
    toml: `[project]
domain = "acme.localhost"

[compose]
file = "x"

[[service]]
name = "api"
[[service]]
name = "api"
`
  },
  {
    name: 'an unrecognized key on one service suppresses the duplicate-service check',
    toml: `${MINIMAL}
[[service]]
name = "web"
bogus = 1
[[service]]
name = "web"
`
  },

  { name: 'empty [hooks] table', toml: `${MINIMAL}
[hooks]
` },
  {
    name: 'hooks with an empty string value rejected',
    toml: `${MINIMAL}
[hooks]
prepare = ""
`
  },
  {
    name: 'all five hook stages configured',
    toml: `${MINIMAL}
[hooks]
prepare = "./prepare.sh"
seed = "./seed.sh"
verify = "./verify.sh"
backup = "./backup.sh"
restore = "./restore.sh"
`
  },

  {
    name: 'output key order follows the schema, not the file',
    toml: `[compose]
file = "docker/compose.yaml"

[project]
domain = "acme.localhost"
name = "acme"
`
  },
  {
    name: 'service field order in output follows the schema, not the file',
    toml: `${MINIMAL}
[[service]]
port = 3000
name = "api"
subdomain = "x"
`
  },

  // TOML allows another table between two [[service]] blocks; every block
  // keeps its own fields, and every table its own key order.
  {
    name: 'a table between two [[service]] blocks leaves both services whole',
    toml: `${MINIMAL}
[[service]]
name = "api"
[hooks]
verify = "./check.sh"
[[service]]
name = "mail"
subdomain = "mail"
`
  },
  {
    name: 'an index table after an interleaved table belongs to the service above it',
    toml: `[[service]]
name = "api"
[project]
name = "acme"
domain = "acme.localhost"
[[service]]
name = "mail"
subdomain = "mail"
[service.index]
label = "Mail"
[compose]
file = "compose.yaml"
`
  },
  {
    name: 'unknown keys in an interleaved second service are reported in file order',
    toml: `${MINIMAL}
[[service]]
name = "api"
[hooks]
seed = "./seed.sh"
[[service]]
zeta = 1
name = "mail"
alpha = 2
`
  },
  {
    name: 'an inline array of service tables',
    toml: `${MINIMAL}
service = [{ name = "api" }, { name = "mail", subdomain = "mail" }]
`
  },

  {
    name: 'invalid TOML reported distinctly from invalid configuration',
    toml: '[project\nname = ',
    tomlSyntaxError: true
  },
  {
    name: 'invalid TOML: unterminated string',
    toml: `${MINIMAL}\nextra = "unterminated`,
    tomlSyntaxError: true
  }
];

const results = cases.map((c) => {
  try {
    const config = parseConfig(c.toml, 'test.toml');
    return { name: c.name, toml: c.toml, ok: true, want: safeJson(config), tomlSyntaxError: false };
  } catch (error) {
    if (!(error instanceof CliError)) throw error;
    return {
      name: c.name,
      toml: c.toml,
      ok: false,
      want: error.message,
      tomlSyntaxError: c.tomlSyntaxError === true
    };
  }
});

process.stdout.write(JSON.stringify(results, null, 2) + '\n');
