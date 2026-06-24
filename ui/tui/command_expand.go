package ui

import (
	"fmt"
	"regexp"
	"strings"
)

// UI-side custom-command expansion (issue #403). It mirrors
// internal/command.Expand exactly but operates on the decoupled ui/tui DTOs, so
// the dispatch path and the editor's live preview share one implementation
// without ui/tui importing internal/. The backend keeps its own copy as the
// source of truth for the (future) daemon API and is independently tested.

var (
	cmdBraceRefRe = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)
	cmdBareRefRe  = regexp.MustCompile(`\$([a-zA-Z_][a-zA-Z0-9_]*)`)
	cmdNamedArgRe = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)=(.*)$`)
)

// expandTemplate binds args to def's parameters (positional fills in declaration
// order, then name=value overrides) and substitutes $name / ${name} placeholders.
// A missing required parameter is an error (the command must not be sent); a
// missing optional one uses its default; an unknown placeholder is left literal.
func expandTemplate(def CommandDef, args []string) (string, error) {
	positionals, named := splitCommandArgs(args)
	values := make(map[string]string, len(def.Parameters))
	for i, p := range def.Parameters {
		v := p.Default
		if i < len(positionals) {
			v = positionals[i]
		}
		if nv, ok := named[p.Name]; ok {
			v = nv
		}
		if p.Required && strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("missing required parameter %q", p.Name)
		}
		values[p.Name] = v
	}
	return substituteCommandRefs(def.Template, values), nil
}

// templateWarnings returns one message per placeholder in template with no
// matching parameter (those expand to literal text). The editor shows these at
// save time so authors notice typos without being blocked.
func templateWarnings(template string, params []CommandParam) []string {
	declared := make(map[string]bool, len(params))
	for _, p := range params {
		declared[p.Name] = true
	}
	seen := make(map[string]bool)
	var warnings []string
	note := func(name string) {
		if declared[name] || seen[name] {
			return
		}
		seen[name] = true
		warnings = append(warnings, fmt.Sprintf("$%s has no matching parameter", name))
	}
	for _, m := range cmdBraceRefRe.FindAllStringSubmatch(template, -1) {
		note(m[1])
	}
	for _, m := range cmdBareRefRe.FindAllStringSubmatch(template, -1) {
		note(m[1])
	}
	return warnings
}

func splitCommandArgs(args []string) (positionals []string, named map[string]string) {
	named = make(map[string]string)
	for _, a := range args {
		if m := cmdNamedArgRe.FindStringSubmatch(a); m != nil {
			named[m[1]] = m[2]
			continue
		}
		positionals = append(positionals, a)
	}
	return positionals, named
}

func substituteCommandRefs(template string, values map[string]string) string {
	out := cmdBraceRefRe.ReplaceAllStringFunc(template, func(match string) string {
		name := cmdBraceRefRe.FindStringSubmatch(match)[1]
		if v, ok := values[name]; ok {
			return v
		}
		return match
	})
	out = cmdBareRefRe.ReplaceAllStringFunc(out, func(match string) string {
		name := cmdBareRefRe.FindStringSubmatch(match)[1]
		if v, ok := values[name]; ok {
			return v
		}
		return match
	})
	return out
}
