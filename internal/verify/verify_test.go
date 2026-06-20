package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestParse exercises the `go test` output parser across the line shapes the
// runner emits. It is table-driven and subprocess-free; the leading indent on
// log lines is a tab, exactly as `go test` writes it.
func TestParse(t *testing.T) {
	cases := []struct {
		name       string
		output     string
		want       []Failure
		okPkgs     int
		failedPkgs int
		noTestPkgs int
	}{
		{
			name:   "clean suite, one package with no tests",
			output: "ok  \texample.com/pkg\t0.002s\n?   \texample.com/cmd\t[no test files]\n",
			okPkgs: 1, noTestPkgs: 1,
		},
		{
			name:       "single failing test",
			output:     "--- FAIL: TestAdd (0.00s)\n\tmain_test.go:10: expected 5, got 4\nFAIL\nFAIL\texample.com/pkg\t0.005s\nFAIL\n",
			want:       []Failure{{Package: "example.com/pkg", Test: "TestAdd", Message: "main_test.go:10: expected 5, got 4"}},
			failedPkgs: 1,
		},
		{
			name:   "two failing tests in one package",
			output: "--- FAIL: TestAdd (0.00s)\n\ta_test.go:10: bad\n--- FAIL: TestSub (0.00s)\n\ta_test.go:20: bad2\nFAIL\nFAIL\texample.com/pkg\t0.005s\n",
			want: []Failure{
				{Package: "example.com/pkg", Test: "TestAdd", Message: "a_test.go:10: bad"},
				{Package: "example.com/pkg", Test: "TestSub", Message: "a_test.go:20: bad2"},
			},
			failedPkgs: 1,
		},
		{
			name:       "build failure reported as a package-level failure",
			output:     "# example.com/pkg\n./main.go:6:14: undefined: undef\nFAIL\texample.com/pkg [build failed]\n",
			want:       []Failure{{Package: "example.com/pkg", Test: "", Message: "./main.go:6:14: undefined: undef"}},
			failedPkgs: 1,
		},
		{
			name:       "panic output folds into the failing test's message",
			output:     "--- FAIL: TestPanic (0.00s)\npanic: runtime error: index out of range [5] with length 3\n\ngoroutine 5 [running]:\nexample.com/pkg.TestPanic(...)\n\t/home/u/pkg/x_test.go:12 +0x3a\nFAIL\nFAIL\texample.com/pkg\t0.001s\n",
			want:       []Failure{{Package: "example.com/pkg", Test: "TestPanic", Message: "panic: runtime error: index out of range [5] with length 3\ngoroutine 5 [running]:\nexample.com/pkg.TestPanic(...)\n/home/u/pkg/x_test.go:12 +0x3a"}},
			failedPkgs: 1,
		},
		{
			name:       "subtest name kept, skip marker is not a failure",
			output:     "--- FAIL: TestParent/sub_name (0.00s)\n\tx_test.go:5: nested failure\n--- SKIP: TestSkipped (0.00s)\nFAIL\nFAIL\texample.com/pkg\t0.005s\n",
			want:       []Failure{{Package: "example.com/pkg", Test: "TestParent/sub_name", Message: "x_test.go:5: nested failure"}},
			failedPkgs: 1,
		},
		{
			name:   "failing test whose package summary was truncated keeps an empty package",
			output: "--- FAIL: TestAdd (0.00s)\n\ta_test.go:1: bad\n",
			want:   []Failure{{Package: "", Test: "TestAdd", Message: "a_test.go:1: bad"}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parse(c.output)
			if got.okPkgs != c.okPkgs {
				t.Errorf("okPkgs: got %d want %d", got.okPkgs, c.okPkgs)
			}
			if got.failedPkgs != c.failedPkgs {
				t.Errorf("failedPkgs: got %d want %d", got.failedPkgs, c.failedPkgs)
			}
			if got.noTestPkgs != c.noTestPkgs {
				t.Errorf("noTestPkgs: got %d want %d", got.noTestPkgs, c.noTestPkgs)
			}
			if !reflect.DeepEqual(got.failures, c.want) {
				t.Errorf("failures:\n got %+v\nwant %+v", got.failures, c.want)
			}
		})
	}
}

// TestParseCapsFailures verifies the result is bounded so a broken suite cannot
// flood the model with hundreds of findings.
func TestParseCapsFailures(t *testing.T) {
	var b strings.Builder
	b.WriteString("# example.com/pkg\n")
	for i := 0; i < maxFailures+5; i++ {
		b.WriteString("a.go:1:1: error\n")
	}
	b.WriteString("FAIL\texample.com/pkg [build failed]\n")
	got := parse(b.String())
	if !got.capped {
		t.Error("capped: got false, want true past the cap")
	}
	if len(got.failures) != maxFailures {
		t.Errorf("len: got %d want %d", len(got.failures), maxFailures)
	}
	if got.failedPkgs != 1 {
		t.Errorf("failedPkgs: got %d want 1", got.failedPkgs)
	}
}

// TestPeelElapsed confirms trailing durations are stripped while plain names and
// names with slashes/parens survive.
func TestPeelElapsed(t *testing.T) {
	cases := map[string]string{
		"TestAdd":                "TestAdd",
		"TestAdd (0.00s)":        "TestAdd",
		"TestParent/sub (0.00s)": "TestParent/sub",
		"TestX/(a) (0.00s)":      "TestX/(a)",
	}
	for in, want := range cases {
		if got := peelElapsed(in); got != want {
			t.Errorf("peelElapsed(%q): got %q want %q", in, got, want)
		}
	}
}

