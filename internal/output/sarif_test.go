package output

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/microsoft/TypeScript/tsc/shim/tspath"
)

type sarifTestLog struct {
	Schema  string         `json:"$schema"`
	Version string         `json:"version"`
	Runs    []sarifTestRun `json:"runs"`
}

type sarifTestRun struct {
	Tool struct {
		Driver struct {
			Name           string          `json:"name"`
			Version        string          `json:"version"`
			InformationURI string          `json:"informationUri"`
			Rules          []sarifTestRule `json:"rules"`
		} `json:"driver"`
	} `json:"tool"`
	Results []sarifTestResult `json:"results"`
}

type sarifTestRule struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	ShortDescription *struct {
		Text string `json:"text"`
	} `json:"shortDescription"`
	HelpURI              string `json:"helpUri"`
	DefaultConfiguration *struct {
		Level string `json:"level"`
	} `json:"defaultConfiguration"`
}

type sarifTestResult struct {
	RuleID       string              `json:"ruleId"`
	RuleIndex    *int                `json:"ruleIndex"`
	Level        string              `json:"level"`
	Message      sarifMessage        `json:"message"`
	Locations    []sarifLocation     `json:"locations"`
	Fingerprints map[string]string   `json:"fingerprints"`
	Fixes        []sarifTestFix      `json:"fixes"`
	Suppressions []sarifTestSuppress `json:"suppressions"`
}

type sarifTestFix struct {
	ArtifactChanges []struct {
		ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
		Replacements     []struct {
			DeletedRegion   sarifByteRegion    `json:"deletedRegion"`
			InsertedContent *sarifInsertedText `json:"insertedContent"`
		} `json:"replacements"`
	} `json:"artifactChanges"`
}

type sarifTestSuppress struct {
	Kind     string         `json:"kind"`
	Location *sarifLocation `json:"location"`
}

func renderSARIFForTest(t *testing.T, report Report, options Options) sarifTestLog {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, report, options); err != nil {
		t.Fatalf("Render SARIF: %v", err)
	}
	var log sarifTestLog
	if err := json.Unmarshal(buf.Bytes(), &log); err != nil {
		t.Fatalf("SARIF output is not valid JSON: %v\n%s", err, buf.String())
	}
	if !strings.HasSuffix(buf.String(), "}\n") {
		t.Fatalf("SARIF output should end with a newline: %q", buf.String())
	}
	return log
}

func sarifTestPaths(t *testing.T) tspath.ComparePathsOptions {
	t.Helper()
	return tspath.ComparePathsOptions{
		CurrentDirectory:          t.TempDir(),
		UseCaseSensitiveFileNames: true,
	}
}

func sarifTestReport(rules []RuleInfo, diagnostics []Diagnostic, suppressed []SuppressedDiagnostic) Report {
	return NewReport(ModeLint, diagnostics, suppressed, rules, Counts{}, nil, Outcome{Kind: OutcomePassed})
}

