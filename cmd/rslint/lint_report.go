package main

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/microsoft/TypeScript/tsc/shim/scanner"
	rslintconfig "github.com/web-infra-dev/rslint/internal/config"
	"github.com/web-infra-dev/rslint/internal/config/target"
	"github.com/web-infra-dev/rslint/internal/linter"
	"github.com/web-infra-dev/rslint/internal/output"
	"github.com/web-infra-dev/rslint/internal/rule"
	"github.com/web-infra-dev/rslint/internal/rulemd"
)

type cancellationAwareWriter struct {
	ctx context.Context
	dst io.Writer
}

func (w cancellationAwareWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.dst.Write(p)
}

// renderLintReport keeps cancellation policy in the command layer while the
// output package remains responsible only for presentation. Besides checking
// before rendering starts, the writer check prevents a cancellation observed
// after diagnostics flush from being followed by a completed status.
func renderLintReport(
	ctx context.Context,
	dst io.Writer,
	report output.Report,
	options output.Options,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return output.Render(cancellationAwareWriter{ctx: ctx, dst: dst}, report, options)
}

type lintReportInput struct {
	Mode          output.Mode
	Diagnostics   []rule.RuleDiagnostic
	Suppressed    []rule.SuppressedDiagnostic
	RuleSeverity  map[string]rule.DiagnosticSeverity
	Summary       *output.Summary
	MaxWarnings   int
	IncludeSource bool
}

// assembleLintReport is the command-owned projection from completed linter
// diagnostics to the presentation contract. It performs no filesystem or
// Program queries: every run-level fact must already be frozen by its owner.
func assembleLintReport(input lintReportInput) (output.Report, error) {
	diagnostics := make([]output.Diagnostic, len(input.Diagnostics))
	var sourcesByPath map[string]lintReportSourceProjection
	if input.IncludeSource {
		sourcesByPath = make(map[string]lintReportSourceProjection)
	}
	counts := output.Counts{}
	for index, diagnostic := range input.Diagnostics {
		projected, err := projectLintReportDiagnostic(diagnostic, sourcesByPath)
		if err != nil {
			return output.Report{}, err
		}
		diagnostics[index] = projected
		typeScriptOrigin, err := lintReportDiagnosticHasTypeScriptOrigin(diagnostic.Origin)
		if err != nil {
			return output.Report{}, fmt.Errorf(
				"diagnostic %q for %q: %w",
				diagnostic.RuleName,
				diagnostic.FilePath,
				err,
			)
		}
		switch projected.Severity {
		case output.SeverityError:
			counts.Errors++
			if input.Mode != output.ModeLint && typeScriptOrigin {
				counts.TypeErrors++
			}
		case output.SeverityWarning:
			counts.Warnings++
		}
	}
	counts.LintErrors = counts.Errors - counts.TypeErrors

	suppressed, err := projectSuppressedDiagnostics(input.Suppressed)
	if err != nil {
		return output.Report{}, err
	}
	rules := assembleRuleInfos(input.RuleSeverity, diagnostics, suppressed)

	outcome := lintReportOutcome(counts, input.MaxWarnings)
	return output.NewReport(input.Mode, diagnostics, suppressed, rules, counts, input.Summary, outcome), nil
}

type lintReportSourceProjection struct {
	text   string
	source *output.DiagnosticSource
}

