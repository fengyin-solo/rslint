/**
 * End-to-end watch tests against the REAL persistent --api service (spawned by
 * the default Rslint factory) and the REAL filesystem watcher.
 *
 * Requires the native binary, like the other spawn-based suites: run
 * `pnpm --filter @rslint/core build:bin` first (the package `build` script
 * does this).
 */
import { describe, expect, test } from 'rstack/test';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';

import { runWatch } from '../../src/cli/watch/run-watch.js';
import type { RoundInfo } from '../../src/cli/watch/index.js';

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

const ROUND_TIMEOUT_MS = 30_000;

function roundWaiter(infos: RoundInfo[]) {
  return (round: number): Promise<RoundInfo> =>
    new Promise((resolve, reject) => {
      const deadline = Date.now() + ROUND_TIMEOUT_MS;
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
        setTimeout(tick, 25);
      };
      tick();
    });
}

function makeSignals() {
  let listener: ((name: string) => void) | null = null;
  return {
    fire: () => listener?.('SIGINT'),
    harness: {
      onSignal(received: (name: string) => void) {
        listener = received;
        return () => {
          listener = null;
        };
      },
    },
  };
}

function outputCapture() {
  const chunks: string[] = [];
  // rslint-disable-next-line @typescript-eslint/no-unsafe-type-assertion
  const stream = {
    write: (chunk: string) => {
      chunks.push(chunk);
      return true;
    },
  } as unknown as NodeJS.WritableStream;
  return {
    stream,
    text: () => chunks.join(''),
  };
}

const configWith = (rules: Record<string, string>): string =>
  `export default ${JSON.stringify([{ rules }])};\n`;

interface Project {
  dir: string;
  code: Promise<number>;
  infos: RoundInfo[];
  signals: ReturnType<typeof makeSignals>;
  output: ReturnType<typeof outputCapture>;
}

async function startProject(dir: string): Promise<Project> {
  const infos: RoundInfo[] = [];
  const signals = makeSignals();
  const output = outputCapture();
  const code = runWatch({
    cwd: dir,
    color: false,
    debounceMs: 40,
    pollMs: 150,
    handleStdinEnd: false,
    signals: signals.harness,
    stdout: output.stream,
    stderr: output.stream,
    onRound: (info) => infos.push(info),
  });
  return { dir, code, infos, signals, output };
}

