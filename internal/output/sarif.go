package output

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/microsoft/TypeScript/tsc/shim/tspath"
)

// SARIF 2.1.0 output. The driver lists every rule the run executed with its
// description, help link, and default level. Every visible diagnostic is a
// result; diagnostics suppressed by inline disable directives are results
// carrying an inSource suppression instead of disappearing. Fixable
// diagnostics include replacement edits. Rule ordering (and therefore rule
// indexes) and fingerprints are deterministic for the same input.

const (
	sarifVersion   = "2.1.0"
	sarifSchemaURI = "https://json.schemastore.org/sarif-2.1.0.json"
	sarifToolName  = "rslint"
	sarifToolURI   = "https://rslint.rs"
	sarifPrimaryFP = "primaryFingerprint"
)

type sarifFormatter struct {
	version     string
	comparePath tspath.ComparePathsOptions
	visible     []sarifResult
}

func newSARIFFormatter(version string, comparePath tspath.ComparePathsOptions) *sarifFormatter {
	return &sarifFormatter{version: version, comparePath: comparePath}
}

type sarifOrdered struct {
	result sarifResult
	// order breaks ties between visible and suppressed results with the same
	// file, start, and rule: visible diagnostics keep the existing formatter
	// order first, suppressed records follow.
	order int
}

func (f *sarifFormatter) begin(w *bufio.Writer, report Report, _ bool) error {
	header := sarifLog{
		Schema:  sarifSchemaURI,
		Version: sarifVersion,
		Runs:    []sarifRun{{Tool: sarifTool{Driver: f.driver(report)}}},
	}
	encoded, err := json.Marshal(header)
	if err != nil {
		return err
	}
	// The encoded header ends with "}}": one brace closes the run object and
	// one closes the root log. Results and both closures are appended after
	// the buffered diagnostics are merged in finish.
	encoded = encoded[:len(encoded)-2]
	if _, err := w.Write(encoded); err != nil {
		return err
	}
	_, err = w.WriteString(`,"results":[`)
	return err
}

func (f *sarifFormatter) diagnostic(_ *bufio.Writer, view diagnosticView) error {
	f.visible = append(f.visible, sarifResultForView(view))
	return nil
}

func (f *sarifFormatter) finish(w *bufio.Writer, report Report) error {
	rulesByName := sarifRuleIndex(report)

	entries := make([]sarifOrdered, 0, len(f.visible)+len(report.suppressed))
	for _, result := range f.visible {
		entries = append(entries, sarifOrdered{result: result})
	}
	for index := range report.suppressed {
		entries = append(entries, sarifOrdered{
			result: sarifResultForSuppressed(report.suppressed[index], f.comparePath),
			order:  index + 1,
		})
	}

	// Visible and suppressed diagnostics come pre-sorted by the command
	// layer (same file/start order the other formatters use). Merge the two
	// sequences stably on that key so suppression records interleave with
	// the results that surround them.
	sort.SliceStable(entries, func(i, j int) bool {
		return sarifLess(entries[i], entries[j])
	})

	// Assign rule indexes and deterministic fingerprints in emission order.
	// The rule set is name-sorted in the driver, so indexes are stable across
	// runs for the same input.
	fingerprints := newSARIFFingerprintState()
	for index := range entries {
		result := &entries[index].result
		if ruleIndex, ok := rulesByName[result.RuleID]; ok {
			result.RuleIndex = &ruleIndex
		}
		result.Fingerprints = map[string]string{
			sarifPrimaryFP: fingerprints.fingerprint(result),
		}
	}

	for index := range entries {
		if index > 0 {
			if err := w.WriteByte(','); err != nil {
				return err
			}
		}
		encoded, err := json.Marshal(entries[index].result)
		if err != nil {
			return err
		}
		if _, err := w.Write(encoded); err != nil {
			return err
		}
	}
	_, err := w.WriteString("]}]}\n")
	return err
}

func (f *sarifFormatter) driver(report Report) sarifDriver {
	driver := sarifDriver{
		Name:           sarifToolName,
		InformationURI: sarifToolURI,
		Rules:          sarifRules(report),
	}
	if f.version != "" {
		driver.Version = f.version
	}
	return driver
}

// --- SARIF JSON shapes ------------------------------------------------------

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool sarifTool `json:"tool"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version,omitempty"`
	InformationURI string      `json:"informationUri,omitempty"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID                   string                `json:"id"`
	Name                 string                `json:"name,omitempty"`
	ShortDescription     *sarifMessage         `json:"shortDescription,omitempty"`
	HelpURI              string                `json:"helpUri,omitempty"`
	DefaultConfiguration *sarifReportingConfig `json:"defaultConfiguration,omitempty"`
}

type sarifReportingConfig struct {
	Level string `json:"level"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           *sarifRegion          `json:"region,omitempty"`
}

