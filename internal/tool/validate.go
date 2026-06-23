package tool

import (
	"fmt"
	"strings"
)

// validateArgs validates tool arguments against the tool's JSON Schema (the
// InputSchema advertised to the model). It enforces the constructs this codebase
// actually declares — property "type", "properties", and "required" — so that
// malformed model output (missing keys, wrong types) is rejected before it can
// reach a tool's ad-hoc type assertions and panic or silently misbehave.
//
// It is a deliberately small subset of JSON Schema rather than a full validator:
// it covers every schema the tools declare, keeps the dependency footprint to
// the standard library, and stays lenient by default (unknown properties are
// allowed, matching JSON Schema's default "additionalProperties").
func validateArgs(args map[string]interface{}, schema interface{}) error {
	if args == nil {
		return fmt.Errorf("args cannot be nil")
	}
	schemaMap, ok := schema.(map[string]interface{})
	if !ok {
		// No object schema to enforce against; nothing to validate.
		return nil
	}
	return validateValue(args, schemaMap, "args")
}

// validateValue checks a single value against its (sub)schema at the given
// dotted path, recursing into object properties.
func validateValue(value, schema interface{}, path string) error {
	schemaMap, ok := schema.(map[string]interface{})
	if !ok {
		return nil
	}
	if typ, ok := schemaMap["type"].(string); ok {
		if err := validateType(value, typ, path); err != nil {
			return err
		}
		if typ == "object" {
			if err := validateObject(value, schemaMap, path); err != nil {
				return err
			}
		}
	}
	// Array items: walk each element against the "items" subschema so constraints
	// nested inside arrays (e.g. todo.status under todos.items.properties) are
	// enforced too. A schema may omit "type" (the todos schema deliberately
	// permits array-or-null), so this is gated on the value actually being an
	// array rather than on a declared "array" type.
	if items, ok := schemaMap["items"].(map[string]interface{}); ok {
		if arr, ok := value.([]interface{}); ok {
			for i, elem := range arr {
				if err := validateValue(elem, items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	// An "enum" subschema constrains the value to a fixed allowed set. Enforce it
	// generically for any property that declares one, so advertised enums
	// (git.operation, grep.output_mode, todo.status, and any future field) are
	// rejected at the gate rather than slipping through to a tool-specific error.
	if allowed := enumValues(schemaMap); allowed != nil {
		if err := validateEnum(value, allowed, path); err != nil {
			return err
		}
	}
	return nil
}

// validateEnum reports an error when value is not among the allowed enum
// entries. The error names the field path and the full allowed set, e.g.
// `args.operation: value must be one of [status diff log], got "rebase"`.
func validateEnum(value interface{}, allowed []interface{}, path string) error {
	for _, a := range allowed {
		if enumEqual(value, a) {
			return nil
		}
	}
	return fmt.Errorf("%s: value must be one of %s, got %s", path, formatEnumAllowed(allowed), formatEnumGot(value))
}

// enumEqual reports whether a provided value matches an allowed enum entry.
//
// String members are matched case-insensitively: the gate's job is to reject
// values the tool could never honor, not to be stricter than the tool itself.
// The enum-typed fields tolerate loose case downstream (e.g. todo.status is run
// through NormalizeTodoStatus, which lower-cases before matching), so rejecting
// "IN_PROGRESS" at the gate would block input the tool would otherwise accept,
// while genuinely out-of-set values ("blocked", "rebase") are still rejected.
//
// For non-string members it compares directly, then falls back to comparing
// string forms so a JSON number (float64) matches a Go integer literal.
func enumEqual(value, allowed interface{}) bool {
	if value == allowed {
		return true
	}
	if sv, ok := value.(string); ok {
		if sa, ok := allowed.(string); ok {
			return strings.EqualFold(sv, sa)
		}
	}
	return fmt.Sprintf("%v", value) == fmt.Sprintf("%v", allowed)
}

// formatEnumAllowed renders the allowed set as `[a b c]` (unquoted, space
// separated), matching the issue's uniform error format.
func formatEnumAllowed(allowed []interface{}) string {
	parts := make([]string, len(allowed))
	for i, a := range allowed {
		parts[i] = fmt.Sprintf("%v", a)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// formatEnumGot renders the offending value for the error message, quoting
// strings so an empty or whitespace value is visible.
func formatEnumGot(value interface{}) string {
	if s, ok := value.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%v", value)
}

// validateObject enforces required keys and the declared property schemas on an
// object value. It assumes value already passed an "object" type check.
func validateObject(value interface{}, schemaMap map[string]interface{}, path string) error {
	obj, ok := value.(map[string]interface{})
	if !ok {
		return nil // a type mismatch is already reported by validateType
	}

	for _, key := range requiredKeys(schemaMap) {
		if _, present := obj[key]; !present {
			return fmt.Errorf("%s: missing required property %q", path, key)
		}
	}

	props, _ := schemaMap["properties"].(map[string]interface{})
	for name, sub := range props {
		field, present := obj[name]
		if !present {
			continue
		}
		if err := validateValue(field, sub, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

// requiredKeys returns the "required" entries of a schema, accepting either a
// []string (as written in the Go schema literals in this codebase) or a
// []interface{} (as decoded from JSON).
func requiredKeys(schemaMap map[string]interface{}) []string {
	switch v := schemaMap["required"].(type) {
	case []string:
		return v
	case []interface{}:
		keys := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				keys = append(keys, s)
			}
		}
		return keys
	}
	return nil
}

// enumValues returns the "enum" entries of a property subschema, accepting
// either a []string (as written in the Go schema literals in this codebase) or a
// []interface{} (as decoded from JSON), mirroring requiredKeys. It returns nil
// when the subschema declares no enum, so the check is a no-op for such fields.
func enumValues(schemaMap map[string]interface{}) []interface{} {
	switch v := schemaMap["enum"].(type) {
	case []interface{}:
		return v
	case []string:
		out := make([]interface{}, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out
	}
	return nil
}

// validateType checks that value matches the named JSON Schema type, returning a
// descriptive error otherwise. The Go values it inspects are those produced by
// encoding/json when decoding into interface{} (strings, float64 numbers, bools,
// []interface{} arrays, map[string]interface{} objects); numeric checks also
// accept Go integer types defensively so non-JSON arg sources are not rejected.
func validateType(value interface{}, typ, path string) error {
	if typ == "" || isType(value, typ) {
		return nil
	}
	return fmt.Errorf("%s: expected %s, got %s", path, typ, jsonTypeName(value))
}

// isType reports whether value satisfies the JSON Schema type named by typ.
func isType(value interface{}, typ string) bool {
	switch typ {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		switch value.(type) {
		case float32, float64, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		}
		return false
	case "integer":
		switch n := value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			return n == float64(int64(n))
		case float32:
			return n == float32(int32(n))
		}
		return false
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]interface{})
		return ok
	case "object":
		_, ok := value.(map[string]interface{})
		return ok
	}
	// An unrecognized schema type is not enforced.
	return true
}

// jsonTypeName returns a human-readable JSON type name for a decoded value, for
// use in validation error messages.
func jsonTypeName(value interface{}) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "number"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	}
	return fmt.Sprintf("%T", value)
}
