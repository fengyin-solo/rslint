package resultcache_test

import (
	"encoding/json"
	"testing"

	"github.com/microsoft/TypeScript/tsc/shim/core"
	"github.com/web-infra-dev/rslint/internal/resultcache"
	"github.com/web-infra-dev/rslint/internal/rule"
)

func TestDiagnosticRoundTrip(t *testing.T) {
	fixes := []rule.RuleFix{
		{Text: "==", Range: core.NewTextRange(10, 12)},
	}
	suggestions := []rule.RuleSuggestion{
		{
			Message: rule.RuleMessage{
				Id:          "useEqEqEq",
				Description: "Replace with ===",
				Data:        map[string]string{"operator": "=="},
			},
			FixesArr: []rule.RuleFix{{Text: "===", Range: core.NewTextRange(10, 12)}},
		},
	}
	input := []rule.RuleDiagnostic{
		{
			Range:    core.NewTextRange(5, 9),
			RuleName: "eqeqeq",
			Message: rule.RuleMessage{
				Id:          "unexpected",
				Description: "Expected '===' and instead saw '=='.",
				Data:        map[string]string{"actual": "==", "expected": "==="},
			},
			FixesPtr:      &fixes,
			Suggestions:  &suggestions,
			Severity:     rule.SeverityWarning,
			Origin:       rule.DiagnosticOriginLint,
			PreFormatted: true,
		},
		{
			Range:    core.NewTextRange(0, 3),
			RuleName: "syntax-error",
			Message:  rule.RuleMessage{Description: "',' expected."},
			Severity: rule.SeverityError,
			Origin:   rule.DiagnosticOriginTypeScript,
		},
	}

	encoded, err := resultcache.EncodeDiagnostics(input)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) != 2 {
		t.Fatalf("want 2 records, got %d", len(encoded))
	}
	decoded, err := resultcache.DecodeDiagnostics(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("want 2 diagnostics, got %d", len(decoded))
	}
	first := decoded[0]
	if first.SourceFile != nil {
		t.Fatal("decoded diagnostic must not carry a SourceFile")
	}
	if first.RuleName != "eqeqeq" || first.Severity != rule.SeverityWarning ||
		first.Origin != rule.DiagnosticOriginLint || !first.PreFormatted {
		t.Fatalf("scalar fields mismatch: %+v", first)
	}
	if first.Range.Pos() != 5 || first.Range.End() != 9 {
		t.Fatalf("range mismatch: %d-%d", first.Range.Pos(), first.Range.End())
	}
	if first.Message.Id != "unexpected" || first.Message.Description == "" ||
		first.Message.Data["expected"] != "===" || len(first.Message.Data) != 2 {
		t.Fatalf("message mismatch: %+v", first.Message)
	}
	gotFixes := first.Fixes()
	if len(gotFixes) != 1 || gotFixes[0].Text != "==" ||
		gotFixes[0].Range.Pos() != 10 || gotFixes[0].Range.End() != 12 {
		t.Fatalf("fixes mismatch: %+v", gotFixes)
	}
	gotSuggestions := *first.Suggestions
	if len(gotSuggestions) != 1 || gotSuggestions[0].Message.Id != "useEqEqEq" ||
		gotSuggestions[0].FixesArr[0].Text != "===" || gotSuggestions[0].Message.Data["operator"] != "==" {
		t.Fatalf("suggestions mismatch: %+v", gotSuggestions)
	}

	second := decoded[1]
	if second.Origin != rule.DiagnosticOriginTypeScript || second.Severity != rule.SeverityError {
		t.Fatalf("second diagnostic scalars mismatch: %+v", second)
	}
	if second.FixesPtr != nil || second.Suggestions != nil {
		t.Fatal("absent optional artifacts must stay absent")
	}
}

func TestEncodeEmptyProducesNil(t *testing.T) {
	encoded, err := resultcache.EncodeDiagnostics(nil)
	if err != nil || encoded != nil {
		t.Fatalf("empty encode = %v, %v", encoded, err)
	}
	decoded, err := resultcache.DecodeDiagnostics(nil)
	if err != nil || decoded != nil {
		t.Fatalf("empty decode = %v, %v", decoded, err)
	}
}

func TestDecodeRejectsMalformedEntry(t *testing.T) {
	good, err := resultcache.EncodeDiagnostics([]rule.RuleDiagnostic{{
		Range:    core.NewTextRange(0, 1),
		RuleName: "x",
		Severity: rule.SeverityError,
		Message:  rule.RuleMessage{Description: "bad"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"garbage":       []byte("{not-json"),
		"wrong version": []byte(`{"v":99,"rule":"x","severity":1,"message":{"description":"d"},"start":0,"end":1}`),
		"inverted range": []byte(`{"v":1,"rule":"x","severity":1,"message":{"description":"d"},"start":5,"end":1}`),
		"bad fix range": []byte(`{"v":1,"rule":"x","severity":1,"message":{"description":"d"},"start":0,"end":1,"fixes":[{"text":"","start":9,"end":2}]}`),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			records := append(append([]json.RawMessage{}, good...), json.RawMessage(bad))
			if _, err := resultcache.DecodeDiagnostics(records); err == nil {
				t.Fatal("malformed entry must fail whole-entry decode, never replay a partial set")
			}
		})
	}
}