describe('runWatch end-to-end', () => {
  test('single-file change, config change, deletion, and .gitignore change', async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'rslint-watch-e2e-'));
    const configPath = path.join(dir, 'rslint.config.mjs');
    const a = path.join(dir, 'a.js');
    const b = path.join(dir, 'b.js');
    await writeFile(configPath, configWith({ 'no-console': 'error' }));
    await writeFile(a, "console.log('x');\n");
    await writeFile(b, 'const b = 2;\n');

    const project = await startProject(dir);
    const waitFor = roundWaiter(project.infos);

    try {
      // 1. Initial full check finds the console error.
      const r1 = await waitFor(1);
      expect(r1.kind).toBe('initial');
      expect(r1.totals.errors).toBe(1);
      expect(project.output.text()).toContain('no-console');
      expect(project.output.text()).toContain('1 error remaining');
      expect(project.output.text()).not.toContain('\x1b[2J');

      // 2. Single-file modification: only that file is rechecked; the stale
      //    diagnostic disappears and nothing remains.
      await writeFile(a, 'const a = 1;\n');
      const r2 = await waitFor(2);
      expect(r2.kind).toBe('files');
      expect(r2.results.some((result) => result.filePath === a)).toBe(true);
      expect(r2.totals.errors).toBe(0);
      expect(project.output.text()).toContain('no problems remaining');

      // 3. New file is admitted on creation.
      const c = path.join(dir, 'c.js');
      await writeFile(c, "console.log('c');\n");
      const r3 = await waitFor(3);
      expect(r3.totals.errors).toBe(1);
      expect(r3.results.some((result) => result.filePath === c)).toBe(true);

      // 4. Deletion removes the file and its diagnostics from the results.
      await rm(c);
      const r4 = await waitFor(4);
      expect(r4.totals.errors).toBe(0);
      expect(project.output.text()).toContain('removed (deleted)');

      // 5. Reintroduce an error, then change the CONFIG to turn the rule off:
      //    that must trigger a full re-check over the whole scope.
      await writeFile(a, "console.log('again');\n");
      const r5 = await waitFor(5);
      expect(r5.totals.errors).toBe(1);
      await writeFile(configPath, configWith({ 'no-console': 'off' }));
      const r6 = await waitFor(6);
      expect(r6.kind).toBe('full');
      expect(r6.totals.errors).toBe(0);

      // 6. A new .gitignore is a config-scope event (full re-check).
      await writeFile(path.join(dir, '.gitignore'), 'ignored.js\n');
      await waitFor(7);

      // 7. A newly ignored file is rechecked but never enters the results.
      const ignored = path.join(dir, 'ignored.js');
      await writeFile(ignored, "console.log('ignored');\n");
      await waitFor(8);
      expect(project.infos[7].totals.errors).toBe(0);
      expect(project.output.text()).not.toContain('ignored.js:');

      // 8. Re-enable the rule (full round picks up the console again)...
      await writeFile(configPath, configWith({ 'no-console': 'error' }));
      const r9 = await waitFor(9);
      expect(r9.totals.errors).toBe(1);
      // ...then swap the ignore to the offending file: a.js is dropped while
      // the previously ignored (now clean) file is admitted. Both writes land
      // in one debounced batch and produce a single full round.
      await writeFile(ignored, 'const i = 1;\n');
      await writeFile(path.join(dir, '.gitignore'), 'a.js\n');
      const r10 = await waitFor(10);
      expect(r10.kind).toBe('full');
      expect(r10.results.some((result) => result.filePath === a)).toBe(false);
      expect(r10.totals.errors).toBe(0);

      // Every round is visibly delimited in the append-only output.
      expect(project.output.text()).toContain('[round 1 ·');
      expect(project.output.text()).toContain('[round 10 ·');

      // Signal exit returns the latest round's result (clean here).
      project.signals.fire();
      expect(await project.code).toBe(0);
    } finally {
      // Always release the resident service so the worker process can exit.
      project.signals.fire();
      await project.code.catch(() => undefined);
      await rm(dir, { recursive: true, force: true });
    }
  });

  test('signal exit while problems remain reports exit code 1', async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'rslint-watch-exit-'));
    await writeFile(
      path.join(dir, 'rslint.config.mjs'),
      configWith({ 'no-console': 'error' }),
    );
    await writeFile(path.join(dir, 'a.js'), "console.log('x');\n");
    const project = await startProject(dir);
    try {
      const first = await roundWaiter(project.infos)(1);
      expect(first.exitCode).toBe(1);
      project.signals.fire();
      expect(await project.code).toBe(1);
    } finally {
      project.signals.fire();
      await project.code.catch(() => undefined);
      await rm(dir, { recursive: true, force: true });
    }
  });

  test('--watch --fix applies fixes once and never loops', async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'rslint-watch-fix-e2e-'));
    const a = path.join(dir, 'a.js');
    await writeFile(
      path.join(dir, 'rslint.config.mjs'),
      configWith({ 'no-var': 'error' }),
    );
    await writeFile(a, 'var x = 1;\n');

    const infos: RoundInfo[] = [];
    const signals = makeSignals();
    const output = outputCapture();
    const code = runWatch({
      cwd: dir,
      fix: true,
      color: false,
      debounceMs: 40,
      pollMs: 150,
      handleStdinEnd: false,
      signals: signals.harness,
      stdout: output.stream,
      stderr: output.stream,
      onRound: (info) => infos.push(info),
    });

    try {
      const first = await roundWaiter(infos)(1);
      expect(first.totals.errors).toBe(0);
      expect(output.text()).toContain('applied fixes to 1 file');
      expect(await readFile(a, 'utf8')).toBe('let x = 1;\n');

      // Wait through several debounce + safety-poll windows: the self-written
      // file must not trigger another round, and the content stays stable.
      await sleep(1200);
      expect(infos).toHaveLength(1);
      expect(await readFile(a, 'utf8')).toBe('let x = 1;\n');
      expect(output.text()).toContain('[round 1 ·');

      signals.fire();
      expect(await code).toBe(0);
    } finally {
      signals.fire();
      await code.catch(() => undefined);
      await rm(dir, { recursive: true, force: true });
    }
  });
});
