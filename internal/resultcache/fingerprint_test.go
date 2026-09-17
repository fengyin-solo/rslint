package resultcache_test

import (
	"sort"
	"testing"

	"github.com/microsoft/TypeScript/tsc/shim/bundled"
	"github.com/microsoft/TypeScript/tsc/shim/core"
	"github.com/microsoft/TypeScript/tsc/shim/tspath"
	"github.com/microsoft/TypeScript/tsc/shim/vfs/osvfs"
	rslintconfig "github.com/web-infra-dev/rslint/internal/config"
	lintprogram "github.com/web-infra-dev/rslint/internal/program"
	"github.com/web-infra-dev/rslint/internal/resultcache"
	"github.com/web-infra-dev/rslint/internal/utils"
)

func TestConfigFingerprintDeterminism(t *testing.T) {
	// Identical logical configs assembled with different Go map iteration
	// orders must hash equally; encoding/json sorts map keys.
	first := rslintconfig.RslintConfig{{
		Files: []string{"**/*.ts"},
		Rules: rslintconfig.Rules{"eqeqeq": "error", "curly": "warn"},
		Settings: rslintconfig.Settings{
			"pluginA": map[string]any{"opt": true},
		},
	}}
	second := rslintconfig.RslintConfig{{
		Files: []string{"**/*.ts"},
		Rules: rslintconfig.Rules{"curly": "warn", "eqeqeq": "error"},
		Settings: rslintconfig.Settings{
			"pluginA": map[string]any{"opt": true},
		},
	}}
	h1, err := resultcache.ConfigFingerprint(first)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := resultcache.ConfigFingerprint(second)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("equivalent configs hashed differently: %s vs %s", h1, h2)
	}
}

func TestConfigFingerprintInvalidation(t *testing.T) {
	base := rslintconfig.RslintConfig{{
		Files:   []string{"**/*.ts"},
		Ignores: []string{"**/dist/**"},
		Rules:   rslintconfig.Rules{"eqeqeq": []any{"error", "always"}},
		Settings: rslintconfig.Settings{"shared": map[string]any{"flag": true}},
		LanguageOptions: &rslintconfig.LanguageOptions{
			Raw: map[string]any{"ecmaVersion": float64(2022)},
		},
		Plugins: []string{"react"},
	}}
	baseHash, err := resultcache.ConfigFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(cfg rslintconfig.RslintConfig) rslintconfig.RslintConfig
	}{
		{"rule severity", func(cfg rslintconfig.RslintConfig) rslintconfig.RslintConfig {
			cfg[0].Rules = rslintconfig.Rules{"eqeqeq": "warn"}
			return cfg
		}},
		{"rule options", func(cfg rslintconfig.RslintConfig) rslintconfig.RslintConfig {
			cfg[0].Rules = rslintconfig.Rules{"eqeqeq": []any{"error", "smart"}}
			return cfg
		}},
		{"rule set", func(cfg rslintconfig.RslintConfig) rslintconfig.RslintConfig {
			cfg[0].Rules = rslintconfig.Rules{"eqeqeq": []any{"error", "always"}, "curly": "error"}
			return cfg
		}},
		{"authored ignores", func(cfg rslintconfig.RslintConfig) rslintconfig.RslintConfig {
			cfg[0].Ignores = []string{"**/dist/**", "**/build/**"}
			return cfg
		}},
		{"gitignore projection", func(cfg rslintconfig.RslintConfig) rslintconfig.RslintConfig {
			projected := rslintconfig.ConfigEntry{Ignores: []string{"**/generated/**"}}
			return append(rslintconfig.RslintConfig{projected}, cfg...)
		}},
		{"settings", func(cfg rslintconfig.RslintConfig) rslintconfig.RslintConfig {
			cfg[0].Settings = rslintconfig.Settings{"shared": map[string]any{"flag": false}}
			return cfg
		}},
		{"language options", func(cfg rslintconfig.RslintConfig) rslintconfig.RslintConfig {
			cfg[0].LanguageOptions = &rslintconfig.LanguageOptions{
				Raw: map[string]any{"ecmaVersion": float64(2023)},
			}
			return cfg
		}},
		{"plugins", func(cfg rslintconfig.RslintConfig) rslintconfig.RslintConfig {
			cfg[0].Plugins = []string{"react", "jest"}
			return cfg
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := tc.mutate(cloneConfig(base))
			hash, err := resultcache.ConfigFingerprint(changed)
			if err != nil {
				t.Fatal(err)
			}
			if hash == baseHash {
				t.Fatalf("%s change did not invalidate the config fingerprint", tc.name)
			}
		})
	}
}

func cloneConfig(cfg rslintconfig.RslintConfig) rslintconfig.RslintConfig {
	cloned := append(rslintconfig.RslintConfig(nil), cfg...)
	return cloned
}

func buildUniverseProgram(t *testing.T, root string, files map[string]string) *lintprogram.Program {
	t.Helper()
	fs := utils.NewOverlayVFS(bundled.WrapFS(osvfs.FS()), files)
	host := utils.CreateCompilerHost(root, fs)
	roots := make([]string, 0, len(files))
	for name := range files {
		roots = append(roots, name)
	}
	// Stable root order for the fixture; the hash sorts anyway.
	sort.Strings(roots)
	raw, err := utils.CreateProgramFromOptions(true, &core.CompilerOptions{
		Module: core.ModuleKindESNext,
	}, roots, host)
	if err != nil {
		t.Fatalf("CreateProgramFromOptions: %v", err)
	}
	return lintprogram.NewFromCompiler(raw)
}

func TestProgramUniverseHash(t *testing.T) {
	const root = "/resultcache-universe-test"
	aPath := tspath.ResolvePath(root, "a.ts")
	bPath := tspath.ResolvePath(root, "b.ts")
	original := map[string]string{
		aPath: `import "./b"; export const a = 1;`,
		bPath: `export const b = 1;`,
	}
	first := buildUniverseProgram(t, root, original)
	h1, err := resultcache.ProgramUniverseHash(first)
	if err != nil {
		t.Fatal(err)
	}
	second := buildUniverseProgram(t, root, map[string]string{
		aPath: `import "./b"; export const a = 1;`,
		bPath: `export const b = 1;`,
	})
	h2, err := resultcache.ProgramUniverseHash(second)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("identical universes hashed differently: %s vs %s", h1, h2)
	}

	// The target file a.ts is unchanged, but a universe member moved: the
	// hash MUST change or type-aware/import-graph results could go stale.
	changed := buildUniverseProgram(t, root, map[string]string{
		aPath: `import "./b"; export const a = 1;`,
		bPath: `export const b = 2;`,
	})
	h3, err := resultcache.ProgramUniverseHash(changed)
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Fatal("changing an imported universe member did not invalidate the universe hash")
	}

	// A target-content-only change likewise flips the universe hash.
	targetChanged := buildUniverseProgram(t, root, map[string]string{
		aPath: `import "./b"; export const a = 99;`,
		bPath: `export const b = 1;`,
	})
	h4, err := resultcache.ProgramUniverseHash(targetChanged)
	if err != nil {
		t.Fatal(err)
	}
	if h4 == h1 {
		t.Fatal("changing a root target did not invalidate the universe hash")
	}

	// The hash is stable across repeated calls on the same Program.
	h1Again, _ := resultcache.ProgramUniverseHash(first)
	if h1 != h1Again {
		t.Fatal("repeated universe hashing was unstable")
	}
}
