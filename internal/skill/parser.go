package skill

import (
	"strings"
)

// parseFrontmatter extracts description from YAML frontmatter
func parseFrontmatter(content string) (string, error) {
	// Simple parser for YAML frontmatter
	// Format:
	// ---
	// name: skill-name
	// description: skill description
	// ---

	lines := strings.Split(content, "\n")
	inFrontmatter := false
	description := ""

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "---" {
			if !inFrontmatter {
				inFrontmatter = true
			} else {
				break // End of frontmatter
			}
			continue
		}

		if inFrontmatter {
			if strings.HasPrefix(line, "description:") {
				desc := strings.TrimSpace(strings.TrimPrefix(line, "description:"))
				description = strings.Trim(desc, "\"'")
			}
		}
	}

	if description == "" {
		return "", &ParseError{Message: "missing description in frontmatter"}
	}

	return description, nil
}

// ParseError represents an error parsing skill files
type ParseError struct {
	Message string
}

func (e *ParseError) Error() string {
	return e.Message
}