func TestSARIFEnvelopeAndRuleMetadata(t *testing.T) {
	paths := sarifTestPaths(t)
	rules := []RuleInfo{
		{Name: "zzz-rule", Description: "Zed rule", HelpURI: "https://rslint.rs/rules/eslint/zzz-rule", DefaultLevel: SeverityWarning},
		{Name: "@typescript-eslint/no-explicit-any", Description: "Ban any", HelpURI: "https://rslint.rs/rules/typescript-eslint/no-explicit-any", DefaultLevel: SeverityError},
		{Name: "no-doc-rule", DefaultLevel: SeverityError},
	}
	diagnostics := []Diagnostic{{
		RuleName: "@typescript-eslint/no-explicit-any",
		FilePath: filepath.Join(paths.CurrentDirectory, "index.ts"),
		Start:    Position{Line: 0, Column: 0},
		End:      Position{Line: 0, Column: 3},
		Message:  "no any",
		Severity: SeverityError,
	}}
	log := renderSARIFForTest(t, sarifTestReport(rules, diagnostics, nil), Options{
		Format: FormatSARIF, ComparePaths: paths, ToolVersion: "3.1.0",
	})

	if log.Version != "2.1.0" || log.Schema != "https://json.schemastore.org/sarif-2.1.0.json" {
		t.Fatalf("unexpected SARIF envelope: %+v", log)
	}
	if len(log.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(log.Runs))
	}
	driver := log.Runs[0].Tool.Driver
	if driver.Name != "rslint" || driver.Version != "3.1.0" || driver.InformationURI != "https://rslint.rs" {
		t.Fatalf("unexpected driver: %+v", driver)
	}

	// Rules are sorted by id for stable indexes regardless of input order.
	gotIDs := []string{driver.Rules[0].ID, driver.Rules[1].ID, driver.Rules[2].ID}
	wantIDs := []string{
		"@typescript-eslint/no-explicit-any",
		"no-doc-rule",
		"zzz-rule",
	}
	for index := range wantIDs {
		if gotIDs[index] != wantIDs[index] {
			t.Fatalf("rule order = %v, want %v", gotIDs, wantIDs)
		}
	}
	tsRule := driver.Rules[0]
	if tsRule.Name != "TypescriptEslintNoExplicitAny" {
		t.Fatalf("component rule name = %q", tsRule.Name)
	}
	if tsRule.ShortDescription == nil || tsRule.ShortDescription.Text != "Ban any" {
		t.Fatalf("missing rule description: %+v", tsRule)
	}
	if tsRule.HelpURI != "https://rslint.rs/rules/typescript-eslint/no-explicit-any" ||
		tsRule.DefaultConfiguration == nil || tsRule.DefaultConfiguration.Level != "error" {
		t.Fatalf("unexpected typescript rule metadata: %+v", tsRule)
	}
	noDoc := driver.Rules[1]
	if noDoc.ShortDescription != nil || noDoc.HelpURI != "" ||
		noDoc.DefaultConfiguration == nil || noDoc.DefaultConfiguration.Level != "error" {
		t.Fatalf("rule without docs should omit docs but keep level: %+v", noDoc)
	}
	warnRule := driver.Rules[2]
	if warnRule.DefaultConfiguration == nil || warnRule.DefaultConfiguration.Level != "warning" {
		t.Fatalf("warning level = %+v", warnRule.DefaultConfiguration)
	}

	results := log.Runs[0].Results
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	result := results[0]
	if result.RuleID != "@typescript-eslint/no-explicit-any" || result.Level != "error" {
		t.Fatalf("unexpected result identity: %+v", result)
	}
	if result.RuleIndex == nil || *result.RuleIndex != 0 {
		t.Fatalf("ruleIndex = %v, want 0", result.RuleIndex)
	}
}

func TestSARIFMultilineRegion(t *testing.T) {
	paths := sarifTestPaths(t)
	// "a\nbb\nccc" — range covers "a" through the final "ccc": start 0:0, end 2:3.
	diagnostics := []Diagnostic{{
		RuleName: "multi",
		FilePath: filepath.Join(paths.CurrentDirectory, "src", "multi.ts"),
		Start:    Position{Line: 0, Column: 0},
		End:      Position{Line: 2, Column: 3},
		Message:  "spans lines",
		Severity: SeverityError,
	}}
	log := renderSARIFForTest(t, sarifTestReport(nil, diagnostics, nil), Options{
		Format: FormatSARIF, ComparePaths: paths,
	})
	region := log.Runs[0].Results[0].Locations[0].PhysicalLocation.Region
	if region.StartLine != 1 || region.StartColumn != 1 || region.EndLine != 3 || region.EndColumn != 4 {
		t.Fatalf("multiline region = %+v, want {1 1 3 4}", region)
	}
}

func TestSARIFSuppressionRecord(t *testing.T) {
	paths := sarifTestPaths(t)
	suppressed := []SuppressedDiagnostic{{
		Diagnostic: Diagnostic{
			RuleName: "no-var",
			FilePath: filepath.Join(paths.CurrentDirectory, "a.js"),
			Start:    Position{Line: 2, Column: 0},
			End:      Position{Line: 2, Column: 3},
			Message:  "no var",
			Severity: SeverityError,
		},
		// Directive comment sits on the preceding line, columns 0..22.
		Suppression: Suppression{
			Start: Position{Line: 1, Column: 0},
			End:   Position{Line: 1, Column: 22},
		},
	}}
	log := renderSARIFForTest(t, sarifTestReport(nil, nil, suppressed), Options{
		Format: FormatSARIF, ComparePaths: paths,
	})
	results := log.Runs[0].Results
	if len(results) != 1 {
		t.Fatalf("results = %d, want the suppressed finding", len(results))
	}
	result := results[0]
	if result.Level != "error" {
		t.Fatalf("suppressed result level = %q, keeps its severity", result.Level)
	}
	if len(result.Suppressions) != 1 {
		t.Fatalf("suppressions = %+v", result.Suppressions)
	}
	suppression := result.Suppressions[0]
	if suppression.Kind != "inSource" || suppression.Location == nil {
		t.Fatalf("unexpected suppression: %+v", suppression)
	}
	region := suppression.Location.PhysicalLocation.Region
	if region.StartLine != 2 || region.StartColumn != 1 || region.EndLine != 2 || region.EndColumn != 23 {
		t.Fatalf("directive region = %+v, want the comment's 1-based range", region)
	}
	if suppression.Location.PhysicalLocation.ArtifactLocation.URI != "a.js" {
		t.Fatalf("suppression uri = %q", suppression.Location.PhysicalLocation.ArtifactLocation.URI)
	}
}

