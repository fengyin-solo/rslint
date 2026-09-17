// Package plugins owns build-time assets shared by the bundled ESLint plugin
// packages below it. The package itself only exposes embedded metadata; rule
// implementations live in the per-plugin subpackages.
package plugins

import "embed"

// PluginSourceFS embeds each plugin's plugin.go so the PLUGIN_NAME constant
// (the rule-id namespace users configure, for example "@typescript-eslint")
// can be resolved without disk access.
//
//go:embed */plugin.go
var PluginSourceFS embed.FS

// RuleDocsFS embeds plugin rule documentation, laid out as
// <plugin_dir>/rules/<rule_dir>/<rule_dir>.md, mirroring the website manifest
// generator's layout.
//
//go:embed */rules/*/*.md
var RuleDocsFS embed.FS
