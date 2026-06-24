package command

import (
	"fmt"
	"regexp"
	"strings"

	"gogent/internal/config"
)

// Custom slash commands (issue #403): template expansion, name/parameter
// validation and the single source of truth for the reserved built-in names the
// editor must not let a custom command shadow.

var (
	// commandNameRe is the syntactic rule for a custom command name: lowercase,
	// digit- or hyphen-separated, starting alphanumeric (opencode-style).
	commandNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// paramNameRe is the rule for a parameter name: a C-style identifier, so it
	// maps cleanly onto $name / ${name} placeholders.
	paramNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	// braceRefRe matches the ${name} placeholder form (unambiguous boundaries).
	braceRefRe = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)
	// bareRefRe matches the $name placeholder form (greedy identifier).
	bareRefRe = regexp.MustCompile(`\$([a-zA-Z_][a-zA-Z0-9_]*)`)
	// namedArgRe matches a name=value invocation argument; the rest is positional.
	namedArgRe = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)=(.*)$`)
)

// ValidCommandName reports whether name is a syntactically valid custom command
// name (it does not check collisions — that is the service's job).
func ValidCommandName(name string) bool { return commandNameRe.MatchString(name) }

// ValidParamName reports whether name is a syntactically valid parameter name.
func ValidParamName(name string) bool { return paramNameRe.MatchString(name) }

// ReservedNames returns the set of built-in command names a custom command may
// not shadow (issue #403). It is the single source of truth shared by the editor
// (collision check) and the runtime: the client-side slash commands handled in
// the TUI, the backend CommandRegistry built-ins, and the file tools registered
// as commands. Returned as a fresh map so callers may freely augment it (e.g.
// the service adds the existing custom-command names).
func ReservedNames() map[string]bool {
	reserved := make(map[string]bool, 18)
	for _, n := range []string{
		// Client-side slash commands (ui/tui handleSlashCommand).
		"undo", "rewind", "fork", "plan", "yolo", "act", "stop",
		"clearqueue", "goal", "markdown", "thinking", "watcher",
		// Backend CommandRegistry built-ins (RegisterBuiltInCommands).
		"calc", "echo", "help",
		// File tools registered as commands (RegisterFileTools).
		"read", "write", "edit",
	} {
		reserved[n] = true
	}
	return reserved
}

// Expand binds args to the declared params and substitutes $name / ${name}
// placeholders in template, returning the prompt to send to the agent.
//
// Binding (in declaration order): positional args fill params left to right, then
// name=value args override by name. A missing required parameter is a descriptive
// error (the command must not be sent); a missing optional parameter falls back to
// its Default (empty if none). A placeholder whose name matches no declared
// parameter is left as literal text (the editor warns about it at save time).
func Expand(template string, params []config.CommandParam, args []string) (string, error) {
	positionals, named := splitArgs(args)

	values := make(map[string]string, len(params))
	for i, p := range params {
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

	return substitute(template, values), nil
}

// ValidateTemplate returns one warning per distinct placeholder reference in
// template that does not match any declared parameter (those references expand to
// literal text at runtime). The editor surfaces these at save time.
func ValidateTemplate(template string, params []config.CommandParam) []string {
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
		warnings = append(warnings, fmt.Sprintf("$%s has no matching parameter (left as literal text)", name))
	}
	for _, m := range braceRefRe.FindAllStringSubmatch(template, -1) {
		note(m[1])
	}
	for _, m := range bareRefRe.FindAllStringSubmatch(template, -1) {
		note(m[1])
	}
	return warnings
}

// splitArgs partitions invocation args into ordered positionals and a name=value
// map. A token is named only when it matches name=value with a valid identifier;
// every other token (including a bare value containing '=') is positional.
func splitArgs(args []string) (positionals []string, named map[string]string) {
	named = make(map[string]string)
	for _, a := range args {
		if m := namedArgRe.FindStringSubmatch(a); m != nil {
			named[m[1]] = m[2]
			continue
		}
		positionals = append(positionals, a)
	}
	return positionals, named
}

// substitute replaces ${name} then $name placeholders using values, leaving any
// reference with no entry untouched (literal). The brace form is handled first so
// ${name}Type binds "name", not "nameType".
func substitute(template string, values map[string]string) string {
	out := braceRefRe.ReplaceAllStringFunc(template, func(match string) string {
		name := braceRefRe.FindStringSubmatch(match)[1]
		if v, ok := values[name]; ok {
			return v
		}
		return match
	})
	out = bareRefRe.ReplaceAllStringFunc(out, func(match string) string {
		name := bareRefRe.FindStringSubmatch(match)[1]
		if v, ok := values[name]; ok {
			return v
		}
		return match
	})
	return out
}
