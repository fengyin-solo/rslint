package output

import (
	"errors"
	"slices"
	"time"
)

type Mode uint8

const (
	ModeLint Mode = iota
	ModeLintAndTypeCheck
	ModeTypeCheckOnly
)

// Severity is the presentation-level severity consumed by output formatters.
// It deliberately does not expose the rule framework's diagnostic type.
type Severity uint8

const (
	SeverityError Severity = iota
	SeverityWarning
	SeverityOff
)

func (severity Severity) String() string {
	switch severity {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warn"
	case SeverityOff:
		return "off"
	default:
		return "error"
	}
}

// Position is zero-based; Column is measured in UTF-16 code units.
type Position struct {
	Line   int
	Column int
}

// TextRange is a half-open range of UTF-8 byte offsets in the source text.
type TextRange struct {
	Start int
	End   int
}

// DiagnosticSource is an immutable, output-owned source snapshot used for
// code frames and location validation. It retains no compiler object and
// clones the caller-owned mutable line map.
type DiagnosticSource struct {
	text       string
	lineStarts []int
}

// NewDiagnosticSource validates that source presentation data is safe to
// consume and copies its mutable, UTF-8-byte-offset line starts into
// output-owned ints. The narrow integer constraint accepts both native Go
// offsets and ts-go TextPos values without importing compiler-core contracts.
// The producer owns ECMAScript line-boundary semantics; output must not
// rediscover line boundaries from the source text.
func NewDiagnosticSource[T ~int | ~int32](text string, lineStarts []T) (*DiagnosticSource, error) {
	if len(lineStarts) == 0 || lineStarts[0] != 0 {
		return nil, errors.New("source line starts are not structurally valid")
	}
	textLength := len(text)
	ownedLineStarts := make([]int, len(lineStarts))
	previous := -1
	for index, value := range lineStarts {
		lineStart := int(value)
		if lineStart <= previous || lineStart > textLength {
			return nil, errors.New("source line starts are not structurally valid")
		}
		ownedLineStarts[index] = lineStart
		previous = lineStart
	}
	return &DiagnosticSource{text: text, lineStarts: ownedLineStarts}, nil
}

// Diagnostic is the presentation projection of one finding. Location values
// have already been detached from the lint domain; Source is populated only
// when the selected formatter needs a code frame.
type Diagnostic struct {
	FilePath     string
	RuleName     string
	Message      string
	Range        TextRange
	Start        Position
	End          Position
	Source       *DiagnosticSource
	Severity     Severity
	PreFormatted bool
	// Fixes is empty for a diagnostic that offers no autofix. Every entry is
	// one replacement over a half-open UTF-8 byte range with its replacement
	// text. Machine formats that render fixes (for example SARIF) consume it;
	// the other formatters ignore it.
	Fixes []Fix
}

// Fix is one replacement edit attached to a Diagnostic. Range is a half-open
// interval of UTF-8 byte offsets in the source text; NewText replaces it (an
// empty string means deletion).
type Fix struct {
	Range   TextRange
	NewText string
}

// RuleInfo is the presentation projection of one rule's tool metadata. It is
// output-owned: the command layer projects rule-framework metadata into it
// before rendering.
type RuleInfo struct {
	Name        string
	Description string
	// HelpURI links the rule's human-readable documentation. It may be empty
	// when no documentation is known.
	HelpURI string
	// DefaultLevel is the rule's level in the run's resolved configuration
	// (error/warn); the SARIF driver exposes it as defaultConfiguration.level.
	DefaultLevel Severity
}

// Suppression locates the inline disable directive that suppressed a
// diagnostic. Start/End are the directive comment's own zero-based line and
// UTF-16 code-unit columns.
type Suppression struct {
	Start Position
	End   Position
}

// SuppressedDiagnostic pairs a diagnostic that an inline disable directive
// suppressed with the directive responsible. The diagnostic stays out of
// error/warning counts and the human-readable formats; machine formats can
// render it as a suppression record.
type SuppressedDiagnostic struct {
	Diagnostic  Diagnostic
	Suppression Suppression
}

// OutcomeKind is the CLI decision that drives the completed status line. The
// command computes it once so rendering and the process exit code cannot
// disagree (notably when --max-warnings is exceeded).
type OutcomeKind uint8

const (
	OutcomePassed OutcomeKind = iota
	OutcomeDiagnosticsFailed
	OutcomeWarningLimitExceeded
)

type Outcome struct {
	Kind         OutcomeKind
	WarningLimit int
}

func (outcome Outcome) Failed() bool {
	return outcome.Kind != OutcomePassed
}

// Summary contains the default formatter's completed-run facts that are not
// already part of the Report. A nil Summary means the report is
// diagnostics-only and avoids computing data that machine formats never use.
type Summary struct {
	Files       int
	Rules       int
	Threads     int
	FixedIssues int
	StartedAt   time.Time
}

type Counts struct {
	Errors     int
	Warnings   int
	LintErrors int
	TypeErrors int
}

type Report struct {
	mode        Mode
	diagnostics []Diagnostic
	suppressed  []SuppressedDiagnostic
	rules       []RuleInfo
	summary     Summary
	hasSummary  bool
	counts      Counts
	outcome     Outcome
}

// NewReport snapshots a completed CLI report. Counts and outcome are supplied
// by the command-owned assembly stage; output treats that projection as
// authoritative and only retains and renders it.
func NewReport(
	mode Mode,
	diagnostics []Diagnostic,
	suppressed []SuppressedDiagnostic,
	rules []RuleInfo,
	counts Counts,
	summary *Summary,
	outcome Outcome,
) Report {
	var ownedSummary Summary
	hasSummary := summary != nil
	if summary != nil {
		ownedSummary = *summary
	}
	return Report{
		mode:        mode,
		diagnostics: slices.Clone(diagnostics),
		suppressed:  slices.Clone(suppressed),
		rules:       slices.Clone(rules),
		summary:     ownedSummary,
		hasSummary:  hasSummary,
		counts:      counts,
		outcome:     outcome,
	}
}

func (report Report) Counts() Counts {
	return report.counts
}

func (report Report) Outcome() Outcome {
	return report.outcome
}

// SuppressedDiagnostics returns the diagnostics inline disable directives
// suppressed, in the command-assigned (stable) order.
func (report Report) SuppressedDiagnostics() []SuppressedDiagnostic {
	return report.suppressed
}

// Rules returns the report's rule metadata in the command-assigned order.
func (report Report) Rules() []RuleInfo {
	return report.rules
}
