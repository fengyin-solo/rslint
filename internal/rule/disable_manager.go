package rule

import (
	"strings"

	"github.com/microsoft/TypeScript/tsc/shim/ast"
	"github.com/microsoft/TypeScript/tsc/shim/core"
	"github.com/microsoft/TypeScript/tsc/shim/scanner"
)

// directiveKind represents the type of an inline directive comment.
type directiveKind int

const (
	directiveNone     directiveKind = iota
	directiveBlock                  // rslint-disable / eslint-disable (block)
	directiveEnable                 // rslint-enable / eslint-enable
	directiveLine                   // rslint-disable-line / eslint-disable-line
	directiveNextLine               // rslint-disable-next-line / eslint-disable-next-line
)

// SuppressionKind identifies the inline directive form responsible for a
// suppression.
type SuppressionKind int

const (
	// SuppressionBlock is a rslint-disable/eslint-disable block directive.
	SuppressionBlock SuppressionKind = iota
	// SuppressionLine is a rslint-disable-line/eslint-disable-line directive.
	SuppressionLine
	// SuppressionNextLine is a rslint-disable-next-line/eslint-disable-next-line
	// directive.
	SuppressionNextLine
)

// DirectiveSuppression identifies the disable directive comment that
// suppressed a diagnostic. CommentRange is the full comment trivia range
// (including its // or /* */ markers) as a half-open UTF-8 byte interval.
type DirectiveSuppression struct {
	Kind         SuppressionKind
	CommentRange core.TextRange
}

// directivePrefix defines the comment prefixes for disable/enable directives.
type directivePrefix struct {
	disable string // e.g. "rslint-disable"
	enable  string // e.g. "rslint-enable"
}

// directivePrefixes lists the supported directive prefixes.
// Both rslint- and eslint- prefixes are supported and fully equivalent.
var directivePrefixes = []directivePrefix{
	{"rslint-disable", "rslint-enable"},
	{"eslint-disable", "eslint-enable"},
}

// directiveRecord is one parsed disable/enable comment with its source
// location. nil rules means the directive applies to every rule (wildcard).
type directiveRecord struct {
	kind   directiveKind
	line   int
	column core.UTF16Offset
	start  int
	end    int
	rules  []string
}

// DisableManager tracks which rules are disabled at different locations in a file
type DisableManager struct {
	sourceFile *ast.SourceFile
	comments   *CommentStore
	parsed     bool
	directives []directiveRecord
}

// NewDisableManager creates a manager whose directives are parsed on the first
// disable check. The manager does not materialize comments without a directive.
func NewDisableManager(sourceFile *ast.SourceFile, comments *CommentStore) *DisableManager {
	return &DisableManager{
		sourceFile: sourceFile,
		comments:   comments,
	}
}

func (dm *DisableManager) ensureParsed() {
	if dm == nil || dm.parsed {
		return
	}
	dm.parsed = true
	if dm.sourceFile == nil || !mayContainDisableDirective(dm.sourceFile.Text()) {
		return
	}
	dm.parseDirectives(dm.comments.All())
}

func mayContainDisableDirective(text string) bool {
	const marker = "lint-"
	for searchStart := 0; searchStart < len(text); {
		offset := strings.Index(text[searchStart:], marker)
		if offset < 0 {
			return false
		}
		start := searchStart + offset
		if start >= 2 {
			prefix := text[start-2 : start]
			rest := text[start+len(marker):]
			if (prefix == "rs" || prefix == "es") &&
				(strings.HasPrefix(rest, "disable") || strings.HasPrefix(rest, "enable")) {
				return true
			}
		}
		searchStart = start + len(marker)
	}
	return false
}

// parseDirectives parses disable/enable directive comments from the source text.
// Both rslint- and eslint- prefixed directives are recognized.
func (dm *DisableManager) parseDirectives(comments []*ast.CommentRange) {
	if dm.sourceFile.Text() == "" || len(comments) == 0 {
		return
	}

	text := dm.sourceFile.Text()

	for _, comment := range comments {
		var commentContent string
		switch comment.Kind {
		case ast.KindSingleLineCommentTrivia:
			commentContent = strings.TrimSpace(text[comment.Pos()+2 : comment.End()])
		case ast.KindMultiLineCommentTrivia:
			commentContent = strings.TrimSpace(text[comment.Pos()+2 : comment.End()-2])
		default:
			continue
		}

		kind, rules := matchDirective(commentContent)
		if kind == directiveNone {
			continue
		}

		lineNum, columnNum := scanner.GetECMALineAndUTF16CharacterOfPosition(dm.sourceFile, comment.Pos())
		dm.directives = append(dm.directives, directiveRecord{
			kind:   kind,
			line:   lineNum,
			column: columnNum,
			start:  comment.Pos(),
			end:    comment.End(),
			rules:  rules,
		})
	}
}

