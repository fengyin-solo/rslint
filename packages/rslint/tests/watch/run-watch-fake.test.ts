import { describe, expect, test } from 'rstack/test';
import { mkdtemp, rm, stat, utimes, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';

import { runWatch } from '../../src/cli/watch/run-watch.js';
import type {
  FileWatcherOptions,
  WatchEvent,
} from '../../src/cli/watch/watcher.js';
import type {
  RoundInfo,
  RunWatchOptions,
  SignalHarness,
  WatchLinter,
} from '../../src/cli/watch/index.js';
import type { LintResult } from '../../src/api/rslint.js';

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

function waitForRound(infos: RoundInfo[], round: number): Promise<RoundInfo> {
  return new Promise((resolve, reject) => {
    const deadline = Date.now() + 4000;
    const tick = (): void => {
      const found = infos.find((info) => info.round === round);
      if (found) {
        resolve(found);
        return;
      }
      if (Date.now() > deadline) {
        reject(new Error(`timed out waiting for round ${round}`));
        return;
      }
      setTimeout(tick, 10);
    };
    tick();
  });
}

async function assertNoRound(infos: RoundInfo[], count: number): Promise<void> {
  await sleep(150);
  expect(infos).toHaveLength(count);
}

interface FakeFile {
  /** Content; 'console.log(' yields a no-console error. */
  code: string;
  /** Pretend Go refuses to admit this path (ignored). */
  excluded?: boolean;
}

interface FakeLinter extends WatchLinter {
  calls: Array<string | string[]>;
  closed: boolean;
  failNext: boolean;
}

function makeLinter(
  cwd: string,
  files: Map<string, FakeFile>,
  fix: boolean,
): FakeLinter {
  const linter: FakeLinter = {
    calls: [],
    closed: false,
    failNext: false,
    async lintFiles(patterns: string | string[]) {
      linter.calls.push(patterns);
      if (linter.failNext) {
        linter.failNext = false;
        throw new Error('boom: config failed to load');
      }
      const list = Array.isArray(patterns) ? patterns : [patterns];
      const results: LintResult[] = [];
      for (const pattern of list) {
        const absolute = path.isAbsolute(pattern)
          ? path.normalize(pattern)
          : path.resolve(cwd, pattern);
        // Full-scope round: the fake enumerates every known file. Keys may be
        // absolute or relative to the session cwd.
        const targets = pattern.includes('*')
          ? [...files.keys()].map((key) =>
              path.isAbsolute(key) ? key : path.resolve(cwd, key),
            )
          : [absolute];
        for (const target of targets) {
          const file =
            files.get(target) ?? files.get(path.relative(cwd, target));
          if (!file || file.excluded) continue;
          // Like the real API, a fix round reports the POST-fix state: when a
          // bounded fix exists, output carries the final source and messages
          // describe the remaining (clean) source.
          const fixedOutput =
            fix && file.code.includes(';;')
              ? file.code.replace(';;', ';')
              : undefined;
          const observedCode = fixedOutput ?? file.code;
          const messages = [];
          if (observedCode.includes('console.log(')) {
            messages.push({
              ruleId: 'no-console',
              severity: 2 as const,
              message: 'Unexpected console statement',
              line: 1,
              column: 1,
            });
          }
          if (fixedOutput === undefined && file.code.includes(';;')) {
            messages.push({
              ruleId: 'no-extra-semi',
              severity: 2 as const,
              message: 'Unnecessary semicolon',
              line: 1,
              column: 1,
            });
          }
          const result: LintResult = {
            filePath: target,
            messages,
            errorCount: messages.length,
            warningCount: 0,
            fixableErrorCount: fix ? messages.length : 0,
            fixableWarningCount: 0,
          };
          if (fixedOutput !== undefined) result.output = fixedOutput;
          results.push(result);
        }
      }
      return results;
    },
    async close() {
      linter.closed = true;
    },
  };
  return linter;
}

interface ScriptedSignals extends SignalHarness {
  fire(signalName: string): void;
  unsubscribed: boolean;
}

function makeSignals(): ScriptedSignals {
  let listener: ((name: string) => void) | null = null;
  const signals: ScriptedSignals = {
    unsubscribed: false,
    fire(signalName) {
      listener?.(signalName);
    },
    onSignal(received) {
      listener = received;
      return () => {
        listener = null;
        signals.unsubscribed = true;
      };
    },
  };
  return signals;
}

function scriptedWatcher(): {
  factory: NonNullable<RunWatchOptions['createWatcher']>;
  emit(events: WatchEvent[]): void;
  closeCount: number;
} {
  let emitEvents: ((events: WatchEvent[]) => void) | null = null;
  const handle = {
    closeCount: 0,
    emit(events: WatchEvent[]) {
      emitEvents?.(events);
    },
    factory: async (options: FileWatcherOptions) => {
      emitEvents = options.onEvents;
      return {
        close() {
          handle.closeCount += 1;
        },
      };
    },
  };
  return handle;
}

function stringStream(): {
  out: { text: string };
  stream: NodeJS.WritableStream;
} {
  const out: { text: string } = { text: '' };
  // rslint-disable-next-line @typescript-eslint/no-unsafe-type-assertion
  const stream = {
    write: (chunk: string) => {
      out.text += chunk;
      return true;
    },
  } as unknown as NodeJS.WritableStream;
  return { out, stream };
}

interface Session {
  cwd: string;
  code: Promise<number>;
  infos: RoundInfo[];
  linter: FakeLinter;
  signals: ScriptedSignals;
  watcher: ReturnType<typeof scriptedWatcher>;
  out: { text: string };
}

async function startSession(
  files: Map<string, FakeFile>,
  fix = false,
): Promise<Session> {
  const cwd = await mkdtemp(path.join(os.tmpdir(), 'rslint-watch-fake-'));
  const infos: RoundInfo[] = [];
  const signals = makeSignals();
  const watcher = scriptedWatcher();
  const linter = makeLinter(cwd, files, fix);
  const { out, stream } = stringStream();
  const code = runWatch({
    cwd,
    fix,
    color: false,
    debounceMs: 5,
    pollMs: 10_000,
    handleStdinEnd: false,
    signals,
    createLinter: () => linter,
    createWatcher: watcher.factory,
    onRound: (info) => infos.push(info),
    stdout: stream,
    stderr: stream,
  });
  return { cwd, code, infos, linter, signals, watcher, out };
}

describe('runWatch orchestration (fake linter)', () => {
  test('initial, scoped, delete, config-full rounds; stop returns latest code', async () => {
    const cwd = await mkdtemp(path.join(os.tmpdir(), 'rslint-watch-flow-'));
    const a = path.join(cwd, 'a.ts');
    const b = path.join(cwd, 'b.ts');
    const config = path.join(cwd, 'rslint.config.mjs');
    const files = new Map<string, FakeFile>([
      [a, { code: 'console.log(1);\n' }],
      [b, { code: 'const b = 2;\n' }],
    ]);
    const session = await startSession(files);

    try {
      const first = await waitForRound(session.infos, 1);
      expect(first.kind).toBe('initial');
      expect(first.totals.errors).toBe(1);
      expect(first.exitCode).toBe(1);
      expect(String(session.linter.calls[0])).toContain('**/*');

      // Content change: scoped round re-lints ONLY the changed file.
      files.set(a, { code: 'const a = 1;\n' });
      session.watcher.emit([{ type: 'update', path: a }]);
      const second = await waitForRound(session.infos, 2);
      expect(second.kind).toBe('files');
      expect(second.totals.errors).toBe(0);
      expect(second.exitCode).toBe(0);
      expect(session.linter.calls[1]).toEqual([a]);

      // Deletion removes the file without issuing a lint request.
      files.delete(a);
      session.watcher.emit([{ type: 'delete', path: a }]);
      const third = await waitForRound(session.infos, 3);
      expect(third.totals.files).toBe(1);
      expect(session.linter.calls).toHaveLength(2);

      // Config change forces a full re-check. The delete round issued no lint
      // request, so this is the third linter call.
      session.watcher.emit([{ type: 'update', path: config }]);
      const fourth = await waitForRound(session.infos, 4);
      expect(fourth.kind).toBe('full');
      expect(session.linter.calls).toHaveLength(3);
      expect(String(session.linter.calls[2])).toContain('**/*');

      // A non-lintable change schedules nothing.
      session.watcher.emit([
        { type: 'update', path: path.join(cwd, 'notes.md') },
      ]);
      await assertNoRound(session.infos, 4);

      session.signals.fire('SIGINT');
      expect(await session.code).toBe(0);
      expect(session.linter.closed).toBe(true);
      expect(session.watcher.closeCount).toBe(1);
      expect(session.signals.unsubscribed).toBe(true);
    } finally {
      await rm(cwd, { recursive: true, force: true });
    }
  });

  test('a failed round keeps the last-good results and fails the exit code', async () => {
    const cwd = await mkdtemp(path.join(os.tmpdir(), 'rslint-watch-fail-'));
    const a = path.join(cwd, 'a.ts');
    const files = new Map<string, FakeFile>([
      [a, { code: 'console.log(1);\n' }],
    ]);
    const infos: RoundInfo[] = [];
    const signals = makeSignals();
    const watcher = scriptedWatcher();
    const linter = makeLinter(cwd, files, false);
    const { out, stream } = stringStream();
    const code = runWatch({
      cwd,
      color: false,
      debounceMs: 5,
      pollMs: 10_000,
      handleStdinEnd: false,
      signals,
      createLinter: () => linter,
      createWatcher: watcher.factory,
      onRound: (info) => infos.push(info),
      stdout: stream,
      stderr: stream,
    });

    try {
      await waitForRound(infos, 1);
      linter.failNext = true;
      watcher.emit([{ type: 'update', path: a }]);
      const failed = await waitForRound(infos, 2);
      expect(failed.error).toContain('boom');
      // The previous generation stays on screen and counted.
      expect(failed.totals.errors).toBe(1);
      expect(failed.exitCode).toBe(1);
      expect(out.text).toContain('keeping the previous round');

      // Recovery: a later change lints successfully again.
      files.set(a, { code: 'const a = 1;\n' });
      watcher.emit([{ type: 'update', path: a }]);
      const third = await waitForRound(infos, 3);
      expect(third.error).toBeUndefined();
      expect(third.totals.errors).toBe(0);

      signals.fire('SIGTERM');
      expect(await code).toBe(0);
    } finally {
      await rm(cwd, { recursive: true, force: true });
    }
  });

  test('a newly excluded file disappears from results', async () => {
    const cwd = await mkdtemp(path.join(os.tmpdir(), 'rslint-watch-excl-'));
    const a = path.join(cwd, 'a.ts');
    const files = new Map<string, FakeFile>([
      [a, { code: 'console.log(1);\n' }],
    ]);
    const infos: RoundInfo[] = [];
    const signals = makeSignals();
    const watcher = scriptedWatcher();
    const linter = makeLinter(cwd, files, false);
    const { out, stream } = stringStream();
    const code = runWatch({
      cwd,
      color: false,
      debounceMs: 5,
      pollMs: 10_000,
      handleStdinEnd: false,
      signals,
      createLinter: () => linter,
      createWatcher: watcher.factory,
      onRound: (info) => infos.push(info),
      stdout: stream,
      stderr: stream,
    });
    try {
      await waitForRound(infos, 1);
      // An ignore/config change: Go no longer admits the file.
      files.set(a, { code: 'console.log(1);\n', excluded: true });
      watcher.emit([{ type: 'update', path: a }]);
      const second = await waitForRound(infos, 2);
      expect(second.totals.files).toBe(0);
      expect(second.totals.errors).toBe(0);
      expect(out.text).toContain('ignored or no longer selected');
      signals.fire('SIGINT');
      expect(await code).toBe(0);
    } finally {
      await rm(cwd, { recursive: true, force: true });
    }
  });

  test('stop while problems remain returns exit code 1', async () => {
    const session = await startSession(
      new Map<string, FakeFile>([['a.ts', { code: 'console.log(1);\n' }]]),
    );
    const first = await waitForRound(session.infos, 1);
    expect(first.exitCode).toBe(1);
    session.signals.fire('SIGHUP');
    expect(await session.code).toBe(1);
  });

  test('--fix writes once and cannot schedule a follow-up round for its own write', async () => {
    const cwd = await mkdtemp(path.join(os.tmpdir(), 'rslint-watch-fix-'));
    const a = path.join(cwd, 'a.ts');
    await writeFile(a, 'const a = 1;;\n');
    const files = new Map<string, FakeFile>([[a, { code: 'const a = 1;;\n' }]]);
    const infos: RoundInfo[] = [];
    const signals = makeSignals();
    const watcher = scriptedWatcher();
    const linter = makeLinter(cwd, files, true);
    const { out, stream } = stringStream();
    const code = runWatch({
      cwd,
      fix: true,
      color: false,
      debounceMs: 5,
      pollMs: 10_000,
      handleStdinEnd: false,
      signals,
      createLinter: () => linter,
      createWatcher: watcher.factory,
      onRound: (info) => infos.push(info),
      stdout: stream,
      stderr: stream,
    });
    try {
      const first = await waitForRound(infos, 1);
      expect(first.totals.errors).toBe(0);
      expect(out.text).toContain('applied fixes to 1 file');
      // An event reporting the fixed file at its on-disk mtime is self-made.
      watcher.emit([{ type: 'update', path: a }]);
      await assertNoRound(infos, 1);
      expect(linter.calls).toHaveLength(1);
      // An external edit (later mtime) is allowed through.
      const mtime = (await stat(a)).mtimeMs;
      const later = new Date(mtime + 5000);
      await writeFile(a, 'const a = 2;\n');
      await utimes(a, later, later);
      files.set(a, { code: 'const a = 2;\n' });
      watcher.emit([{ type: 'update', path: a }]);
      const second = await waitForRound(infos, 2);
      expect(second.kind).toBe('files');
      expect(linter.calls).toHaveLength(2);
      signals.fire('SIGINT');
      expect(await code).toBe(0);
    } finally {
      await rm(cwd, { recursive: true, force: true });
    }
  });
});