// --- Run integration tests against a real, tiny Go module ---

// writeModule writes a minimal module into dir, then each (name, src) file.
func writeModule(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/verifytest\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// skipIfGoMissing skips when the go toolchain is unavailable, since the default
// command and these fixtures rely on it.
func skipIfGoMissing(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}
}

const passingTest = `package main

import "testing"

func TestAdd(t *testing.T) {
	if 2+2 != 4 {
		t.Errorf("expected 4")
	}
}
`

const failingTest = `package main

import "testing"

func TestAdd(t *testing.T) {
	if 2+2 != 5 {
		t.Errorf("expected 5, got %d", 2+2)
	}
}
`

const brokenMain = `package main

import "fmt"

func main() {
	fmt.Println(undef)
}
`

func TestRunPassingSuite(t *testing.T) {
	skipIfGoMissing(t)
	dir := t.TempDir()
	writeModule(t, dir, map[string]string{"math_test.go": passingTest})

	rep, err := Run(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Pass {
		t.Errorf("Pass: got false, want true (passing suite); output=%q exit=%d", rep.Output, rep.ExitCode)
	}
	if rep.Count != 0 {
		t.Errorf("Count: got %d want 0; failures=%+v", rep.Count, rep.Failures)
	}
	if rep.PackagesOK == 0 {
		t.Errorf("PackagesOK: got %d, want >=1", rep.PackagesOK)
	}
	if rep.Timeout {
		t.Error("Timeout should be false on a passing run")
	}
}

func TestRunFailingTest(t *testing.T) {
	skipIfGoMissing(t)
	dir := t.TempDir()
	writeModule(t, dir, map[string]string{"math_test.go": failingTest})

	rep, err := Run(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Pass {
		t.Error("Pass: got true, want false (failing test)")
	}
	if rep.ExitCode == 0 {
		t.Error("ExitCode: got 0, want non-zero for a failing test")
	}
	if rep.PackagesFailed == 0 {
		t.Errorf("PackagesFailed: got 0, want >=1; output=%q", rep.Output)
	}
	if rep.Count == 0 {
		t.Fatalf("Count: got 0, want at least one failure; output=%q", rep.Output)
	}
	f := rep.Failures[0]
	if f.Test != "TestAdd" {
		t.Errorf("Test: got %q want TestAdd", f.Test)
	}
	if f.Package != "example.com/verifytest" {
		t.Errorf("Package: got %q want example.com/verifytest", f.Package)
	}
	if !strings.Contains(f.Message, "expected 5") {
		t.Errorf("Message: got %q, want it to mention the assertion", f.Message)
	}
}

func TestRunBuildFailure(t *testing.T) {
	skipIfGoMissing(t)
	dir := t.TempDir()
	writeModule(t, dir, map[string]string{"main.go": brokenMain})

	rep, err := Run(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Pass {
		t.Error("Pass: got true, want false (build failure)")
	}
	if rep.Count == 0 {
		t.Fatalf("Count: got 0, want a build failure; output=%q", rep.Output)
	}
	// A build failure has no single test name; the message carries the error.
	var found bool
	for _, f := range rep.Failures {
		if f.Test == "" && strings.Contains(f.Message, "undefined: undef") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a package-level failure mentioning undefined: undef, got %+v", rep.Failures)
	}
}

// TestRunDefaultCommand verifies an empty command falls back to the Go default
// (`go test ./...`) and the report records exactly what ran.
func TestRunDefaultCommand(t *testing.T) {
	skipIfGoMissing(t)
	dir := t.TempDir()
	writeModule(t, dir, map[string]string{"math_test.go": passingTest})

	rep, err := Run(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Command) == 0 || rep.Command[0] != DefaultCommand[0] || rep.Command[2] != DefaultCommand[2] {
		t.Errorf("Command: got %v, want the go test default %v", rep.Command, DefaultCommand)
	}
}

// TestRunCustomCommand runs a fixed command that exits cleanly with no test
// output, confirming a configured command is honored verbatim.
func TestRunCustomCommand(t *testing.T) {
	skipIfGoMissing(t)
	rep, err := Run(Config{Command: []string{"go", "version"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Pass {
		t.Errorf("Pass: got false, want true for `go version`; output=%q", rep.Output)
	}
	if rep.Count != 0 {
		t.Errorf("Count: got %d, want 0 (go version emits no test results)", rep.Count)
	}
}

// TestRunMissingBinary verifies a command whose binary is absent surfaces a
// clear error rather than a misleading green run.
func TestRunMissingBinary(t *testing.T) {
	_, err := Run(Config{Command: []string{"definitely-not-a-real-binary-xyz"}})
	if err == nil {
		t.Fatal("want error for missing binary, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error: got %q, want it to mention 'not found'", err.Error())
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
	if rep.Pass {
		t.Error("Pass: got true, want false on timeout")
	}
	if !rep.Truncated {
		t.Error("Truncated: got false, want true (run was incomplete)")
	}
	// Must return well before the sleep would have finished naturally.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("did not honor the deadline: elapsed=%v", elapsed)
	}
}
