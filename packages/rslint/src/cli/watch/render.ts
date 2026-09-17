/**
 * Append-only watch mode renderer.
 *
 * Watch output never clears the screen, hides the cursor, or switches to an
 * alternate screen buffer: every round is appended below the previous one, so
 * the terminal state is exactly the state the process started with. Each
 * round prints (1) a header describing what triggered it, (2) diagnostics for
 * the files rechecked in THIS round only, and (3) a cumulative footer with
 * the problems still remembered across the whole watched scope. Older
 * diagnostics remain visible in the scrollback, while the footer always
 * answers "how many problems are left right now".
 */
import path from 'node:path';
import type { LintMessage } from '../../api/rslint.js';
import type { FileState, RoundDelta, StoreTotals } from './store.js';

export interface ColorOptions {
  /** Destination is a TTY (caller decides via its sink inspection). */
  tty: boolean;
  noColor?: boolean;
  forceColor?: boolean;
}

export interface RoundRenderInput {
  /** 1-based round number; the initial full check is round 1. */
  round: number;
  kind: 'initial' | 'full' | 'files';
  /** Human-readable triggers, e.g. "src/a.ts changed". */
  reasons: readonly string[];
  /** States of every file rechecked in this round (rendered diagnostics). */
  checked: readonly FileState[];
  /** Delta against the pre-round generation. */
  delta: RoundDelta;
  /** Cumulative post-round totals (the "what remains" answer). */
  totals: StoreTotals;
  quiet: boolean;
  /**
   * Fix results when the round ran with --fix. `issues` is the before/after
   * problem delta; the initial round has no previous generation, so it may be
   * omitted there even when fixes were written.
   */
  fixed?: { files: number; issues?: number } | undefined;
  /** Notices about files removed/added due to deletes, ignores, renames. */
  notices?: readonly string[];
  durationMs: number;
}

export interface FailedRoundRenderInput {
  round: number;
  kind: 'initial' | 'full' | 'files';
  reasons: readonly string[];
  error: string;
  /** Cumulative totals of the last-good generation still in force. */
  totals: StoreTotals;
}

const RESET = '\x1b[0m';
const RED = '\x1b[31m';
const GREEN = '\x1b[32m';
const YELLOW = '\x1b[33m';
const CYAN = '\x1b[36m';
const DIM = '\x1b[2m';

export function resolveColorEnabled(options: ColorOptions): boolean {
  if (options.noColor) return false;
  if (options.forceColor) return true;
  if (process.env.NO_COLOR !== undefined && process.env.NO_COLOR !== '') {
    return false;
  }
  if (process.env.FORCE_COLOR !== undefined && process.env.FORCE_COLOR !== '') {
    return true;
  }
  return options.tty;
}

function pad2(value: number): string {
  return value < 10 ? `0${value}` : String(value);
}

function timestamp(): string {
  const now = new Date();
  return `${pad2(now.getHours())}:${pad2(now.getMinutes())}:${pad2(now.getSeconds())}`;
}

function pluralize(count: number, singular: string): string {
  if (count === 1) return singular;
  if (/[sxz]$|[cs]h$/.test(singular)) return `${singular}es`;
  return `${singular}s`;
}

function messageLocation(message: LintMessage): string {
  const end =
    message.endLine !== undefined && message.endLine !== message.line
      ? `-${message.endLine}:${message.endColumn}`
      : '';
  return `${message.line}:${message.column}${end}`;
}

class Renderer {
  constructor(
    private readonly cwd: string,
    private readonly color: boolean,
  ) {}

  paint(code: string, text: string): string {
    return this.color ? `${code}${text}${RESET}` : text;
  }

  relative(filePath: string): string {
    const relative = path.relative(this.cwd, filePath);
    return relative.length === 0 || relative.startsWith('..')
      ? filePath
      : relative;
  }

  startBanner(
    debounceMs: number,
    pollMs: number,
    patterns: readonly string[],
  ): string {
    const scope = patterns.length > 0 ? patterns.join(' ') : '.';
    const lines = [
      `${this.paint(CYAN, '👀 rslint watch')} started (debounce ${debounceMs}ms${
        pollMs > 0 ? `, safety poll ${pollMs}ms` : ''
      })`,
      `${this.paint(DIM, 'scope')} ${scope}`,
    ];
    return lines.join('\n') + '\n';
  }

