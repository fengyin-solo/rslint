import path from 'node:path';
import fs from 'node:fs';
import {
  OUTPUT_FORMATS,
  parseArgs,
  isJSConfigFile,
  isOutputFormat,
} from '../utils/args.js';
import { resolveRslintBinary } from '../internal/resolve-binary.js';

export type RunCLIOptions = {
  /**
   * The command-line arguments to parse, matching the shape of Node.js `process.argv`
   * @default process.argv
   */
  argv?: string[];
};

export async function run(
  binPath: string,
  argv: string[],
  startTime: number,
): Promise<number> {
  const cwd = process.cwd();
  const args = parseArgs(argv);

  // --init: pass through to Go (no config payload — Go writes the default
  // config to disk and prints the "Created …" line, forwarded via `output`).
  // It intentionally takes priority over unrelated lint flags, matching the
  // existing fast-path contract.
  if (args.init) {
    const { runEngine } = await import('./engine.js');
    return runEngine({ binPath, goArgs: ['--init'], cwd });
  }

  // Reject an invalid stdout protocol before config discovery/evaluation.
  // Help retains its existing priority and is forwarded to Go, which owns the
  // usage text. Go validates format again after the IPC init payload is merged
  // so every CLI entry path receives the same single-error behavior.
  if (!args.help && args.format !== null && !isOutputFormat(args.format)) {
    process.stderr.write(
      `error: invalid output format ${JSON.stringify(args.format)} (expected ${OUTPUT_FORMATS.slice(0, -1).join(', ')}, or ${OUTPUT_FORMATS.at(-1)})\n`,
    );
    return 2;
  }

  // Validate explicit --config flag
  if (args.config) {
    const configPath = path.resolve(cwd, args.config);
    if (!fs.existsSync(configPath)) {
      process.stderr.write(`Error: config file not found: ${configPath}\n`);
      return 1;
    }
    if (!isJSConfigFile(configPath)) {
      const migrationHint = /\.jsonc?$/.test(configPath)
        ? ' Run `rslint --init` to migrate a legacy JSON config.'
        : '';
      process.stderr.write(
        `Error: unsupported config file: ${configPath}\n` +
          `Rslint configs must be JavaScript or TypeScript modules.${migrationHint}\n`,
      );
      return 1;
    }
  }

  // --watch is orchestrated in this process (it drives the persistent --api
  // service), so validate the flag combination here before spawning anything.
  // --help still falls through to the Go binary's usage text.
  if (args.watch && !args.help) {
    const watchError = validateWatchArgs(args);
    if (watchError) {
      process.stderr.write(`error: ${watchError}\n`);
      return 2;
    }
    const { runWatch, parseRuleOverrides } = await import('./watch/index.js');
    const ruleFlags: string[] = [];
    for (let index = 0; index < args.rest.length; index += 1) {
      if (args.rest[index] === '--rule' && index + 1 < args.rest.length) {
        ruleFlags.push(args.rest[index + 1]);
        index += 1;
      }
    }
    let ruleOverrides: import('../config/define-config.js').RulesRecord | null =
      null;
    try {
      if (ruleFlags.length > 0) ruleOverrides = parseRuleOverrides(ruleFlags);
    } catch (error) {
      process.stderr.write(
        `error: ${error instanceof Error ? error.message : String(error)}\n`,
      );
      return 2;
    }
    let maxWarnings = -1;
    if (args.maxWarnings !== null) {
      const parsed = Number.parseInt(args.maxWarnings, 10);
      if (!Number.isFinite(parsed)) {
        process.stderr.write(
          `error: invalid value ${JSON.stringify(args.maxWarnings)} for --max-warnings\n`,
        );
        return 2;
      }
      maxWarnings = parsed;
    }
    let debounceMs: number | undefined;
    let pollMs: number | undefined;
    try {
      debounceMs = readPositiveIntEnv('RSLINT_WATCH_DEBOUNCE_MS');
      pollMs = readPositiveIntEnv('RSLINT_WATCH_POLL_MS');
    } catch (error) {
      process.stderr.write(
        `error: ${error instanceof Error ? error.message : String(error)}\n`,
      );
      return 2;
    }
    return runWatch({
      cwd,
      positionalPatterns: args.positionals,
      fix: args.fix,
      quiet: args.quiet,
      maxWarnings,
      configFile: args.config ? path.resolve(cwd, args.config) : null,
      ruleOverrides,
      ...(debounceMs === undefined ? {} : { debounceMs }),
      ...(pollMs === undefined ? {} : { pollMs }),
    });
  }

  // Build Go args: start-time flag BEFORE positional args, because Go's
  // flag.Parse stops at the first positional argument.
  const goArgs = [`--start-time=${startTime}`, ...args.rest];

  // Configuration discovery is initiated by Go after the IPC channel is
  // live; Node only evaluates the exact frontier candidates Go sends back
  // through reverse RPC.
  const explicitConfigPath = args.config
    ? path.resolve(cwd, args.config)
    : null;
  const { runEngine } = await import('./engine.js');
  return runEngine({
    binPath,
    goArgs,
    cwd,
    runtime: { singleThreaded: args.singleThreaded },
    extraInit: {
      configDiscovery: {
        explicitConfigPath: explicitConfigPath ?? undefined,
      },
    },
  });
}

export async function runCLI({
  argv = process.argv,
}: RunCLIOptions = {}): Promise<void> {
  const startTime = Date.now();
  const exitCode = await run(resolveRslintBinary(), argv.slice(2), startTime);
  // Let stdout/stderr flush naturally instead of terminating the process.
  process.exitCode = exitCode;
}

/**
 * Watch mode is a Node-hosted session over the persistent --api service, so
 * options only the one-shot Go pipeline can service are rejected up front
 * with one clear error instead of being silently ignored mid-session.
 */
export function validateWatchArgs(args: {
  typeCheck?: boolean;
  typeCheckOnly?: boolean;
  format?: string | null;
  rest?: readonly string[];
}): string | null {
  if (args.typeCheck || args.typeCheckOnly) {
    return '--watch cannot be combined with --type-check or --type-check-only (type checking is not available in watch mode yet)';
  }
  if (
    args.format !== null &&
    args.format !== undefined &&
    args.format !== 'default'
  ) {
    return `--watch only supports the default output format (got --format ${args.format})`;
  }
  const unsupported = new Set(['--timing', '--trace', '--cpuprof']);
  for (const token of args.rest ?? []) {
    if (unsupported.has(token)) {
      return `--watch cannot be combined with ${token}`;
    }
  }
  return null;
}

function readPositiveIntEnv(name: string): number | undefined {
  const raw = process.env[name];
  if (raw === undefined || raw === '') return undefined;
  const parsed = Number.parseInt(raw, 10);
  if (!Number.isFinite(parsed) || parsed < 0) {
    throw new Error(
      `invalid ${name}=${JSON.stringify(raw)}: expected a non-negative integer`,
    );
  }
  return parsed;
}