func TestSARIFFixesIncludeReplacementContent(t *testing.T) {
	paths := sarifTestPaths(t)
	diagnostics := []Diagnostic{{
		RuleName: "no-var",
		FilePath: filepath.Join(paths.CurrentDirectory, "a.js"),
		Start:    Position{Line: 0, Column: 0},
		End:      Position{Line: 0, Column: 3},
		Range:    TextRange{Start: 0, End: 3},
		Message:  "no var",
		Severity: SeverityError,
		Fixes: []Fix{
			{Range: TextRange{Start: 0, End: 3}, NewText: "let"},
			{Range: TextRange{Start: 10, End: 10}, NewText: ""},
		},
	}}
	log := renderSARIFForTest(t, sarifTestReport(nil, diagnostics, nil), Options{
		Format: FormatSARIF, ComparePaths: paths,
	})
	changes := log.Runs[0].Results[0].Fixes[0].ArtifactChanges
	if len(changes) != 1 || changes[0].ArtifactLocation.URI != "a.js" {
		t.Fatalf("artifact changes = %+v", changes)
	}
	replacements := changes[0].Replacements
	if len(replacements) != 2 {
		t.Fatalf("replacements = %+v", replacements)
	}
	first := replacements[0]
	if first.DeletedRegion.ByteOffset != 0 || first.DeletedRegion.ByteLength != 3 ||
		first.InsertedContent == nil || first.InsertedContent.Text != "let" {
		t.Fatalf("replacement = %+v", first)
	}
	second := replacements[1]
	if second.DeletedRegion.ByteOffset != 10 || second.DeletedRegion.ByteLength != 0 ||
		second.InsertedContent != nil {
		t.Fatalf("empty insertion replacement = %+v", second)
	}
}

func TestSARIFFingerprintsStableAndUnique(t *testing.T) {
	paths := sarifTestPaths(t)
	build := func() Report {
		diagnostics := []Diagnostic{
			{
				RuleName: "dup", Message: "same",
				FilePath: filepath.Join(paths.CurrentDirectory, "a.js"),
				Start: Position{Line: 0, Column: 0}, End: Position{Line: 0, Column: 1}, Severity: SeverityError,
			},
			{
				RuleName: "dup", Message: "same",
				FilePath: filepath.Join(paths.CurrentDirectory, "a.js"),
				Start: Position{Line: 0, Column: 0}, End: Position{Line: 0, Column: 1}, Severity: SeverityError,
			},
			{
				RuleName: "other", Message: "same",
				FilePath: filepath.Join(paths.CurrentDirectory, "a.js"),
				Start: Position{Line: 0, Column: 0}, End: Position{Line: 0, Column: 1}, Severity: SeverityError,
			},
		}
		return sarifTestReport(nil, diagnostics, nil)
	}
	options := Options{Format: FormatSARIF, ComparePaths: paths}
	first := renderSARIFForTest(t, build(), options)
	second := renderSARIFForTest(t, build(), options)

	fingerprints := func(log sarifTestLog) []string {
		got := make([]string, len(log.Runs[0].Results))
		for index, result := range log.Runs[0].Results {
			got[index] = result.Fingerprints["primaryFingerprint"]
		}
		return got
	}
	firstFP := fingerprints(first)
	secondFP := fingerprints(second)
	if len(firstFP) != 3 {
		t.Fatalf("expected 3 fingerprints, got %v", firstFP)
	}
	hexSHA256 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for index := range firstFP {
		if firstFP[index] != secondFP[index] {
			t.Fatalf("fingerprints changed between runs: %v vs %v", firstFP, secondFP)
		}
		if !hexSHA256.MatchString(firstFP[index]) {
			t.Fatalf("fingerprint %q is not a sha256 hex value", firstFP[index])
		}
	}
	if firstFP[0] == firstFP[1] {
		t.Fatalf("identical duplicate results share a fingerprint: %v", firstFP)
	}
}

