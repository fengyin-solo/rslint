import { describe, expect, test } from 'rstack/test';
import path from 'node:path';

import {
  createRenderer,
  resolveColorEnabled,
  type RoundRenderInput,
} from '../../src/cli/watch/render.js';
import type {
  FileState,
  RoundDelta,
  StoreTotals,
} from '../../src/cli/watch/store.js';

const cwd = '/project';

function state(
  relativePath: string,
  messages: FileState['messages'],
): FileState {
  let errors = 0;
  let warnings = 0;
  for (const message of messages) {
    if (message.severity === 2) errors += 1;
    else warnings += 1;
  }
  return {
    filePath: path.resolve(cwd, relativePath),
    errors,
    warnings,
    messages,
  };
}

function totals(
  files: number,
  filesWithProblems: number,
  errors: number,
  warnings: number,
): StoreTotals {
  return { files, filesWithProblems, errors, warnings };
}

const emptyDelta: RoundDelta = {
  filesAdded: 0,
  filesRemoved: 0,
  errorsAdded: 0,
  errorsRemoved: 0,
  warningsAdded: 0,
  warningsRemoved: 0,
};

function roundInput(
  overrides: Partial<RoundRenderInput> = {},
): RoundRenderInput {
  return {
    round: 2,
    kind: 'files',
    reasons: ['src/a.ts changed'],
    checked: [],
    delta: emptyDelta,
    totals: totals(1, 0, 0, 0),
    quiet: false,
    durationMs: 12,
    ...overrides,
  };
}

describe('watch renderer', () => {
  test('never emits clear-screen, cursor-hide, or carriage-return escapes', () => {
    const renderer = createRenderer(cwd, false);
    const output = [
      renderer.startBanner(100, 1000, ['**/*.ts']),
      renderer.round(
        roundInput({
          checked: [
            state('src/a.ts', [
              {
                ruleId: 'no-console',
                severity: 2,
                message: 'no',
                line: 3,
                column: 5,
              },
            ]),
          ],
        }),
      ),
      renderer.failedRound({
        round: 3,
        kind: 'full',
        reasons: ['rslint.config.mjs changed'],
        error: 'boom',
        totals: totals(1, 1, 1, 0),
      }),
      renderer.stop(1),
    ].join('');
    expect(output).not.toContain('\x1b[2J');
    expect(output).not.toContain('\x1b[H');
    expect(output).not.toContain('\x1b[?25');
    expect(output).not.toContain('\x1b[K');
    expect(output).not.toContain('\r');
  });

  test('round header separates this round from earlier output', () => {
    const renderer = createRenderer(cwd, false);
    const output = renderer.round(
      roundInput({
        round: 4,
        reasons: ['src/a.ts changed', 'src/b.ts created'],
      }),
    );
    expect(output).toContain('[round 4 · re-check ·');
    expect(output).toContain('src/a.ts changed, src/b.ts created');
  });

  test('diagnostics use compact file:line:col rows with rule id', () => {
    const renderer = createRenderer(cwd, false);
    const output = renderer.round(
      roundInput({
        checked: [
          state('src/a.ts', [
            {
              ruleId: 'no-console',
              severity: 2,
              message: 'Unexpected console',
              line: 3,
              column: 5,
              endLine: 3,
              endColumn: 12,
            },
            {
              ruleId: 'eqeqeq',
              severity: 1,
              message: 'Use ===',
              line: 9,
              column: 1,
            },
          ]),
        ],
        totals: totals(2, 1, 1, 1),
      }),
    );
    expect(output).toMatch(
      /src\/a\.ts:3:5\s+error\s+Unexpected console\s+no-console/,
    );
    expect(output).toMatch(/src\/a\.ts:9:1\s+warning\s+Use ===\s+eqeqeq/);
    expect(output).toContain('1 error, 1 warning remaining');
    expect(output).toContain('of 2 files');
  });

  test('cumulative footer always reports the remaining total', () => {
    const renderer = createRenderer(cwd, false);
    const clean = renderer.round(roundInput());
    expect(clean).toContain('no problems remaining');
    expect(clean).toContain('1 file watched-scope results kept');

    const failing = renderer.round(
      roundInput({
        totals: totals(10, 3, 4, 2),
        delta: { ...emptyDelta, errorsRemoved: 1 },
      }),
    );
    expect(failing).toContain('4 errors, 2 warnings remaining');
    expect(failing).toContain('across 3 of 10 files');
    expect(failing).toContain('this round:');
    expect(failing).toContain('−1 error');
  });

  test('--quiet hides warning rows and warning totals', () => {
    const renderer = createRenderer(cwd, false);
    const output = renderer.round(
      roundInput({
        quiet: true,
        checked: [
          state('src/a.ts', [
            {
              ruleId: 'eqeqeq',
              severity: 1,
              message: 'Use ===',
              line: 1,
              column: 1,
            },
          ]),
        ],
        totals: totals(1, 1, 0, 1),
      }),
    );
    expect(output).not.toContain('eqeqeq');
    expect(output).toContain('no problems remaining');
  });

  test('fix result and removal notices render distinctly', () => {
    const renderer = createRenderer(cwd, false);
    const output = renderer.round(
      roundInput({
        fixed: { files: 2, issues: 3 },
        notices: ['src/old.ts removed (deleted)'],
        totals: totals(2, 0, 0, 0),
      }),
    );
    expect(output).toContain('applied 3 fixes to 2 files');
    expect(output).toContain('src/old.ts removed (deleted)');
  });

  test('failed rounds render the error and keep the previous totals visible', () => {
    const renderer = createRenderer(cwd, false);
    const output = renderer.failedRound({
      round: 3,
      kind: 'full',
      reasons: ['rslint.config.mjs changed'],
      error: 'Invalid config: boom',
      totals: totals(4, 2, 5, 0),
    });
    expect(output).toContain('Invalid config: boom');
    expect(output).toContain('keeping the previous round’s results');
    expect(output).toContain('5 errors remaining');
  });

  test('stop banner reports the latest round code', () => {
    const renderer = createRenderer(cwd, false);
    expect(renderer.stop(0)).toContain('clean');
    expect(renderer.stop(1)).toContain('exit 1');
  });

  test('resolveColorEnabled honors NO_COLOR, FORCE_COLOR, and TTY', () => {
    const previousNoColor = process.env.NO_COLOR;
    const previousForceColor = process.env.FORCE_COLOR;
    try {
      delete process.env.NO_COLOR;
      delete process.env.FORCE_COLOR;
      expect(resolveColorEnabled({ tty: true })).toBe(true);
      expect(resolveColorEnabled({ tty: false })).toBe(false);
      process.env.NO_COLOR = '1';
      expect(resolveColorEnabled({ tty: true, forceColor: false })).toBe(false);
      delete process.env.NO_COLOR;
      process.env.FORCE_COLOR = '1';
      expect(resolveColorEnabled({ tty: false })).toBe(true);
      // Explicit flags win over env.
      expect(resolveColorEnabled({ tty: true, noColor: true })).toBe(false);
    } finally {
      if (previousNoColor === undefined) delete process.env.NO_COLOR;
      else process.env.NO_COLOR = previousNoColor;
      if (previousForceColor === undefined) delete process.env.FORCE_COLOR;
      else process.env.FORCE_COLOR = previousForceColor;
    }
  });

  test('color output contains SGR codes and plain output does not', () => {
    const colored = createRenderer(cwd, true).stop(0);
    expect(colored).toContain('\x1b[');
    const plain = createRenderer(cwd, false).stop(0);
    expect(plain).not.toContain('\x1b[');
  });
});