func projectLintReportDiagnostic(
	diagnostic rule.RuleDiagnostic,
	sourcesByPath map[string]lintReportSourceProjection,
) (output.Diagnostic, error) {
	if diagnostic.SourceFile == nil {
		return output.Diagnostic{}, fmt.Errorf(
			"diagnostic %q for %q has no source file",
			diagnostic.RuleName,
			diagnostic.FilePath,
		)
	}
	start, end := diagnostic.Range.Pos(), diagnostic.Range.End()
	text := diagnostic.SourceFile.Text()
	if start < 0 || end < start || end > len(text) {
		return output.Diagnostic{}, fmt.Errorf(
			"diagnostic %q for %q has invalid range [%d,%d) for source length %d",
			diagnostic.RuleName,
			diagnostic.FilePath,
			start,
			end,
			len(text),
		)
	}
	var fixes []output.Fix
	for _, fix := range diagnostic.Fixes() {
		fixStart, fixEnd := fix.Range.Pos(), fix.Range.End()
		if fixStart < 0 || fixEnd < fixStart || fixEnd > len(text) {
			return output.Diagnostic{}, fmt.Errorf(
				"diagnostic %q for %q has fix with invalid range [%d,%d) for source length %d",
				diagnostic.RuleName,
				diagnostic.FilePath,
				fixStart,
				fixEnd,
				len(text),
			)
		}
		fixes = append(fixes, output.Fix{
			Range:   output.TextRange{Start: fixStart, End: fixEnd},
			NewText: fix.Text,
		})
	}
	severity, err := projectLintReportSeverity(diagnostic.Severity)
	if err != nil {
		return output.Diagnostic{}, fmt.Errorf(
			"diagnostic %q for %q: %w",
			diagnostic.RuleName,
			diagnostic.FilePath,
			err,
		)
	}
	startLine, startColumn := scanner.GetECMALineAndUTF16CharacterOfPosition(diagnostic.SourceFile, start)
	endLine, endColumn := scanner.GetECMALineAndUTF16CharacterOfPosition(diagnostic.SourceFile, end)
	var projectedSource *output.DiagnosticSource
	if sourcesByPath != nil {
		sourceProjection, cached := sourcesByPath[diagnostic.FilePath]
		if !cached || sourceProjection.text != text {
			compilerLineStarts := scanner.GetECMALineStarts(diagnostic.SourceFile)
			projectedSource, err = output.NewDiagnosticSource(text, compilerLineStarts)
			if err != nil {
				return output.Diagnostic{}, fmt.Errorf(
					"diagnostic %q for %q has invalid source metadata: %w",
					diagnostic.RuleName,
					diagnostic.FilePath,
					err,
				)
			}
			sourceProjection = lintReportSourceProjection{
				text:   text,
				source: projectedSource,
			}
			sourcesByPath[diagnostic.FilePath] = sourceProjection
		} else {
			projectedSource = sourceProjection.source
		}
	}
	return output.Diagnostic{
		FilePath:     diagnostic.FilePath,
		RuleName:     diagnostic.RuleName,
		Message:      diagnostic.Message.Description,
		Range:        output.TextRange{Start: start, End: end},
		Start:        output.Position{Line: startLine, Column: int(startColumn)},
		End:          output.Position{Line: endLine, Column: int(endColumn)},
		Source:       projectedSource,
		Severity:     severity,
		PreFormatted: diagnostic.PreFormatted,
		Fixes:        fixes,
	}, nil
}

// projectSuppressedDiagnostics projects diagnostics removed by inline disable
// directives together with the directive comment locations. Suppressed
// findings never carry source frames or fix artifacts.
func projectSuppressedDiagnostics(suppressed []rule.SuppressedDiagnostic) ([]output.SuppressedDiagnostic, error) {
	if len(suppressed) == 0 {
		return nil, nil
	}
	projected := make([]output.SuppressedDiagnostic, 0, len(suppressed))
	for _, item := range suppressed {
		diagnostic, err := projectLintReportDiagnostic(item.Diagnostic, nil)
		if err != nil {
			return nil, err
		}
		if item.Diagnostic.SourceFile == nil {
			return nil, fmt.Errorf(
				"suppressed diagnostic %q for %q has no source file",
				item.Diagnostic.RuleName,
				item.Diagnostic.FilePath,
			)
		}
		commentStart := item.Directive.CommentRange.Pos()
		commentEnd := item.Directive.CommentRange.End()
		startLine, startColumn := scanner.GetECMALineAndUTF16CharacterOfPosition(item.Diagnostic.SourceFile, commentStart)
		endLine, endColumn := scanner.GetECMALineAndUTF16CharacterOfPosition(item.Diagnostic.SourceFile, commentEnd)
		projected = append(projected, output.SuppressedDiagnostic{
			Diagnostic: diagnostic,
			Suppression: output.Suppression{
				Start: output.Position{Line: startLine, Column: int(startColumn)},
				End:   output.Position{Line: endLine, Column: int(endColumn)},
			},
		})
	}
	return projected, nil
}

