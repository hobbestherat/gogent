package diagnostics

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestParse exercises the compiler/linter output parser across the line shapes
// `go vet`, `go build` and friends emit. It is table-driven and subprocess-free.
func TestParse(t *testing.T) {
	warnAll := regexp.MustCompile(".*") // every message is a warning

	cases := []struct {
		name   string
		output string
		warn   *regexp.Regexp
		want   []Diagnostic
		capped bool
	}{
		{
			name:   "go build plain with column",
			output: "./main.go:6:14: undefined: undef\n",
			want:   []Diagnostic{{Path: "main.go", Line: 6, Column: 14, Severity: SeverityError, Message: "undefined: undef"}},
		},
		{
			name:   "go vet prefixed line",
			output: "# example.com/p\nvet: ./main.go:6:14: undefined: undef\n",
			want:   []Diagnostic{{Path: "main.go", Line: 6, Column: 14, Severity: SeverityError, Message: "undefined: undef"}},
		},
		{
			name:   "line without column",
			output: "main.go:12: missing argument\n",
			want:   []Diagnostic{{Path: "main.go", Line: 12, Column: 0, Severity: SeverityError, Message: "missing argument"}},
		},
		{
			name:   "package header and exit status are skipped",
			output: "# example.com/p\n# [example.com/p]\nexit status 2\nFAIL\texample.com/p [build failed]\n",
			want:   nil,
		},
		{
			name:   "source and caret context lines are skipped",
			output: "./main.go:6:14: undefined: undef\n\tfmt.Println(undef)\n\t             ^\n",
			want:   []Diagnostic{{Path: "main.go", Line: 6, Column: 14, Severity: SeverityError, Message: "undefined: undef"}},
		},
		{
			name:   "absolute path kept as-is",
			output: "/home/u/proj/main.go:3:1: syntax error\n",
			want:   []Diagnostic{{Path: "/home/u/proj/main.go", Line: 3, Column: 1, Severity: SeverityError, Message: "syntax error"}},
		},
		{
			name:   "warning pattern downgrades severity",
			output: "./main.go:9:2: printf: foo (warn)\n./main.go:10:2: undefined: bar\n",
			warn:   warnAll,
			want: []Diagnostic{
				{Path: "main.go", Line: 9, Column: 2, Severity: SeverityWarning, Message: "printf: foo (warn)"},
				{Path: "main.go", Line: 10, Column: 2, Severity: SeverityWarning, Message: "undefined: bar"},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, capped := parse(c.output, c.warn)
			if capped != c.capped {
				t.Errorf("capped: got %v want %v", capped, c.capped)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("diagnostics:\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// TestParseCapsDiagnostics verifies the result is bounded so a broken build
// cannot flood the model with hundreds of findings.
func TestParseCapsDiagnostics(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxDiagnostics+5; i++ {
		b.WriteString("main.go:1:1: error\n")
	}
	got, capped := parse(b.String(), nil)
	if !capped {
		t.Error("capped: got false, want true past the cap")
	}
	if len(got) != maxDiagnostics {
		t.Errorf("len: got %d want %d", len(got), maxDiagnostics)
	}
}

// --- Run integration tests against a real, tiny Go module ---

// writeGoModule writes a minimal module into dir with the given main.go source.
func writeGoModule(t *testing.T, dir, mainSrc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/diagtest\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
}

const cleanMain = "package main\n\nfunc main() {}\n"

const brokenMain = `package main

import "fmt"

func main() {
	fmt.Println(undef)
}
`

// skipIfGoMissing skips the test when the go toolchain is unavailable, since the
// default command and these fixtures rely on it.
func skipIfGoMissing(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}
}

func TestRunCleanGoModule(t *testing.T) {
	skipIfGoMissing(t)
	dir := t.TempDir()
	writeGoModule(t, dir, cleanMain)

	rep, err := Run(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.OK {
		t.Errorf("OK: got false, want true (clean module); output=%q exit=%d", rep.Output, rep.ExitCode)
	}
	if rep.Count != 0 {
		t.Errorf("Count: got %d, want 0; diagnostics=%+v", rep.Count, rep.Diagnostics)
	}
	if rep.Timeout {
		t.Error("Timeout should be false on a clean run")
	}
}

func TestRunBrokenGoModule(t *testing.T) {
	skipIfGoMissing(t)
	dir := t.TempDir()
	writeGoModule(t, dir, brokenMain)

	rep, err := Run(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.OK {
		t.Error("OK: got true, want false (broken module)")
	}
	if rep.ExitCode == 0 {
		t.Error("ExitCode: got 0, want non-zero for a failing build")
	}
	if rep.Count == 0 {
		t.Fatalf("Count: got 0, want at least one diagnostic; output=%q", rep.Output)
	}
	d := rep.Diagnostics[0]
	if d.Path != "main.go" {
		t.Errorf("Path: got %q want main.go", d.Path)
	}
	if d.Line != 6 {
		t.Errorf("Line: got %d want 6", d.Line)
	}
	if d.Severity != SeverityError {
		t.Errorf("Severity: got %q want %q", d.Severity, SeverityError)
	}
	if !strings.Contains(d.Message, "undef") {
		t.Errorf("Message: got %q, want it to mention undef", d.Message)
	}
}

// TestRunDefaultCommand verifies an empty command falls back to the Go default
// (`go vet ./...`) and the report records exactly what ran.
func TestRunDefaultCommand(t *testing.T) {
	skipIfGoMissing(t)
	dir := t.TempDir()
	writeGoModule(t, dir, cleanMain)

	rep, err := Run(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Command) == 0 || rep.Command[0] != DefaultCommand[0] {
		t.Errorf("Command: got %v, want the go vet default %v", rep.Command, DefaultCommand)
	}
}

// TestRunCustomCommand runs a fixed binary that exits cleanly with no
// diagnostics, confirming a configured command is honored verbatim.
func TestRunCustomCommand(t *testing.T) {
	skipIfGoMissing(t)
	rep, err := Run(Config{Command: []string{"go", "version"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.OK {
		t.Errorf("OK: got false, want true for `go version`; output=%q", rep.Output)
	}
	if rep.Count != 0 {
		t.Errorf("Count: got %d, want 0 (go version emits no diagnostics)", rep.Count)
	}
}

// TestRunMissingBinary verifies a command whose binary is absent surfaces a
// clear error rather than a misleading clean run.
func TestRunMissingBinary(t *testing.T) {
	_, err := Run(Config{Command: []string{"definitely-not-a-real-binary-xyz"}})
	if err == nil {
		t.Fatal("want error for missing binary, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error: got %q, want it to mention 'not found'", err.Error())
	}
}

// TestRunInvalidWarningPattern verifies a bad warning_pattern regex is reported.
func TestRunInvalidWarningPattern(t *testing.T) {
	skipIfGoMissing(t)
	_, err := Run(Config{Command: []string{"go", "version"}, WarningPattern: "(?P<bad"})
	if err == nil {
		t.Fatal("want error for invalid warning pattern, got nil")
	}
	if !strings.Contains(err.Error(), "warning_pattern") {
		t.Errorf("error: got %q, want it to mention warning_pattern", err.Error())
	}
}

// TestRunTimeout verifies the deadline fires and is reported via Timeout, not as
// a launch error. It skips when `sleep` is unavailable.
func TestRunTimeout(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not installed")
	}
	start := time.Now()
	rep, err := Run(Config{Command: []string{"sleep", "2"}, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Run: %v (want nil on timeout)", err)
	}
	if !rep.Timeout {
		t.Error("Timeout: got false, want true for an expired deadline")
	}
	if rep.OK {
		t.Error("OK: got true, want false on timeout")
	}
	if !rep.Truncated {
		t.Error("Truncated: got false, want true (run was incomplete)")
	}
	// Must return well before the sleep would have finished naturally.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("did not honor the deadline: elapsed=%v", elapsed)
	}
}