type sarifArtifactLocation struct {
	URI string `json:"uri"`
}

// Region line/column numbers are 1-based. SARIF columns count UTF-16 code
// units, matching the projection's columns. endColumn is exclusive (one past
// the end), so the zero-based end position becomes endColumn = column + 1.
type sarifRegion struct {
	StartLine   int `json:"startLine"`
	StartColumn int `json:"startColumn,omitempty"`
	EndLine     int `json:"endLine,omitempty"`
	EndColumn   int `json:"endColumn,omitempty"`
}

type sarifReplacement struct {
	DeletedRegion   sarifByteRegion    `json:"deletedRegion"`
	InsertedContent *sarifInsertedText `json:"insertedContent,omitempty"`
}

// Fix replacement regions use UTF-8 byte offsets, the same offset space as the
// projected TextRange, so edits round-trip without character-encoding drift.
type sarifByteRegion struct {
	ByteOffset int `json:"byteOffset"`
	ByteLength int `json:"byteLength"`
}

type sarifInsertedText struct {
	Text string `json:"text"`
}

type sarifArtifactChange struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Replacements     []sarifReplacement    `json:"replacements"`
}

type sarifFix struct {
	ArtifactChanges []sarifArtifactChange `json:"artifactChanges"`
}

type sarifSuppression struct {
	Kind     string         `json:"kind"`
	Location *sarifLocation `json:"location,omitempty"`
}

type sarifResult struct {
	RuleID       string             `json:"ruleId"`
	RuleIndex    *int               `json:"ruleIndex,omitempty"`
	Level        string             `json:"level"`
	Message      sarifMessage       `json:"message"`
	Locations    []sarifLocation    `json:"locations"`
	Fingerprints map[string]string  `json:"fingerprints,omitempty"`
	Fixes        []sarifFix         `json:"fixes,omitempty"`
	Suppressions []sarifSuppression `json:"suppressions,omitempty"`

	// sort identity (not serialized)
	uri       string
	startLine int
	startCol  int
}

// --- projection helpers -----------------------------------------------------

func sarifRules(report Report) []sarifRule {
	infos := report.Rules()
	rules := make([]sarifRule, 0, len(infos))
	for _, info := range infos {
		rules = append(rules, sarifRuleForInfo(info))
	}
	sort.Slice(rules, func(i, j int) bool {
		return rules[i].ID < rules[j].ID
	})
	return rules
}

func sarifRuleIndex(report Report) map[string]int {
	rules := sarifRules(report)
	index := make(map[string]int, len(rules))
	for i, rule := range rules {
		index[rule.ID] = i
	}
	return index
}

func sarifRuleForInfo(info RuleInfo) sarifRule {
	rule := sarifRule{
		ID:   info.Name,
		Name: sarifRuleComponentName(info.Name),
	}
	if info.Description != "" {
		rule.ShortDescription = &sarifMessage{Text: info.Description}
	}
	if info.HelpURI != "" {
		rule.HelpURI = info.HelpURI
	}
	if level := sarifLevel(info.DefaultLevel); level != "none" {
		rule.DefaultConfiguration = &sarifReportingConfig{Level: level}
	}
	return rule
}

// sarifRuleComponentName turns a rule ID into the [A-Za-z0-9_.-] component
// name SARIF reserves for the optional `name` property (for example
// @typescript-eslint/no-unused-vars -> TypescriptEslintNoUnusedVars).
func sarifRuleComponentName(ruleID string) string {
	var builder strings.Builder
	capitalize := true
	for _, r := range ruleID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			if capitalize && r >= 'a' && r <= 'z' {
				r -= 'a' - 'A'
			}
			builder.WriteRune(r)
			capitalize = false
			continue
		}
		capitalize = true
	}
	return builder.String()
}

func sarifLevel(severity Severity) string {
	switch severity {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	default:
		return "none"
	}
}

func sarifLocationFor(uri string, start, end Position) sarifLocation {
	return sarifLocation{
		PhysicalLocation: sarifPhysicalLocation{
			ArtifactLocation: sarifArtifactLocation{URI: uri},
			Region: &sarifRegion{
				StartLine:   start.Line + 1,
				StartColumn: start.Column + 1,
				EndLine:     end.Line + 1,
				EndColumn:   end.Column + 1,
			},
		},
	}
}

func sarifResultForView(view diagnosticView) sarifResult {
	start := Position{Line: view.start.line, Column: view.start.column}
	end := Position{Line: view.end.line, Column: view.end.column}
	return newSarifResult(view.raw, view.relativePath, start, end)
}