  round(input: RoundRenderInput): string {
    const lines: string[] = [];
    const kindLabel =
      input.kind === 'initial'
        ? 'initial check'
        : input.kind === 'full'
          ? 'full re-check'
          : 're-check';
    const trigger =
      input.reasons.length > 0 ? ` · ${input.reasons.join(', ')}` : '';
    lines.push(
      this.paint(
        DIM,
        `\n[round ${input.round} · ${kindLabel} · ${timestamp()}${trigger} · ${input.durationMs}ms]`,
      ),
    );

    for (const notice of input.notices ?? []) {
      lines.push(this.paint(DIM, `– ${notice}`));
    }

    if (input.fixed && input.fixed.files > 0) {
      const label =
        input.fixed.issues && input.fixed.issues > 0
          ? `${input.fixed.issues} ${pluralize(input.fixed.issues, 'fix')}`
          : 'fixes';
      lines.push(
        this.paint(
          GREEN,
          `applied ${label} to ${input.fixed.files} ${pluralize(input.fixed.files, 'file')}`,
        ),
      );
    }

    let roundErrors = 0;
    let roundWarnings = 0;
    for (const file of input.checked) {
      const messages = input.quiet
        ? file.messages.filter((message) => message.severity === 2)
        : file.messages;
      if (messages.length === 0) continue;
      const sorted = [...messages].sort(
        (left, right) =>
          left.line - right.line ||
          left.column - right.column ||
          right.severity - left.severity,
      );
      for (const message of sorted) {
        const isError = message.severity === 2;
        if (isError) roundErrors += 1;
        else roundWarnings += 1;
        const severity = isError
          ? this.paint(RED, 'error  ')
          : this.paint(YELLOW, 'warning');
        const rule = message.ruleId ? this.paint(DIM, message.ruleId) : '';
        lines.push(
          `${this.paint(CYAN, this.relative(file.filePath))}:${this.paint(
            DIM,
            messageLocation(message),
          )}  ${severity}  ${message.message}${rule ? `  ${rule}` : ''}`,
        );
      }
    }

    if (
      roundErrors === 0 &&
      roundWarnings === 0 &&
      (input.notices?.length ?? 0) === 0
    ) {
      lines.push(this.paint(GREEN, 'no problems in this round'));
    }

    lines.push(...this.footer(input.totals, input.delta, input.quiet));
    return lines.join('\n') + '\n';
  }

  failedRound(input: FailedRoundRenderInput): string {
    const kindLabel =
      input.kind === 'initial'
        ? 'initial check'
        : input.kind === 'full'
          ? 'full re-check'
          : 're-check';
    const trigger =
      input.reasons.length > 0 ? ` · ${input.reasons.join(', ')}` : '';
    const lines = [
      '',
      this.paint(
        DIM,
        `[round ${input.round} · ${kindLabel} · ${timestamp()}${trigger}]`,
      ),
      this.paint(RED, `✖ ${input.error}`),
      this.paint(DIM, 'keeping the previous round’s results'),
    ];
    lines.push(...this.footer(input.totals, undefined, false));
    return lines.join('\n') + '\n';
  }

  stop(exitCode: number): string {
    const outcome =
      exitCode === 0
        ? this.paint(GREEN, 'clean')
        : this.paint(RED, `exit ${exitCode}`);
    return `${this.paint(CYAN, 'watch stopped')} · last round: ${outcome}\n`;
  }

  /**
   * Cumulative footer — the single source of truth for "what remains". Delta
   * lines only render when the round changed something; a failed round omits
   * them entirely.
   */
  private footer(
    totals: StoreTotals,
    delta: RoundDelta | undefined,
    quiet: boolean,
  ): string[] {
    const warnings = quiet ? 0 : totals.warnings;
    const problems = totals.errors + warnings;
    const location = `across ${totals.filesWithProblems} of ${totals.files} ${pluralize(totals.files, 'file')}`;
    const lines: string[] = [];
    if (problems === 0) {
      lines.push(
        `${this.paint(GREEN, '✔ no problems remaining')} ${this.paint(DIM, `(${totals.files} ${pluralize(totals.files, 'file')} watched-scope results kept`)}`,
      );
    } else {
      const parts: string[] = [];
      if (totals.errors > 0) {
        parts.push(
          this.paint(
            RED,
            `${totals.errors} ${pluralize(totals.errors, 'error')}`,
          ),
        );
      }
      if (warnings > 0) {
        parts.push(
          this.paint(YELLOW, `${warnings} ${pluralize(warnings, 'warning')}`),
        );
      }
      lines.push(
        `${this.paint(RED, '✖')} ${parts.join(', ')} remaining ${this.paint(DIM, location)}`,
      );
    }
    if (delta) {
      const changes: string[] = [];
      if (delta.errorsAdded > 0)
        changes.push(
          `+${delta.errorsAdded} ${pluralize(delta.errorsAdded, 'error')}`,
        );
      if (!quiet && delta.warningsAdded > 0)
        changes.push(
          `+${delta.warningsAdded} ${pluralize(delta.warningsAdded, 'warning')}`,
        );
      if (delta.errorsRemoved > 0)
        changes.push(
          this.paint(
            GREEN,
            `−${delta.errorsRemoved} ${pluralize(delta.errorsRemoved, 'error')}`,
          ),
        );
      if (!quiet && delta.warningsRemoved > 0)
        changes.push(
          this.paint(
            GREEN,
            `−${delta.warningsRemoved} ${pluralize(delta.warningsRemoved, 'warning')}`,
          ),
        );
      if (delta.filesAdded > 0)
        changes.push(
          `+${delta.filesAdded} ${pluralize(delta.filesAdded, 'file')}`,
        );
      if (delta.filesRemoved > 0)
        changes.push(
          this.paint(
            DIM,
            `−${delta.filesRemoved} ${pluralize(delta.filesRemoved, 'file')}`,
          ),
        );
      if (changes.length > 0) {
        lines.push(this.paint(DIM, `this round: ${changes.join(', ')}`));
      }
    }
    return lines;
  }
}

export interface WatchRenderer {
  startBanner(
    debounceMs: number,
    pollMs: number,
    patterns: readonly string[],
  ): string;
  round(input: RoundRenderInput): string;
  failedRound(input: FailedRoundRenderInput): string;
  stop(exitCode: number): string;
}

export function createRenderer(cwd: string, color: boolean): WatchRenderer {
  return new Renderer(cwd, color);
}
