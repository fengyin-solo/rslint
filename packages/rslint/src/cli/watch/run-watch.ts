/**
 * Watch mode orchestrator.
 *
 * Drives ONE persistent linter service (the long-lived Go --api process held
 * by the Rslint class) through debounced lint rounds:
 *
 *  - round 1 always lints the full requested scope;
 *  - later rounds lint only the changed files, merging per-file results into
 *    the DiagnosticStore;
 *  - config/config-extension/.gitignore changes invalidate everything and run
 *    a full round with freshly re-discovered configuration;
 *  - deletions and renames update or remove remembered results deterministically;
 *  - --fix writes once per round and suppresses its own file events, so a
 *    fix round can never schedule itself again.
 *
 * The process never changes terminal state (no alt screen, no cursor hiding,
 * no clear screen) and exits on SIGINT/SIGTERM/SIGHUP with the latest round's
 * result code after closing the resident service.
 */
import fsp from 'node:fs/promises';
import path from 'node:path';

import { nativePathIdentity } from '../../api/path-identity.js';
import type { LintResult } from '../../api/rslint.js';
import type {
  RuleEntry,
  RuleSeverity,
  RulesRecord,
} from '../../config/define-config.js';
import { DiagnosticStore, type RoundDelta } from './store.js';
import { createRenderer, resolveColorEnabled } from './render.js';
import {
  createFileWatcher,
  type FileWatcher,
  type WatchEvent,
} from './watcher.js';
import { collectConfigSensitivePaths } from './config-deps.js';

// Mirrors discovery.AutoJSConfigFileNames plus the CommonJS/CTS module names
// accepted by an explicit --config path.
const CONFIG_FILE_NAMES = new Set([
  'rslint.config.js',
  'rslint.config.mjs',
  'rslint.config.cjs',
  'rslint.config.ts',
  'rslint.config.mts',
  'rslint.config.cts',
]);
const IGNORE_FILE_NAME = '.gitignore';
// Mirrors config.DefaultLintFileExtensions.
const LINTABLE_EXTENSIONS = new Set([
  '.js',
  '.mjs',
  '.cjs',
  '.jsx',
  '.ts',
  '.tsx',
  '.mts',
  '.cts',
]);
const PRUNE_DIRECTORY_NAMES = new Set(['node_modules', '.git']);
const DEFAULT_DEBOUNCE_MS = 100;
const DEFAULT_POLL_MS = 1000;
const DEFAULT_GLOB = '**/*.{js,mjs,cjs,jsx,ts,tsx,mts,cts}';

export interface WatchLinter {
  lintFiles(patterns: string | string[]): Promise<LintResult[]>;
  close(): Promise<void>;
}

export interface SignalHarness {
  /** Subscribe to interrupt/termination signals. Returns an unsubscribe. */
  onSignal(listener: (signalName: string) => void): () => void;
}

export interface RoundInfo {
  round: number;
  kind: 'initial' | 'full' | 'files';
  results: readonly LintResult[];
  totals: import('./store.js').StoreTotals;
  exitCode: number;
  error?: string;
}

export interface RunWatchOptions {
  cwd: string;
  /** Raw positional patterns from the user (files/dirs/globs). */
  positionalPatterns?: readonly string[];
  fix?: boolean;
  quiet?: boolean;
  /** -1/unset disables the warning budget. */
  maxWarnings?: number;
  /** Resolved absolute explicit config path (--config), if any. */
  configFile?: string | null;
  /** Synthetic rule overrides parsed from repeatable --rule flags. */
  ruleOverrides?: RulesRecord | null;
  stdout?: NodeJS.WritableStream;
  stderr?: NodeJS.WritableStream;
  /** Forces the color decision; defaults follow TTY/NO_COLOR/FORCE_COLOR. */
  color?: boolean;
  debounceMs?: number;
  pollMs?: number;
  /** Test seam: build the persistent linter (default: new Rslint(...)). */
  createLinter?: (options: {
    cwd: string;
    fix: boolean;
    configFile: string | null;
    ruleOverrides: RulesRecord | null;
  }) => WatchLinter | Promise<WatchLinter>;
  /** Test seam: the file watcher (default: fs.watch + safety poll). */
  createWatcher?: typeof createFileWatcher;
  /** Test seam: signal subscription (default: process SIGINT/TERM/HUP). */
  signals?: SignalHarness;
  /**
   * Test seam: register for stdin-end shutdown. Returning false keeps the
   * session running when stdin closes (default registers on non-TTY stdin).
   */
  handleStdinEnd?: boolean;
  /** Notified after every finished round (including failed ones). */
  onRound?: (info: RoundInfo) => void;
}