func TestSARIFResultOrderMatchesFileAndStart(t *testing.T) {
	paths := sarifTestPaths(t)
	diagnostics := []Diagnostic{
		{
			RuleName: "b-rule", Message: "later file",
			FilePath: filepath.Join(paths.CurrentDirectory, "b.js"),
			Start: Position{Line: 0, Column: 0}, End: Position{Line: 0, Column: 1}, Severity: SeverityError,
		},
		{
			RuleName: "a-rule", Message: "same start, later rule",
			FilePath: filepath.Join(paths.CurrentDirectory, "a.js"),
			Start: Position{Line: 1, Column: 0}, End: Position{Line: 1, Column: 1}, Severity: SeverityWarning,
		},
		{
			RuleName: "a-rule", Message: "first",
			FilePath: filepath.Join(paths.CurrentDirectory, "a.js"),
			Start: Position{Line: 0, Column: 0}, End: Position{Line: 0, Column: 1}, Severity: SeverityError,
		},
	}
	log := renderSARIFForTest(t, sarifTestReport(nil, diagnostics, nil), Options{
		Format: FormatSARIF, ComparePaths: paths,
	})
	var order []string
	for _, result := range log.Runs[0].Results {
		order = append(order, result.Message.Text)
	}
	want := []string{"first", "same start, later rule", "later file"}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("result order = %v, want %v", order, want)
		}
	}
}

func TestSARIFSpecialCharactersInFilenameAreEscaped(t *testing.T) {
	paths := sarifTestPaths(t)
	// Spaces, "#", "?", "%", and non-ASCII characters all need URI escaping;
	// the path does not have to exist for relative-path projection.
	filePath := filepath.Join(paths.CurrentDirectory, "weird dir", "#100% 文件?.ts")
	diagnostics := []Diagnostic{{
		RuleName: "rule", Message: "msg",
		FilePath: filePath,
		Start:    Position{Line: 0, Column: 0}, End: Position{Line: 0, Column: 1},
		Severity: SeverityError,
	}}
	log := renderSARIFForTest(t, sarifTestReport(nil, diagnostics, nil), Options{
		Format: FormatSARIF, ComparePaths: paths,
	})
	uri := log.Runs[0].Results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI
	if strings.ContainsAny(uri, " #?") {
		t.Fatalf("uri %q kept a raw reserved character", uri)
	}
	if strings.Contains(uri, "文") {
		t.Fatalf("uri %q kept a raw non-ASCII character", uri)
	}
	// Every percent must introduce a hex escape (including the escaped "%").
	expanded := regexp.MustCompile(`%[0-9A-Fa-f]{2}`).ReplaceAllString(uri, "")
	if strings.Contains(expanded, "%") {
		t.Fatalf("uri %q has a percent sign outside an escape", uri)
	}
	if !strings.Contains(uri, "weird%20dir/") || !strings.Contains(uri, "%23100%25") {
		t.Fatalf("uri %q missing expected escaped segments", uri)
	}
	if strings.Contains(uri, "\\") {
		t.Fatalf("uri %q contains a platform separator", uri)
	}
}

func TestSARIFEmptyReportIsValidDocument(t *testing.T) {
	log := renderSARIFForTest(t, sarifTestReport(nil, nil, nil), Options{Format: FormatSARIF})
	if log.Version != "2.1.0" || len(log.Runs) != 1 {
		t.Fatalf("empty report envelope = %+v", log)
	}
	if len(log.Runs[0].Tool.Driver.Rules) != 0 || len(log.Runs[0].Results) != 0 {
		t.Fatalf("empty report should have empty rules and results: %+v", log.Runs[0])
	}
}

func TestSARIFQuietKeepsSuppressionRecords(t *testing.T) {
	paths := sarifTestPaths(t)
	visible := []Diagnostic{{
		RuleName: "warn", Message: "hidden by quiet",
		FilePath: filepath.Join(paths.CurrentDirectory, "a.js"),
		Start: Position{Line: 0, Column: 0}, End: Position{Line: 0, Column: 1},
		Severity: SeverityWarning,
	}}
	suppressed := []SuppressedDiagnostic{{
		Diagnostic: Diagnostic{
			RuleName: "err", Message: "suppressed but present",
			FilePath: filepath.Join(paths.CurrentDirectory, "b.js"),
			Start: Position{Line: 0, Column: 0}, End: Position{Line: 0, Column: 1},
			Severity: SeverityError,
		},
		Suppression: Suppression{Start: Position{Line: 0, Column: 0}, End: Position{Line: 0, Column: 5}},
	}}
	log := renderSARIFForTest(t, sarifTestReport(nil, visible, suppressed), Options{
		Format: FormatSARIF, ComparePaths: paths, Quiet: true,
	})
	results := log.Runs[0].Results
	if len(results) != 1 || results[0].RuleID != "err" || len(results[0].Suppressions) != 1 {
		t.Fatalf("quiet should filter visible warnings but keep suppression records: %+v", results)
	}
}
