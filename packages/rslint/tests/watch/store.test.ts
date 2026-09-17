import { describe, expect, test } from 'rstack/test';
import path from 'node:path';

import { DiagnosticStore } from '../../src/cli/watch/store.js';
import type { LintMessage, LintResult } from '../../src/api/rslint.js';

function message(ruleId: string, severity: 1 | 2): LintMessage {
  return {
    ruleId,
    severity,
    message: `${ruleId} message`,
    line: 1,
    column: 1,
  };
}

function result(
  filePath: string,
  messages: Array<[string, 1 | 2]> = [],
  cwd = '/project',
): LintResult {
  const absolute = path.isAbsolute(filePath)
    ? path.normalize(filePath)
    : path.resolve(cwd, filePath);
  let errorCount = 0;
  let warningCount = 0;
  const list = messages.map(([ruleId, severity]) => {
    if (severity === 2) errorCount += 1;
    else warningCount += 1;
    return message(ruleId, severity);
  });
  return {
    filePath: absolute,
    messages: list,
    errorCount,
    warningCount,
    fixableErrorCount: 0,
    fixableWarningCount: 0,
  };
}

describe('watch DiagnosticStore', () => {
  test('replaceAll seeds the first generation and computes totals', () => {
    const store = new DiagnosticStore();
    const delta = store.replaceAll([
      result('src/a.ts', [['no-console', 2]]),
      result('src/b.ts', [['eqeqeq', 1]]),
      result('src/c.ts'),
    ]);
    expect(delta.filesAdded).toBe(3);
    const totals = store.totals;
    expect(totals.files).toBe(3);
    expect(totals.filesWithProblems).toBe(2);
    expect(totals.errors).toBe(1);
    expect(totals.warnings).toBe(1);
  });

  test('a scoped upsert replaces the file and leaves others untouched', () => {
    const store = new DiagnosticStore();
    store.replaceAll([
      result('src/a.ts', [['no-console', 2]]),
      result('src/b.ts', [['eqeqeq', 1]]),
    ]);
    const delta = store.upsertFiles(
      [
        result('src/a.ts', [
          ['no-console', 2],
          ['eqeqeq', 2],
        ]),
      ],
      [path.resolve('/project/src/a.ts')],
    );
    expect(delta.errorsAdded).toBe(1);
    expect(delta.errorsRemoved).toBe(0);
    expect(store.totals.errors).toBe(2);
    // The untouched file keeps its warning.
    expect(store.totals.warnings).toBe(1);
  });

  test('fixing all messages in a file reports removed diagnostics', () => {
    const store = new DiagnosticStore();
    store.replaceAll([result('src/a.ts', [['no-console', 2]])]);
    const delta = store.upsertFiles(
      [result('src/a.ts')],
      [path.resolve('/project/src/a.ts')],
    );
    expect(delta.errorsRemoved).toBe(1);
    expect(store.totals.files).toBe(1);
    expect(store.totals.filesWithProblems).toBe(0);
    expect(store.totals.errors).toBe(0);
  });

  test('a requested file missing from the round is forgotten (ignored/deleted)', () => {
    const store = new DiagnosticStore();
    const aPath = path.resolve('/project/src/a.ts');
    const bPath = path.resolve('/project/src/b.ts');
    store.replaceAll([
      result('src/a.ts', [['no-console', 2]]),
      result('src/b.ts', [['no-console', 2]]),
    ]);
    const delta = store.upsertFiles(
      [result('src/a.ts', [['no-console', 2]])],
      [aPath, bPath],
    );
    expect(delta.filesRemoved).toBe(1);
    expect(store.has(bPath)).toBe(false);
    expect(store.has(aPath)).toBe(true);
    expect(store.totals.files).toBe(1);
    expect(store.totals.errors).toBe(1);
  });

  test('deleteFiles removes only known files and counts the delta', () => {
    const store = new DiagnosticStore();
    const aPath = path.resolve('/project/src/a.ts');
    store.replaceAll([
      result('src/a.ts', [
        ['no-console', 2],
        ['eqeqeq', 1],
      ]),
      result('src/b.ts'),
    ]);
    const delta = store.deleteFiles([
      aPath,
      path.resolve('/project/src/never.ts'),
    ]);
    expect(delta.filesRemoved).toBe(1);
    expect(delta.errorsRemoved).toBe(1);
    expect(delta.warningsRemoved).toBe(1);
    expect(store.has(aPath)).toBe(false);
    expect(store.totals.files).toBe(1);
  });

  test('the next full round cannot retain stale files', () => {
    const store = new DiagnosticStore();
    store.replaceAll([
      result('src/a.ts', [['no-console', 2]]),
      result('src/b.ts', [['no-console', 2]]),
    ]);
    // Full round after a config/ignore change drops b and adds c.
    const delta = store.replaceAll([
      result('src/a.ts', [['no-console', 2]]),
      result('src/c.ts', [['eqeqeq', 1]]),
    ]);
    expect(delta.filesAdded).toBe(1);
    expect(delta.filesRemoved).toBe(1);
    expect(store.has(path.resolve('/project/src/b.ts'))).toBe(false);
    expect(store.has(path.resolve('/project/src/c.ts'))).toBe(true);
  });

  test('snapshot states are path-sorted for rendering', () => {
    const store = new DiagnosticStore();
    store.replaceAll([
      result('src/z.ts', [['no-console', 2]]),
      result('src/a.ts'),
    ]);
    const { states } = store.snapshot();
    expect(states.map((state) => path.basename(state.filePath))).toEqual([
      'a.ts',
      'z.ts',
    ]);
  });
});