const defaultSignals: SignalHarness = {
  onSignal(listener) {
    const pairs: Array<[NodeJS.Signals, () => void]> = [
      [
        'SIGINT',
        () => {
          listener('SIGINT');
        },
      ],
      [
        'SIGTERM',
        () => {
          listener('SIGTERM');
        },
      ],
      [
        'SIGHUP',
        () => {
          listener('SIGHUP');
        },
      ],
    ];
    for (const [name, handler] of pairs) process.on(name, handler);
    return () => {
      for (const [name, handler] of pairs) process.off(name, handler);
    };
  },
};

interface PlannedScope {
  roots: string[];
  files: string[];
}

interface PendingAction {
  full: boolean;
  content: Map<string, string>; // key -> absolute path
  deleted: Map<string, string>;
  reasons: string[];
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function isRuleSeverity(value: unknown): value is RuleSeverity {
  return (
    value === 'off' ||
    value === 'warn' ||
    value === 'error' ||
    value === 0 ||
    value === 1 ||
    value === 2
  );
}

/**
 * Parse repeatable --rule values the same way the one-shot Go CLI does:
 *   "rule: error" | "rule: warn" | "rule: off" | 'rule: ["error", {...}]'
 * Returns a rules record usable as a flat-config entry.
 */
export function parseRuleOverrides(flags: readonly string[]): RulesRecord {
  const rules: RulesRecord = {};
  for (const flag of flags) {
    const separator = flag.indexOf(':');
    if (separator < 1) {
      throw new Error(
        `invalid --rule format ${JSON.stringify(flag)}: expected "ruleName: severity"`,
      );
    }
    const name = flag.slice(0, separator).trim();
    const rawValue = flag.slice(separator + 1).trim();
    if (name === '' || rawValue === '') {
      throw new Error(
        `invalid --rule format ${JSON.stringify(flag)}: expected "ruleName: severity"`,
      );
    }
    let entry: RuleEntry;
    if (rawValue.startsWith('[')) {
      let parsed: unknown;
      try {
        parsed = JSON.parse(rawValue);
      } catch {
        throw new Error(
          `invalid --rule format ${JSON.stringify(flag)}: rule options must be a JSON array`,
        );
      }
      if (
        !Array.isArray(parsed) ||
        parsed.length === 0 ||
        !isRuleSeverity(parsed[0])
      ) {
        throw new Error(
          `invalid --rule format ${JSON.stringify(flag)}: rule options must be a JSON array starting with a severity`,
        );
      }
      const [severity, ...options] = parsed;
      entry = [severity, ...options];
    } else {
      if (!isRuleSeverity(rawValue)) {
        throw new Error(
          `invalid --rule severity ${JSON.stringify(rawValue)}: expected "off", "warn", or "error"`,
        );
      }
      entry = rawValue;
    }
    rules[name] = entry;
  }
  return rules;
}

function isDynamicPattern(pattern: string): boolean {
  return /[*?[\]{}]/.test(pattern);
}

/** Static (non-glob) directory a pattern is rooted at, mirroring the JS API. */
function staticGlobRoot(pattern: string, cwd: string): string {
  const absolute = path.resolve(cwd, pattern);
  const root = path.parse(absolute).root;
  const segments = absolute.slice(root.length).split(path.sep);
  let current = root;
  for (const segment of segments) {
    if (!segment || isDynamicPattern(segment)) break;
    current = path.join(current, segment);
  }
  return current || cwd;
}

async function existsAsDirectory(target: string): Promise<boolean> {
  try {
    return (await fsp.stat(target)).isDirectory();
  } catch {
    return false;
  }
}

async function planScope(
  cwd: string,
  patterns: readonly string[],
): Promise<PlannedScope> {
  if (patterns.length === 0) return { roots: [cwd], files: [] };
  const roots = new Map<string, string>();
  const files: string[] = [];
  for (const pattern of patterns) {
    if (isDynamicPattern(pattern)) {
      const root = staticGlobRoot(pattern, cwd);
      if (await existsAsDirectory(root))
        roots.set(nativePathIdentity.key(root), root);
      continue;
    }
    const absolute = path.resolve(cwd, pattern);
    if (await existsAsDirectory(absolute)) {
      roots.set(nativePathIdentity.key(absolute), absolute);
    } else {
      files.push(absolute); // missing files are still explicit targets
    }
  }
  return { roots: [...roots.values()], files };
}

async function walkConfigLikeFiles(
  roots: readonly string[],
): Promise<{ configs: string[]; ignores: string[] }> {
  const configs: string[] = [];
  const ignores: string[] = [];
  const visit = async (directory: string): Promise<void> => {
    let entries: import('node:fs').Dirent[];
    try {
      entries = await fsp.readdir(directory, { withFileTypes: true });
    } catch {
      return;
    }
    for (const entry of entries) {
      if (PRUNE_DIRECTORY_NAMES.has(entry.name)) continue;
      const target = path.join(directory, entry.name);
      if (entry.isDirectory()) {
        await visit(target);
      } else if (CONFIG_FILE_NAMES.has(entry.name)) {
        configs.push(target);
      } else if (entry.name === IGNORE_FILE_NAME) {
        ignores.push(target);
      }
    }
  };
  for (const root of roots) await visit(root);
  return { configs, ignores };
}

function ancestorDirectories(root: string): string[] {
  const result: string[] = [];
  let current = path.dirname(root);
  while (current !== path.dirname(current)) {
    result.push(current);
    current = path.dirname(current);
  }
  return result;
}

async function drainWritable(stream: NodeJS.WritableStream): Promise<void> {
  await new Promise<void>((resolve) => {
    stream.write('', () => {
      resolve();
    });
  });
}

export async function runWatch(options: RunWatchOptions): Promise<number> {
  const cwd = path.resolve(options.cwd);
  const fix = options.fix ?? false;
  const quiet = options.quiet ?? false;
  const maxWarnings = options.maxWarnings ?? -1;
  const configFile = options.configFile ?? null;
  const ruleOverrides = options.ruleOverrides ?? null;
  const stdout = options.stdout ?? process.stdout;
  const stderr = options.stderr ?? process.stderr;
  const debounceMs = options.debounceMs ?? DEFAULT_DEBOUNCE_MS;
  const pollMs = options.pollMs ?? DEFAULT_POLL_MS;
  const color =
    options.color ??
    resolveColorEnabled({
      tty: 'isTTY' in stdout && stdout.isTTY === true,
    });

  const userPatterns = options.positionalPatterns ?? [];
  const fullPatterns =
    userPatterns.length > 0 ? [...userPatterns] : [DEFAULT_GLOB];
  const scope = await planScope(cwd, userPatterns);

  const write = async (text: string): Promise<void> => {
    if (!stdout.write(text)) await drainWritable(stdout);
  };
  const writeError = (text: string): void => {
    stderr.write(text);
  };

  const renderer = createRenderer(cwd, color);
  await write(renderer.startBanner(debounceMs, pollMs, fullPatterns));

  const linterFactory =
    options.createLinter ??
    (async () => {
      const { Rslint } = await import('../../api/rslint.js');
      // Structurally compatible with WatchLinter (lintFiles + close).
      const instance: WatchLinter = new Rslint({
        cwd,
        fix,
        overrideConfigFile: configFile,
        overrideConfig: ruleOverrides ? { rules: ruleOverrides } : null,
      });
      return instance;
    });
  const linter = await linterFactory({ cwd, fix, configFile, ruleOverrides });

  const store = new DiagnosticStore();

  // ── config / ignore sensitivity ─────────────────────────────────────────
  let sensitiveKeys = new Set<string>();
  const sentinelNames = new Set([...CONFIG_FILE_NAMES, IGNORE_FILE_NAME]);
  const refreshSensitive = async (): Promise<void> => {
    const next = new Set<string>();
    const { configs, ignores } = await walkConfigLikeFiles(scope.roots);
    const seeds = [...configs];
    if (configFile) seeds.push(configFile);
    for (const config of seeds) next.add(nativePathIdentity.key(config));
    for (const ignore of ignores) next.add(nativePathIdentity.key(ignore));
    try {
      for (const dependency of await collectConfigSensitivePaths(seeds)) {
        next.add(nativePathIdentity.key(dependency));
      }
    } catch (error) {
      // Dependency scanning never blocks linting; the primary config names are
      // still watched.
      writeError(
        `rslint: warning: failed to scan config dependencies: ${errorMessage(error)}\n`,
      );
    }
    sensitiveKeys = next;
  };
  await refreshSensitive();
  const ancestorSentinels = ancestorDirectories(scope.roots[0] ?? cwd).map(
    (directory) => ({ directory, names: sentinelNames }),
  );

  // ── round state ──────────────────────────────────────────────────────────
  let round = 0;
  let lastExitCode = 0;
  let stopping = false;
  let chain: Promise<void> = Promise.resolve();
  let queued: PendingAction | null = null;
  /** mtimeMs of files written by our own --fix pass, keyed by path identity. */
  const selfWrites = new Map<string, number>();

  // Resolved when the stop sequence (watcher off, in-flight round finished,
  // resident service closed) completes; runWatch awaits exactly this.
  let resolveStopped: (code: number) => void;
  const stopped = new Promise<number>((resolve) => {
    resolveStopped = resolve;
  });

  const isConfigLike = (target: string): boolean => {
    const base = path.basename(target);
    return (
      base === IGNORE_FILE_NAME ||
      CONFIG_FILE_NAMES.has(base) ||
      sensitiveKeys.has(nativePathIdentity.key(target))
    );
  };

  const isLintable = (target: string): boolean =>
    LINTABLE_EXTENSIONS.has(path.extname(target));

  const exitCodeForTotals = (totals: {
    errors: number;
    warnings: number;
  }): number => {
    if (totals.errors > 0) return 1;
    if (!quiet && maxWarnings >= 0 && totals.warnings > maxWarnings) return 1;
    return 0;
  };

  const snapshotCounts = (): Map<
    string,
    { errors: number; warnings: number }
  > => {
    const counts = new Map<string, { errors: number; warnings: number }>();
    for (const state of store.snapshot().states) {
      counts.set(nativePathIdentity.key(state.filePath), {
        errors: state.errors,
        warnings: state.warnings,
      });
    }
    return counts;
  };

  /**
   * Persist --fix output exactly once per round. Record each written file's
   * post-write mtime so the watcher event it produces is recognized as our
   * own and suppressed. Issues are counted against the pre-round generation.
   */
  const persistFixes = async (
    results: readonly LintResult[],
    before: ReadonlyMap<string, { errors: number; warnings: number }>,
  ): Promise<{ files: number; issues: number }> => {
    let files = 0;
    let issues = 0;
    for (const result of results) {
      if (typeof result.output !== 'string') continue;
      files += 1;
      await fsp.writeFile(result.filePath, result.output);
      const key = nativePathIdentity.key(result.filePath);
      const previous = before.get(key);
      if (previous) {
        issues += Math.max(
          0,
          previous.errors +
            previous.warnings -
            (result.errorCount + result.warningCount),
        );
      }
      try {
        selfWrites.set(key, (await fsp.stat(result.filePath)).mtimeMs);
      } catch {
        // The mtime guard is best effort; the post-fix state stays final.
      }
    }
    return { files, issues };
  };

  const debugLog = process.env.RSLINT_WATCH_DEBUG
    ? (message: string): void => {
        process.stderr.write(`[watch-debug] ${message}\n`);
      }
    : (): void => undefined;

  const executeRound = async (action: PendingAction): Promise<void> => {
    const current = ++round;
    debugLog(
      `round ${current} start ${current === 1 ? 'initial' : action.full ? 'full' : 'files'} reasons=${action.reasons.join('|')} content=${action.content.size} deleted=${action.deleted.size}`,
    );
    const kind: 'initial' | 'full' | 'files' =
      current === 1 ? 'initial' : action.full ? 'full' : 'files';
    const started = Date.now();
    const before = snapshotCounts();
    try {
      let results: LintResult[];
      let delta: RoundDelta;
      const checkedKeys = new Set<string>();
      const notices: string[] = [];
      let fixed: { files: number; issues: number } | undefined;

      if (action.full) {
        results = [...(await linter.lintFiles(fullPatterns))];
        fixed = await persistFixes(results, before);
        delta = store.replaceAll(results);
        for (const result of results) {
          checkedKeys.add(nativePathIdentity.key(result.filePath));
        }
        // The full round may have picked up new config modules/ignore files.
        await refreshSensitive();
      } else {
        const deletedPaths = [...action.deleted.values()];
        for (const deletedPath of deletedPaths) {
          if (store.has(deletedPath)) {
            notices.push(
              `${path.relative(cwd, deletedPath)} removed (deleted)`,
            );
          }
        }
        const deleteDelta = store.deleteFiles(deletedPaths);

        const lintPaths = [...action.content.values()];
        results =
          lintPaths.length > 0 ? [...(await linter.lintFiles(lintPaths))] : [];
        fixed = await persistFixes(results, before);
        const resultKeys = new Set(
          results.map((result) => nativePathIdentity.key(result.filePath)),
        );
        const upsertDelta = store.upsertFiles(results, lintPaths);
        for (const result of results) {
          checkedKeys.add(nativePathIdentity.key(result.filePath));
        }
        // A requested file absent from the response is no longer admitted
        // by Go (newly ignored, excluded, or outside lintable extensions):
        // the upsert just dropped it, so surface that as a notice when it
        // belonged to the previous round.
        for (const lintPath of lintPaths) {
          const key = nativePathIdentity.key(lintPath);
          if (!resultKeys.has(key) && before.has(key) && !store.has(lintPath)) {
            notices.push(
              `${path.relative(cwd, lintPath)} removed (ignored or no longer selected)`,
            );
          }
        }
        delta = {
          filesAdded: deleteDelta.filesAdded + upsertDelta.filesAdded,
          filesRemoved: deleteDelta.filesRemoved + upsertDelta.filesRemoved,
          errorsAdded: deleteDelta.errorsAdded + upsertDelta.errorsAdded,
          errorsRemoved: deleteDelta.errorsRemoved + upsertDelta.errorsRemoved,
          warningsAdded: deleteDelta.warningsAdded + upsertDelta.warningsAdded,
          warningsRemoved:
            deleteDelta.warningsRemoved + upsertDelta.warningsRemoved,
        };
      }

      const generation = store.snapshot();
      const checked = generation.states.filter((state) =>
        checkedKeys.has(nativePathIdentity.key(state.filePath)),
      );
      lastExitCode = exitCodeForTotals(generation.totals);
      await write(
        renderer.round({
          round: current,
          kind,
          reasons: action.reasons,
          checked,
          delta,
          totals: generation.totals,
          quiet,
          fixed,
          notices,
          durationMs: Date.now() - started,
        }),
      );
      debugLog(
        `round ${current} done results=${results.length} errors=${generation.totals.errors}`,
      );
      options.onRound?.({
        round: current,
        kind,
        results,
        totals: generation.totals,
        exitCode: lastExitCode,
      });
    } catch (error) {
      debugLog(`round ${current} ERROR ${errorMessage(error)}`);
      // Keep the last-good generation visible; a failed round is exit 1.
      lastExitCode = 1;
      const totals = store.totals;
      await write(
        renderer.failedRound({
          round: current,
          kind,
          reasons: action.reasons,
          error: errorMessage(error),
          totals,
        }),
      );
      options.onRound?.({
        round: current,
        kind,
        results: [],
        totals,
        exitCode: lastExitCode,
        error: errorMessage(error),
      });
    }
  };

  const pump = async (): Promise<void> => {
    while (!stopping && queued) {
      const action = queued;
      queued = null;
      await executeRound(action);
    }
  };

  const schedule = (action: PendingAction): void => {
    if (stopping) return;
    if (!queued) {
      queued = action;
    } else if (action.full) {
      queued = {
        full: true,
        content: new Map([...queued.content, ...action.content]),
        deleted: new Map([...queued.deleted, ...action.deleted]),
        reasons: [...queued.reasons, ...action.reasons],
      };
    } else {
      for (const [key, value] of action.content) queued.content.set(key, value);
      for (const [key, value] of action.deleted) queued.deleted.set(key, value);
      queued.reasons.push(...action.reasons);
    }
    chain = chain.then(pump, pump);
  };

  // ── event classification ─────────────────────────────────────────────────
  const classifyBatch = async (events: WatchEvent[]): Promise<void> => {
    debugLog(
      `batch n=${events.length}: ${events.map((event) => `${event.type}:${path.relative(cwd, event.path)}`).join(',')}`,
    );
    const action: PendingAction = {
      full: false,
      content: new Map(),
      deleted: new Map(),
      reasons: [],
    };
    const reasonKeys = new Set<string>();
    const addReason = (key: string, label: string): void => {
      if (reasonKeys.has(key)) return;
      reasonKeys.add(key);
      action.reasons.push(label);
    };
    for (const event of events) {
      const target = path.resolve(event.path);
      const key = nativePathIdentity.key(target);
      const label = path.relative(cwd, target) || target;
      // Suppress events from our own --fix writes. A later external edit
      // changes mtime and is allowed through (and clears the guard).
      const selfMtime = selfWrites.get(key);
      if (selfMtime !== undefined && event.type !== 'delete') {
        try {
          const stats = await fsp.stat(target);
          if (stats.mtimeMs === selfMtime) continue;
        } catch {
          // File vanished after our write; fall through to normal handling.
        }
        selfWrites.delete(key);
      }
      if (isConfigLike(target)) {
        action.full = true;
        addReason(
          key,
          `${label} ${event.type === 'update' ? 'changed' : event.type + 'd'}`,
        );
        continue;
      }
      if (!isLintable(target)) continue;
      if (event.type === 'delete') {
        action.deleted.set(key, target);
        addReason(key, `${label} deleted`);
      } else {
        action.content.set(key, target);
        addReason(
          key,
          `${label} ${event.type === 'create' ? 'created' : 'changed'}`,
        );
      }
    }
    if (
      !action.full &&
      action.content.size === 0 &&
      action.deleted.size === 0
    ) {
      return;
    }
    schedule(action);
  };

  const watcherFactory = options.createWatcher ?? createFileWatcher;
  const watcher: FileWatcher = await watcherFactory({
    roots: scope.roots,
    files: scope.files,
    sentinels: ancestorSentinels,
    pruneDirectoryNames: PRUNE_DIRECTORY_NAMES,
    debounceMs,
    pollMs,
    onEvents: (events) => {
      void classifyBatch(events);
    },
    onNotice: (message) => {
      writeError(`rslint: ${message}\n`);
    },
  });

  // ── signals / shutdown ─────────────────────────────────────────────────
  let signalCount = 0;
  let cleanupStarted = false;
  const requestStop = (): void => {
    signalCount += 1;
    if (signalCount > 1) {
      // Repeated interrupt: leave immediately with the latest round's code.
      process.exit(lastExitCode);
    }
    if (cleanupStarted) return;
    cleanupStarted = true;
    debugLog('stop requested');
    stopping = true;
    queued = null; // never-started work is discarded; in-flight work finishes
    watcher.close();
    void chain
      .catch(() => undefined)
      .then(async () => {
        await linter.close().catch((error) => {
          writeError(
            `rslint: warning: error stopping lint service: ${errorMessage(error)}\n`,
          );
        });
        await write(renderer.stop(lastExitCode));
        unsubscribeSignals();
        resolveStopped(lastExitCode);
      });
  };
  let unsubscribeSignals = (options.signals ?? defaultSignals).onSignal(() => {
    requestStop();
  });
  if (
    options.handleStdinEnd ??
    (process.stdin as Partial<NodeJS.ReadStream>).isTTY === false
  ) {
    const onStdinEnd = (): void => {
      requestStop();
    };
    process.stdin.once('end', onStdinEnd);
    const previousUnsubscribe = unsubscribeSignals;
    unsubscribeSignals = () => {
      previousUnsubscribe();
      process.stdin.off('end', onStdinEnd);
    };
  }

  // Round 1: the initial full check runs immediately.
  schedule({
    full: true,
    content: new Map(),
    deleted: new Map(),
    reasons: [],
  });

  return stopped;
}
