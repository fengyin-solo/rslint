package resultcache

import (
	"encoding/json"
	"fmt"

	"github.com/microsoft/TypeScript/tsc/shim/core"
	"github.com/web-infra-dev/rslint/internal/rule"
)

// RecordVersion versions the per-diagnostic payload independently of the
// envelope format; a change invalidates decoding (whole-entry miss) even if a
// future format keeps the envelope compatible.
const RecordVersion = 1

// cachedMessage mirrors rule.RuleMessage without any AST attachment.
type cachedMessage struct {
	Id          string            `json:"id,omitempty"`
	Description string            `json:"description"`
	Data        map[string]string `json:"data,omitempty"`
}

// cachedFix is one UTF-16 offset range plus its replacement text.
type cachedFix struct {
	Text  string `json:"text"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// cachedSuggestion is one rule suggestion: message plus ordered fixes.
type cachedSuggestion struct {
	Message cachedMessage `json:"message"`
	Fixes   []cachedFix   `json:"fixes,omitempty"`
}

// cachedDiagnostic is the AST-free, JSON-persisted projection of one
// rule.RuleDiagnostic. Offsets are flat UTF-16 positions exactly as rule
// code reported them; on replay the gate re-attaches the content-identical
// SourceFile so renderers compute line/column against the current
// generation.
type cachedDiagnostic struct {
	Version      int                `json:"v"`
	RuleName     string             `json:"rule"`
	Severity     int                `json:"severity"`
	Origin       int                `json:"origin"`
	PreFormatted bool               `json:"preformatted,omitempty"`
	Message      cachedMessage      `json:"message"`
	Start        int                `json:"start"`
	End          int                `json:"end"`
	Fixes        []cachedFix        `json:"fixes,omitempty"`
	Suggestions  []cachedSuggestion `json:"suggestions,omitempty"`
}

// EncodeDiagnostics projects complete per-file diagnostics into storable JSON
// records. The order of input is preserved verbatim; callers persist only
// diagnostics from a completed, sorted observation.
func EncodeDiagnostics(diagnostics []rule.RuleDiagnostic) ([]json.RawMessage, error) {
	if len(diagnostics) == 0 {
		return nil, nil
	}
	encoded := make([]json.RawMessage, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		record := cachedDiagnostic{
			Version:      RecordVersion,
			RuleName:     diagnostic.RuleName,
			Severity:     int(diagnostic.Severity),
			Origin:       int(diagnostic.Origin),
			PreFormatted: diagnostic.PreFormatted,
			Message: cachedMessage{
				Id:          diagnostic.Message.Id,
				Description: diagnostic.Message.Description,
				Data:        cloneStringMap(diagnostic.Message.Data),
			},
			Start: diagnostic.Range.Pos(),
			End:   diagnostic.Range.End(),
		}
		if diagnostic.FixesPtr != nil {
			record.Fixes = make([]cachedFix, 0, len(*diagnostic.FixesPtr))
			for _, fix := range *diagnostic.FixesPtr {
				record.Fixes = append(record.Fixes, cachedFix{
					Text:  fix.Text,
					Start: fix.Range.Pos(),
					End:   fix.Range.End(),
				})
			}
		}
		if diagnostic.Suggestions != nil {
			record.Suggestions = make([]cachedSuggestion, 0, len(*diagnostic.Suggestions))
			for _, suggestion := range *diagnostic.Suggestions {
				item := cachedSuggestion{
					Message: cachedMessage{
						Id:          suggestion.Message.Id,
						Description: suggestion.Message.Description,
						Data:        cloneStringMap(suggestion.Message.Data),
					},
				}
				item.Fixes = make([]cachedFix, 0, len(suggestion.FixesArr))
				for _, fix := range suggestion.FixesArr {
					item.Fixes = append(item.Fixes, cachedFix{
						Text:  fix.Text,
						Start: fix.Range.Pos(),
						End:   fix.Range.End(),
					})
				}
				record.Suggestions = append(record.Suggestions, item)
			}
		}
		data, err := json.Marshal(record)
		if err != nil {
			return nil, fmt.Errorf("resultcache: encode diagnostic %q: %w", diagnostic.RuleName, err)
		}
		encoded = append(encoded, data)
	}
	return encoded, nil
}

// DecodeDiagnostics reverses EncodeDiagnostics. It is deliberately
// all-or-nothing at the entry level: if a single record is malformed, from an
// unsupported payload version, or structurally invalid, the whole entry is a
// miss. Returning a partial diagnostic set could silently drop a diagnostic,
// which the cache must never do.
//
// The returned diagnostics carry no SourceFile; the gate attaches the current
// Program's SourceFile and the stable target FilePath.
func DecodeDiagnostics(records []json.RawMessage) ([]rule.RuleDiagnostic, error) {
	if len(records) == 0 {
		return nil, nil
	}
	diagnostics := make([]rule.RuleDiagnostic, 0, len(records))
	for index, data := range records {
		var record cachedDiagnostic
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("decode diagnostic %d: %w", index, err)
		}
		if record.Version != RecordVersion {
			return nil, fmt.Errorf("decode diagnostic %d: unsupported record version %d", index, record.Version)
		}
		if record.Start < 0 || record.End < 0 || record.Start > record.End {
			return nil, fmt.Errorf("decode diagnostic %d: invalid range %d:%d", index, record.Start, record.End)
		}
		diagnostic := rule.RuleDiagnostic{
			Range:        core.NewTextRange(record.Start, record.End),
			RuleName:     record.RuleName,
			Message:      rule.RuleMessage{Id: record.Message.Id, Description: record.Message.Description, Data: cloneStringMap(record.Message.Data)},
			Severity:     rule.DiagnosticSeverity(record.Severity),
			Origin:       rule.DiagnosticOrigin(record.Origin),
			PreFormatted: record.PreFormatted,
		}
		if len(record.Fixes) > 0 {
			fixes := make([]rule.RuleFix, 0, len(record.Fixes))
			for _, fix := range record.Fixes {
				if fix.Start < 0 || fix.End < 0 || fix.Start > fix.End {
					return nil, fmt.Errorf("decode diagnostic %d: invalid fix range", index)
				}
				fixes = append(fixes, rule.RuleFix{
					Text:  fix.Text,
					Range: core.NewTextRange(fix.Start, fix.End),
				})
			}
			diagnostic.FixesPtr = &fixes
		}
		if len(record.Suggestions) > 0 {
			suggestions := make([]rule.RuleSuggestion, 0, len(record.Suggestions))
			for _, suggestion := range record.Suggestions {
				fixes := make([]rule.RuleFix, 0, len(suggestion.Fixes))
				for _, fix := range suggestion.Fixes {
					if fix.Start < 0 || fix.End < 0 || fix.Start > fix.End {
						return nil, fmt.Errorf("decode diagnostic %d: invalid suggestion fix range", index)
					}
					fixes = append(fixes, rule.RuleFix{
						Text:  fix.Text,
						Range: core.NewTextRange(fix.Start, fix.End),
					})
				}
				suggestions = append(suggestions, rule.RuleSuggestion{
					Message: rule.RuleMessage{
						Id:          suggestion.Message.Id,
						Description: suggestion.Message.Description,
						Data:        cloneStringMap(suggestion.Message.Data),
					},
					FixesArr: fixes,
				})
			}
			diagnostic.Suggestions = &suggestions
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	return diagnostics, nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
