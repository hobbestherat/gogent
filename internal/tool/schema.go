package tool

// unsupportedSchemaKeys are JSON Schema keywords that strict tool-schema
// validators reject or silently ignore. OpenAI's function parameters tolerate a
// permissive superset, but Anthropic's input_schema and Gemini's
// functionDeclarations are stricter and refuse referencing/composition keywords
// and defaults. Stripping them once at registration yields a single normalized
// schema that serializes cleanly to every provider (see NormalizeSchema).
var unsupportedSchemaKeys = map[string]bool{
	"$schema":     true,
	"$id":         true,
	"$ref":        true,
	"$defs":       true,
	"definitions": true,
	"default":     true,
	"allOf":       true,
}

// NormalizeSchema returns a portable copy of a tool input schema. It guarantees
// a top-level object schema carrying a properties map (Anthropic and Gemini both
// require these even for a no-argument tool) and recursively strips the keywords
// strict providers reject (see unsupportedSchemaKeys). A nil or non-object
// schema becomes the empty object schema {"type":"object","properties":{}}.
//
// Normalization runs once, at Register time, so the same schema is reused for
// validation, display, and every provider's wire format. The original value is
// not mutated; a fresh map is returned.
func NormalizeSchema(schema interface{}) map[string]interface{} {
	m, ok := schema.(map[string]interface{})
	if !ok {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	out := normalizeNode(m)
	// The root of a tool schema is always an object, whatever the author wrote.
	out["type"] = "object"
	if _, ok := out["properties"]; !ok {
		out["properties"] = map[string]interface{}{}
	}
	return out
}

// normalizeNode deep-copies one schema object, dropping unsupported keywords and
// recursing into nested schemas. The "properties" map is handled specially: its
// keys are caller-chosen property names (which may legitimately collide with a
// schema keyword such as "default"), so only their subschema values are
// normalized, never the names themselves.
func normalizeNode(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		if unsupportedSchemaKeys[k] {
			continue
		}
		if k == "properties" {
			if props, ok := v.(map[string]interface{}); ok {
				np := make(map[string]interface{}, len(props))
				for name, sub := range props {
					np[name] = normalizeValue(sub)
				}
				out[k] = np
				continue
			}
		}
		out[k] = normalizeValue(v)
	}
	return out
}

// normalizeValue normalizes a schema-bearing value, recursing through nested
// objects (e.g. an "items" subschema) and arrays while leaving scalars and
// non-schema slices (such as a []string "required" or "enum") untouched.
func normalizeValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return normalizeNode(t)
	case []interface{}:
		arr := make([]interface{}, len(t))
		for i, e := range t {
			arr[i] = normalizeValue(e)
		}
		return arr
	default:
		return v
	}
}