// assembleRuleInfos builds the run's rule metadata. Every executed rule is
// included, plus any rule referenced by a visible or suppressed diagnostic
// (for example TypeScript(TSxxxx) syntax findings). Order is lexicographic so
// rule indexes stay stable regardless of map or scheduling order.
func assembleRuleInfos(
	severities map[string]rule.DiagnosticSeverity,
	diagnostics []output.Diagnostic,
	suppressed []output.SuppressedDiagnostic,
) []output.RuleInfo {
	levels := make(map[string]output.Severity, len(severities))
	consider := func(name string, severity output.Severity) {
		if existing, ok := levels[name]; !ok || strongerSeverity(severity, existing) {
			levels[name] = severity
		}
	}
	for name, severity := range severities {
		projected, err := projectLintReportSeverity(severity)
		if err != nil {
			continue
		}
		consider(name, projected)
	}
	for index := range diagnostics {
		consider(diagnostics[index].RuleName, diagnostics[index].Severity)
	}
	for index := range suppressed {
		consider(suppressed[index].Diagnostic.RuleName, suppressed[index].Diagnostic.Severity)
	}

	registry := rulemd.Default()
	names := make([]string, 0, len(levels))
	for name := range levels {
		names = append(names, name)
	}
	sort.Strings(names)

	rules := make([]output.RuleInfo, 0, len(names))
	for _, name := range names {
		info := output.RuleInfo{
			Name:         name,
			DefaultLevel: levels[name],
		}
		if description := registry.Description(name); description != "" {
			info.Description = description
			info.HelpURI = rulemd.HelpURI(name)
		}
		rules = append(rules, info)
	}
	return rules
}

// strongerSeverity reports whether a should win over b when the same rule ran
// with different configured levels. Error outranks warning.
func strongerSeverity(a, b output.Severity) bool {
	if a == output.SeverityError {
		return b != output.SeverityError
	}
	if a == output.SeverityWarning {
		return b == output.SeverityOff
	}
	return false
}

func lintReportDiagnosticHasTypeScriptOrigin(origin rule.DiagnosticOrigin) (bool, error) {
	switch origin {
	case rule.DiagnosticOriginLint:
		return false, nil
	case rule.DiagnosticOriginTypeScript:
		return true, nil
	default:
		return false, fmt.Errorf("invalid diagnostic origin %d", origin)
	}
}

func projectLintReportSeverity(severity rule.DiagnosticSeverity) (output.Severity, error) {
	switch severity {
	case rule.SeverityError:
		return output.SeverityError, nil
	case rule.SeverityWarning:
		return output.SeverityWarning, nil
	case rule.SeverityOff:
		return output.SeverityOff, nil
	default:
		return output.SeverityOff, fmt.Errorf("invalid diagnostic severity %d", severity)
	}
}

func projectLintRuleTimings(timings map[string]linter.RuleTiming) map[string]output.RuleTiming {
	projected := make(map[string]output.RuleTiming, len(timings))
	for name, timing := range timings {
		projected[name] = output.RuleTiming{
			Kind:  timing.Kind,
			Time:  timing.Time,
			Files: timing.Files,
		}
	}
	return projected
}

// lintReportOutcome is the command-owned exit policy. The returned value is
// also passed to the output renderer so the status line and exit code consume
// one decision.
func lintReportOutcome(counts output.Counts, maxWarnings int) output.Outcome {
	switch {
	case counts.Errors > 0:
		return output.Outcome{Kind: output.OutcomeDiagnosticsFailed}
	case maxWarnings >= 0 && counts.Warnings > maxWarnings:
		return output.Outcome{
			Kind:         output.OutcomeWarningLimitExceeded,
			WarningLimit: maxWarnings,
		}
	default:
		return output.Outcome{Kind: output.OutcomePassed}
	}
}

// lintReportFileCount returns the user-visible number of distinct files. Lint
// targets and compiler roots describe different phases, so combined mode uses
// their canonical union rather than either execution count or max(set sizes).
func lintReportFileCount(
	mode output.Mode,
	lintTargets []target.File,
	typeCheckedRootIDs []string,
) int {
	seen := make(map[string]struct{}, len(lintTargets)+len(typeCheckedRootIDs))
	if mode != output.ModeTypeCheckOnly {
		for _, lintTarget := range lintTargets {
			path := lintTarget.CanonicalPath
			if path == "" {
				path = lintTarget.Path
			}
			// A target.Plan has already frozen physical identity for this run.
			// Re-resolving it against the live filesystem here could make the
			// reported count disagree with the files the pipeline actually used.
			seen[rslintconfig.ExactPathID(path)] = struct{}{}
		}
	}
	if mode != output.ModeLint {
		for _, rootID := range typeCheckedRootIDs {
			seen[rootID] = struct{}{}
		}
	}
	return len(seen)
}
