package gogent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxAgentsContextBytes caps the combined AGENTS.md content injected into the
// system prompt so a deep tree of large files cannot blow the context budget.
const maxAgentsContextBytes = 32 * 1024

// agentsDoc is a discovered AGENTS.md file and its content.
type agentsDoc struct {
	path    string
	content string
}

// discoverAgentsDocs collects AGENTS.md instruction files relevant to the
// workspace: a global ~/.gogent/AGENTS.md first, then every AGENTS.md found
// walking from the filesystem root down to the workspace root (so the
// nearest/most-specific file comes last and can refine the more general ones).
func discoverAgentsDocs(workspaceRoot, configDir string) []agentsDoc {
	var docs []agentsDoc
	seen := make(map[string]bool)

	add := func(path string) {
		clean := filepath.Clean(path)
		if seen[clean] {
			return
		}
		seen[clean] = true
		data, err := os.ReadFile(clean)
		if err != nil {
			return
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			return
		}
		docs = append(docs, agentsDoc{path: clean, content: text})
	}

	if configDir != "" {
		add(filepath.Join(configDir, "AGENTS.md"))
	}

	// Build the ancestor chain from the workspace root up to the filesystem root,
	// then add them outermost-first.
	var chain []string
	dir := filepath.Clean(workspaceRoot)
	for {
		chain = append(chain, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for i := len(chain) - 1; i >= 0; i-- {
		add(filepath.Join(chain[i], "AGENTS.md"))
	}

	return docs
}

// buildSystemContext assembles the extra system-prompt context for a session:
// project AGENTS.md instructions followed by the index of available skills. It
// is installed as a session's SystemContextProvider, so it is re-evaluated each
// loop and reflects runtime skill (de)activation.
func (g *Gogent) buildSystemContext() string {
	var b strings.Builder

	if g.agentsContext != "" {
		b.WriteString(g.agentsContext)
	}

	if g.skills != nil {
		active := g.skills.ListActiveSkills()
		if len(active) > 0 {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString("## Available skills\n")
			b.WriteString("You have access to these skills. Call the `skill` tool with the skill's `name` to load its full instructions before performing a task it covers.\n")
			for _, sk := range active {
				b.WriteString(fmt.Sprintf("- %s: %s\n", sk.Name, sk.Description))
			}
		}
	}

	return strings.TrimSpace(b.String())
}

// renderAgentsContext concatenates discovered AGENTS.md docs into a single
// system-prompt section with per-file headers, truncating at the size cap.
func renderAgentsContext(docs []agentsDoc) string {
	if len(docs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Project instructions (AGENTS.md)\n")
	b.WriteString("Follow these repository conventions. More specific (deeper) files refine earlier ones.\n")
	for _, d := range docs {
		section := fmt.Sprintf("\n### %s\n%s\n", d.path, d.content)
		if b.Len()+len(section) > maxAgentsContextBytes {
			b.WriteString("\n[additional AGENTS.md content truncated]\n")
			break
		}
		b.WriteString(section)
	}
	return strings.TrimSpace(b.String())
}
