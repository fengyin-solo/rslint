// Package rulemd serves rule documentation metadata embedded at build time.
// It maps rule ids (for example "no-var" and "@typescript-eslint/no-explicit-any")
// to a short plain-text description and the website documentation URL.
package rulemd

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"sync"

	pluginassets "github.com/web-infra-dev/rslint/internal/plugins"
	corerules "github.com/web-infra-dev/rslint/internal/rules"
)

const documentationBaseURI = "https://rslint.rs/rules/"

// Registry is an immutable index of rule id → description.
type Registry struct {
	descriptions map[string]string
}

var (
	defaultRegistry     *Registry
	defaultRegistryOnce sync.Once
)

// Default returns the process-wide registry built from embedded docs.
func Default() *Registry {
	defaultRegistryOnce.Do(func() {
		defaultRegistry = build()
	})
	return defaultRegistry
}

func build() *Registry {
	descriptions := make(map[string]string)

	// Core rules: internal/rules/<dir>/<dir>.md, rule id is the snake_case
	// directory name converted to kebab-case.
	if err := fs.WalkDir(corerules.RuleDocsFS, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			return nil
		}
		ruleName := strings.ReplaceAll(parts[0], "_", "-")
		if description := loadDescription(corerules.RuleDocsFS, path); description != "" {
			descriptions[ruleName] = description
		}
		return nil
	}); err != nil {
		panic(err)
	}

	// Plugin rules: internal/plugins/<plugin>/rules/<dir>/<dir>.md, where
	// <plugin>'s rule-id namespace is the PLUGIN_NAME constant in plugin.go.
	prefixes := pluginPrefixes()
	if err := fs.WalkDir(pluginassets.RuleDocsFS, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		parts := strings.Split(path, "/")
		if len(parts) != 5 || parts[1] != "rules" {
			return nil
		}
		prefix, known := prefixes[parts[0]]
		if !known {
			return nil
		}
		ruleName := strings.ReplaceAll(parts[2], "_", "-")
		id := prefix + "/" + ruleName
		if description := loadDescription(pluginassets.RuleDocsFS, path); description != "" {
			descriptions[id] = description
		}
		return nil
	}); err != nil {
		panic(err)
	}

	return &Registry{descriptions: descriptions}
}

var pluginNamePattern = regexp.MustCompile(`PLUGIN_NAME\s*=\s*"([^"]+)"`)

func pluginPrefixes() map[string]string {
	prefixes := make(map[string]string)
	entries, err := fs.ReadDir(pluginassets.PluginSourceFS, ".")
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		source, err := fs.ReadFile(pluginassets.PluginSourceFS, entry.Name()+"/plugin.go")
		if err != nil {
			continue
		}
		match := pluginNamePattern.FindSubmatch(source)
		if match == nil {
			continue
		}
		prefixes[entry.Name()] = string(match[1])
	}
	return prefixes
}

func loadDescription(fileSystem fs.FS, path string) string {
	content, err := fs.ReadFile(fileSystem, path)
	if err != nil {
		return ""
	}
	return extractDescription(string(content))
}

// Description returns the rule's short plain-text description, or "" when no
// embedded documentation is known.
func (r *Registry) Description(ruleName string) string {
	if r == nil {
		return ""
	}
	return r.descriptions[ruleName]
}

// RuleNames returns every rule id with an embedded description, sorted.
func (r *Registry) RuleNames() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.descriptions))
	for name := range r.descriptions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// HelpURI returns the website documentation URL for a rule id. Core rules
// live under /rules/eslint/; scoped rules use their namespace with any "@"
// stripped (for example @typescript-eslint/no-explicit-any ->
// /rules/typescript-eslint/no-explicit-any).
func HelpURI(ruleName string) string {
	if scope, rest, found := strings.Cut(ruleName, "/"); found {
		return documentationBaseURI + strings.TrimPrefix(scope, "@") + "/" + rest
	}
	return documentationBaseURI + "eslint/" + ruleName
}

// extractDescription derives a one-paragraph plain-text summary from a rule
// doc: the first paragraph under "## Rule Details" when present, otherwise the
// first paragraph after the H1 title.
func extractDescription(markdown string) string {
	text := strings.ReplaceAll(markdown, "\r\n", "\n")
	if newline := strings.IndexByte(text, '\n'); newline >= 0 {
		text = text[newline+1:]
	}
	if idx := strings.Index(text, "## Rule Details"); idx >= 0 {
		text = text[idx+len("## Rule Details"):]
	}
	for _, block := range strings.Split(text, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" || strings.HasPrefix(block, "#") || strings.HasPrefix(block, "```") {
			continue
		}
		if description := markdownParagraphToText(block); description != "" {
			return description
		}
	}
	return ""
}

var (
	mdLinkPattern     = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdHTMLEntity      = regexp.MustCompile(`<[^>]+>`)
	mdEntityPattern   = regexp.MustCompile(`&(?:amp|lt|gt|quot|#39);`)
	mdWhitespace      = regexp.MustCompile(`\s+`)
	mdInlineMarkup, _ = regexp.Compile(`[*_` + "`" + `~]+`)
)

func markdownParagraphToText(block string) string {
	// A paragraph can soft-wrap across lines; join before stripping markup.
	text := strings.Join(strings.Split(block, "\n"), " ")
	text = mdLinkPattern.ReplaceAllString(text, "$1")
	text = mdHTMLEntity.ReplaceAllString(text, "")
	text = mdInlineMarkup.ReplaceAllString(text, "")
	text = mdEntityPattern.ReplaceAllStringFunc(text, func(entity string) string {
		switch entity {
		case "&amp;":
			return "&"
		case "&lt;":
			return "<"
		case "&gt;":
			return ">"
		case "&quot;":
			return `"`
		case "&#39;":
			return "'"
		default:
			return entity
		}
	})
	text = mdWhitespace.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}
