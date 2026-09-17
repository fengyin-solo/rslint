package rules

import "embed"

// RuleDocsFS embeds every core rule's markdown documentation, laid out as
// <rule_dir>/<rule_dir>.md (snake_case directories matching kebab-case rule
// names). Presentation layers that render rule metadata (for example the
// SARIF driver) read descriptions from here instead of hitting the disk.
//
//go:embed */*.md
var RuleDocsFS embed.FS
