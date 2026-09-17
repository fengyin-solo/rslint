package resultcache_test

import (
	"testing"

	"github.com/web-infra-dev/rslint/internal/resultcache"
)

func validKeyInput() resultcache.KeyInput {
	return resultcache.KeyInput{
		TargetPath:   "/repo/src/foo.ts",
		ContentHash:  resultcache.ContentHash("const a = 1;"),
		ConfigHash:   "config-hash",
		UniverseHash: "universe-hash",
	}
}

func TestBuildKeyDeterministicAndDistinct(t *testing.T) {
	first, err := resultcache.BuildKey(validKeyInput(), "3.1.0")
	if err != nil {
		t.Fatalf("build key: %v", err)
	}
	second, err := resultcache.BuildKey(validKeyInput(), "3.1.0")
	if err != nil {
		t.Fatalf("rebuild key: %v", err)
	}
	if first.String() != second.String() {
		t.Fatalf("identical inputs produced different keys: %s vs %s", first, second)
	}
	if first.IsZero() || len(first.String()) != 32 {
		t.Fatalf("unexpected key shape: %q", first.String())
	}

	cases := []struct {
		name   string
		mutate func(*resultcache.KeyInput)
	}{
		{"target path", func(in *resultcache.KeyInput) { in.TargetPath = "/repo/src/bar.ts" }},
		{"content", func(in *resultcache.KeyInput) { in.ContentHash = resultcache.ContentHash("const a = 2;") }},
		{"config", func(in *resultcache.KeyInput) { in.ConfigHash = "other-config" }},
		{"universe", func(in *resultcache.KeyInput) { in.UniverseHash = "other-universe" }},
		{"mode", func(in *resultcache.KeyInput) { in.Mode = "other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := validKeyInput()
			tc.mutate(&input)
			other, err := resultcache.BuildKey(input, "3.1.0")
			if err != nil {
				t.Fatalf("build key: %v", err)
			}
			if other.String() == first.String() {
				t.Fatalf("changing %s did not invalidate the key", tc.name)
			}
		})
	}
}

func TestBuildKeyIncludesRslintVersion(t *testing.T) {
	first, _ := resultcache.BuildKey(validKeyInput(), "3.1.0")
	second, err := resultcache.BuildKey(validKeyInput(), "3.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if first.String() == second.String() {
		t.Fatal("rslint version change must invalidate keys")
	}
}

func TestBuildKeyRejectsIncompleteInput(t *testing.T) {
	removeField := func(field string) resultcache.KeyInput {
		input := validKeyInput()
		switch field {
		case "target":
			input.TargetPath = ""
		case "content":
			input.ContentHash = ""
		case "config":
			input.ConfigHash = ""
		case "universe":
			input.UniverseHash = ""
		}
		return input
	}
	for _, field := range []string{"target", "content", "config", "universe"} {
		key, err := resultcache.BuildKey(removeField(field), "3.1.0")
		if err == nil || !key.IsZero() {
			t.Fatalf("missing %s must fail key construction, got key=%q err=%v", field, key.String(), err)
		}
	}
}

func TestContentHashDistinguishesBOM(t *testing.T) {
	noBOM := resultcache.ContentHash("const a = 1;")
	withBOM := resultcache.ContentHash(string(rune(0xFEFF)) + "const a = 1;")
	if noBOM == withBOM {
		t.Fatal("BOM presence must change the content hash")
	}
	if resultcache.ContentHash("x") == resultcache.ContentHash("y") {
		t.Fatal("different content produced same hash")
	}
}
