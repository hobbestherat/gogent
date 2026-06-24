//go:build !windows

package daemon

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestIssue358WindowsCrossCompileBuildsWholeRepo(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"GOOS=windows",
		"GOARCH=amd64",
	)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("GOOS=windows GOARCH=amd64 go build ./... failed: %v\n%s", err, out.String())
	}
}