func sarifResultForSuppressed(suppressed SuppressedDiagnostic, comparePath tspath.ComparePathsOptions) sarifResult {
	diagnostic := suppressed.Diagnostic
	relativePath := tspath.ConvertToRelativePath(diagnostic.FilePath, comparePath)
	result := newSarifResult(diagnostic, relativePath, diagnostic.Start, diagnostic.End)
	result.Suppressions = []sarifSuppression{{
		Kind: "inSource",
		Location: &sarifLocation{
			PhysicalLocation: sarifPhysicalLocation{
				ArtifactLocation: sarifArtifactLocation{URI: result.uri},
				Region: &sarifRegion{
					StartLine:   suppressed.Suppression.Start.Line + 1,
					StartColumn: suppressed.Suppression.Start.Column + 1,
					EndLine:     suppressed.Suppression.End.Line + 1,
					EndColumn:   suppressed.Suppression.End.Column + 1,
				},
			},
		},
	}}
	return result
}

func newSarifResult(diagnostic Diagnostic, relativePath string, start, end Position) sarifResult {
	uri := sarifURI(relativePath)
	result := sarifResult{
		RuleID:  diagnostic.RuleName,
		Level:   sarifLevel(diagnostic.Severity),
		Message: sarifMessage{Text: diagnostic.Message},
		Locations: []sarifLocation{
			sarifLocationFor(uri, start, end),
		},
		uri:       uri,
		startLine: start.Line + 1,
		startCol:  start.Column + 1,
	}
	if len(diagnostic.Fixes) > 0 {
		result.Fixes = []sarifFix{{
			ArtifactChanges: []sarifArtifactChange{{
				ArtifactLocation: sarifArtifactLocation{URI: uri},
				Replacements:     sarifReplacements(diagnostic.Fixes),
			}},
		}}
	}
	return result
}

func sarifReplacements(fixes []Fix) []sarifReplacement {
	replacements := make([]sarifReplacement, 0, len(fixes))
	for _, fix := range fixes {
		replacement := sarifReplacement{
			DeletedRegion: sarifByteRegion{
				ByteOffset: fix.Range.Start,
				ByteLength: fix.Range.End - fix.Range.Start,
			},
		}
		if fix.NewText != "" {
			replacement.InsertedContent = &sarifInsertedText{Text: fix.NewText}
		}
		replacements = append(replacements, replacement)
	}
	return replacements
}

func sarifLess(a, b sarifOrdered) bool {
	if a.result.uri != b.result.uri {
		return a.result.uri < b.result.uri
	}
	if a.result.startLine != b.result.startLine {
		return a.result.startLine < b.result.startLine
	}
	if a.result.startCol != b.result.startCol {
		return a.result.startCol < b.result.startCol
	}
	if a.result.RuleID != b.result.RuleID {
		return a.result.RuleID < b.result.RuleID
	}
	return a.order < b.order
}

// sarifURI renders a POSIX-style relative path as an RFC 3986 relative URI:
// forward slashes are structural and every other path segment is percent
// escaped (spaces, "#", "?", "%", non-ASCII bytes).
func sarifURI(relativePath string) string {
	segments := strings.Split(relativePath, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// --- fingerprints -----------------------------------------------------------

type sarifFingerprintState struct {
	nextSalt map[string]int
	emitted  map[string]struct{}
}

func newSARIFFingerprintState() *sarifFingerprintState {
	return &sarifFingerprintState{
		nextSalt: make(map[string]int),
		emitted:  make(map[string]struct{}),
	}
}

// fingerprint hashes the stable identity of one result. The tuple matches the
// GitLab formatter's identity (path, rule, message, and the 1-based range),
// using SHA-256 as SARIF conventionally does. Duplicate tuples receive
// deterministic :1, :2, ... salts in emission order.
func (s *sarifFingerprintState) fingerprint(result *sarifResult) string {
	region := result.Locations[0].PhysicalLocation.Region
	input := fmt.Sprintf(
		"%s:%s:%s:%d:%d:%d:%d",
		result.uri,
		result.RuleID,
		result.Message.Text,
		region.StartLine,
		region.StartColumn,
		region.EndLine,
		region.EndColumn,
	)
	salt := s.nextSalt[input]
	for {
		value := input
		if salt > 0 {
			value += ":" + strconv.Itoa(salt)
		}
		salt++
		s.nextSalt[input] = salt

		sum := sha256.Sum256([]byte(value))
		fingerprint := hex.EncodeToString(sum[:])
		if _, exists := s.emitted[fingerprint]; exists {
			continue
		}
		s.emitted[fingerprint] = struct{}{}
		return fingerprint
	}
}