// matchDirective checks if a comment content string is a disable/enable directive.
// Returns the directive kind and any specified rule names.
func matchDirective(commentContent string) (directiveKind, []string) {
	for _, p := range directivePrefixes {
		if strings.HasPrefix(commentContent, p.disable) {
			rest := commentContent[len(p.disable):]
			if strings.HasPrefix(rest, "-line") {
				return directiveLine, parseRuleNames(rest[len("-line"):])
			}
			if strings.HasPrefix(rest, "-next-line") {
				return directiveNextLine, parseRuleNames(rest[len("-next-line"):])
			}
			return directiveBlock, parseRuleNames(rest)
		}
		if strings.HasPrefix(commentContent, p.enable) {
			return directiveEnable, parseRuleNames(commentContent[len(p.enable):])
		}
	}
	return directiveNone, nil
}

// parseRuleNames parses rule names from a string like " rule1, rule2, rule3"
// It also strips inline descriptions after " -- " (e.g., "rule1 -- reason")
func parseRuleNames(rulesStr string) []string {
	// Strip inline description after " -- " before trimming, so that
	// wildcard-with-description like " -- reason" is correctly handled.
	if idx := strings.Index(rulesStr, " -- "); idx != -1 {
		rulesStr = rulesStr[:idx]
	}

	rulesStr = strings.TrimSpace(rulesStr)
	if rulesStr == "" {
		return nil
	}

	var rules []string
	for _, rule := range strings.Split(rulesStr, ",") {
		rule = strings.TrimSpace(rule)
		if rule != "" {
			rules = append(rules, rule)
		}
	}
	return rules
}

// IsRuleDisabled checks if a rule is disabled at the given position
func (dm *DisableManager) IsRuleDisabled(ruleName string, pos int) bool {
	_, disabled := dm.Suppression(ruleName, pos)
	return disabled
}

// Suppression returns the directive suppressing ruleName at pos when one
// applies. The replay order and wildcard/specific precedence are identical to
// IsRuleDisabled: block directives first (in source order), then same-line
// directives, then next-line directives.
func (dm *DisableManager) Suppression(ruleName string, pos int) (DirectiveSuppression, bool) {
	if dm == nil || dm.sourceFile == nil {
		return DirectiveSuppression{}, false
	}
	dm.ensureParsed()
	if len(dm.directives) == 0 {
		return DirectiveSuppression{}, false
	}

	line, column := scanner.GetECMALineAndUTF16CharacterOfPosition(dm.sourceFile, pos)

	if record, ok := dm.blockSuppression(ruleName, line, column); ok {
		return DirectiveSuppression{
			Kind:         SuppressionBlock,
			CommentRange: core.NewTextRange(record.start, record.end),
		}, true
	}

	if record, ok := dm.lineSuppression(ruleName, line, directiveLine); ok {
		return DirectiveSuppression{
			Kind:         SuppressionLine,
			CommentRange: core.NewTextRange(record.start, record.end),
		}, true
	}

	if record, ok := dm.lineSuppression(ruleName, line, directiveNextLine); ok {
		return DirectiveSuppression{
			Kind:         SuppressionNextLine,
			CommentRange: core.NewTextRange(record.start, record.end),
		}, true
	}

	return DirectiveSuppression{}, false
}

// blockSuppression replays block disable/enable directives in source order to
// determine whether a rule is disabled at the given source location and, when
// so, which directive decided it.
func (dm *DisableManager) blockSuppression(ruleName string, line int, column core.UTF16Offset) (directiveRecord, bool) {
	allDisabled := false
	ruleDisabled := false
	hasRuleSpecific := false

	var wildcardRecord directiveRecord
	var ruleRecord directiveRecord

	for _, d := range dm.directives {
		if d.kind != directiveBlock && d.kind != directiveEnable {
			continue
		}
		if d.line > line || (d.line == line && d.column > column) {
			break
		}

		if len(d.rules) == 0 {
			// Wildcard directive: affects all rules and resets rule-specific state
			allDisabled = d.kind == directiveBlock
			hasRuleSpecific = false
			wildcardRecord = d
		} else {
			for _, r := range d.rules {
				if r == ruleName {
					ruleDisabled = d.kind == directiveBlock
					hasRuleSpecific = true
					ruleRecord = d
				}
			}
		}
	}

	if hasRuleSpecific {
		return ruleRecord, ruleDisabled
	}
	return wildcardRecord, allDisabled
}

// lineSuppression finds a same-line (directiveLine) or next-line
// (directiveNextLine) directive applying to ruleName on line. When several
// comments on the line qualify, a rule-specific directive is attributed in
// preference to a wildcard one, matching the rule that a consumer should show.
func (dm *DisableManager) lineSuppression(
	ruleName string,
	line int,
	kind directiveKind,
) (directiveRecord, bool) {
	var wildcardRecord directiveRecord
	hasWildcard := false
	for _, d := range dm.directives {
		if d.kind != kind || d.line != line {
			continue
		}
		if len(d.rules) == 0 {
			if !hasWildcard {
				wildcardRecord = d
				hasWildcard = true
			}
			continue
		}
		for _, r := range d.rules {
			if r == ruleName {
				return d, true
			}
		}
	}
	if hasWildcard {
		return wildcardRecord, true
	}
	return directiveRecord{}, false
}
