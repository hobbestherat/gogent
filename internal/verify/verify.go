// Package verify runs a project's test command and turns its output into
// structured pass/fail results (failing package + test + message). It backs the
// model-callable "verify" tool (issue #44): the tight edit→test→read-failures
// loop that lets an agent reliably land green, without the model shelling out
// to the test runner by hand and eyeballing text.
//
// It executes the configured command with an explicit argument vector (never a
// shell string, so there is no shell-injection surface) and bounds it with a
// timeout. The default command is `go test ./...`, whose text output is parsed
// into per-test and per-package failures. Output from the raw run is always
// retained (truncated) as a fallback, so anything the line parser misses is
// still visible to the model.
package verify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// DefaultCommand is the verify command used when Config.Command is empty: `go
// test ./...` runs every Go test in the workspace and is the canonical tight
// verify loop for a Go project.
var DefaultCommand = []string{"go", "test", "./..."}

// DefaultTimeout bounds a verify run when Config.Timeout is non-positive. A full
// test suite can be slower than a compile or git plumbing, so it is the most
// generous of the built-in tool timeouts.
const DefaultTimeout = 5 * time.Minute

// maxFailures caps how many failures Run returns, so a broken suite with
// hundreds of failing cases cannot flood the model's context. The raw output is
// always retained (truncated) for the overflow.
const maxFailures = 50

// maxFailureMessage caps each failure's captured message, so a panic stack trace
// cannot dominate the structured result. The head is kept, where the assertion
// or the first stack frame appears; the full text remains in the run's Output.
const maxFailureMessage = 512

// maxOutput caps the raw combined output retained as a fallback. The head is
// kept, where the first failures usually appear.
const maxOutput = 4 * 1024

// Failure is one test or build failure parsed from the test command's output.
type Failure struct {
	// Package is the failing package (a Go import path, or whatever token the
	// runner emits in its summary line). It may be empty when the output was
	// truncated before the package summary arrived.
	Package string `json:"package"`
	// Test is the failing test/benchmark/example name (e.g. "TestAdd"). Empty
	// for a build/compile failure that is not tied to a single test.
	Test string `json:"test,omitempty"`
	// Message is the failure's captured output: the t.Error/t.Log text for a
	// test failure, or the path:line:col error line for a build failure.
	Message string `json:"message"`
}

// Config controls a verify run.
type Config struct {
	// Dir is the directory the command runs in (the workspace root). Empty uses
	// the process working directory.
	Dir string
	// Command is the argument vector to run; empty falls back to DefaultCommand.
	Command []string
	// Timeout bounds the command. Non-positive falls back to DefaultTimeout.
	Timeout time.Duration
}

// Report is the outcome of a verify run.
type Report struct {
	Command         []string  `json:"command"`
	ExitCode        int       `json:"exit_code"`
	Pass            bool      `json:"pass"`              // ran to completion with exit 0 (suite green)
	Timeout         bool      `json:"timeout"`           // the deadline fired
	PackagesOK      int       `json:"packages_ok"`       // packages that reported `ok`
	PackagesFailed  int       `json:"packages_failed"`   // packages that reported `FAIL`
	PackagesNoTests int       `json:"packages_no_tests"` // packages with no test files (`?`)
	Failures        []Failure `json:"failures"`
	Count           int       `json:"count"`               // len(Failures)
	Output          string    `json:"output,omitempty"`    // truncated raw combined output
	Truncated       bool      `json:"truncated,omitempty"` // the run, output or failures were capped
}

// Run executes the configured verify command and returns a Report. A non-zero
// exit (the normal case when tests fail) is reported via Report.Pass /
// Report.Failures, not as a Go error; an error is returned only when the command
// cannot be launched at all (not installed) or fails to start for an unexpected
// reason.
func Run(cfg Config) (*Report, error) {
	cmd := cfg.Command
	if len(cmd) == 0 {
		cmd = DefaultCommand
	}
	if _, err := exec.LookPath(cmd[0]); err != nil {
		return nil, fmt.Errorf("verify command not found: %s", cmd[0])
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...) //nolint:gosec // launches the configured/trusted verify command
	if cfg.Dir != "" {
		c.Dir = cfg.Dir
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	runErr := c.Run()
	combined := stderr.String() + stdout.String()

	rep := &Report{
		Command: cmd,
		Output:  capString(combined, maxOutput),
	}
	if runErr != nil {
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			rep.Timeout = true
		default:
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				rep.ExitCode = exitErr.ExitCode()
			} else {
				// Could not launch (rare; "not installed" is caught by LookPath
				// above). Surface it rather than report a misleading green run.
				return nil, fmt.Errorf("failed to run %s: %w", strings.Join(cmd, " "), runErr)
			}
		}
	}

	p := parse(combined)
	rep.Failures = p.failures
	rep.PackagesOK = p.okPkgs
	rep.PackagesFailed = p.failedPkgs
	rep.PackagesNoTests = p.noTestPkgs
	rep.Count = len(rep.Failures)
	rep.Pass = !rep.Timeout && runErr == nil
	rep.Truncated = p.capped || len(combined) > maxOutput || rep.Timeout
	return rep, nil
}

// pkgSummaryRE matches `go test`'s per-package result line:
//
//	"FAIL\tpkg\t0.005s", "FAIL\tpkg [build failed]"
//	"ok  \tpkg\t0.005s"
//	"?   \tpkg\t[no test files]"
//
// The status word starts at column 0 (so indented t.Log lines never match) and
// is followed by whitespace and the package token. A bare "FAIL"/"PASS" has no
// token after it, so it does not match and is handled as a section terminator.
var pkgSummaryRE = regexp.MustCompile(`^(FAIL|ok|\?)\s+(\S+)`)

// testResultRE matches a test result marker: "--- FAIL: TestAdd (0.00s)" (also
// SKIP/PASS). The trailing "(elapsed)" is peeled off the name afterwards.
var testResultRE = regexp.MustCompile(`^--- (FAIL|SKIP|PASS):\s+(.*)$`)

// buildErrRE matches a compiler error line `path:line:col: message` (column
// optional), the same shape `go vet`/`go build` emit for a build failure. An
// optional leading `tool: ` prefix is tolerated. It mirrors diagnostics' line
// regex; inside an open test failure the same shape is a t.Log line and is sent
// to the failure's message instead.
var buildErrRE = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9_-]*: )?([^:\s][^:]*):(\d+):(?:(\d+):)?\s*(.*)$`)

// elapsedRE peels a trailing "(...)" duration (e.g. "(0.00s)") off a test name.
var elapsedRE = regexp.MustCompile(`^(.*)\s+\(([^)]*)\)$`)

type parsed struct {
	failures   []Failure
	okPkgs     int
	failedPkgs int
	noTestPkgs int
	capped     bool
}

// parse extracts failures and package tallies from test-command output. It is a
// small state machine over the line-oriented text `go test` emits.
//
// go test prints a failing test's section ("--- FAIL: Name", its logs, then a
// bare "FAIL") *before* the package summary line that names its package, so a
// test failure is buffered until its package summary arrives. Build failures
// invert this: a "# pkg" header precedes the path:line error lines, so the
// package is known up front. Panics, stack traces and any other output emitted
// while a test failure is open are folded into that failure's message.
func parse(output string) parsed {
	var (
		currentPackage string
		current        *Failure // test failure currently collecting log output
		pending        []Failure
		out            parsed
	)

	// flush closes the test failure currently collecting output, moving it to
	// pending to await its package summary.
	flush := func() {
		if current == nil {
			return
		}
		current.Message = capString(current.Message, maxFailureMessage)
		pending = append(pending, *current)
		current = nil
	}

	// assign attaches the named package to every buffered test failure and moves
	// them into the result; pending is emptied.
	assign := func(pkg string) {
		for i := range pending {
			pending[i].Package = pkg
			out.failures = append(out.failures, pending[i])
		}
		pending = nil
	}

	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)

		// Per-package summary line names the package and resolves buffered test
		// failures.
		if m := pkgSummaryRE.FindStringSubmatch(line); m != nil {
			flush()
			currentPackage = m[2]
			switch m[1] {
			case "FAIL":
				out.failedPkgs++
				assign(currentPackage)
			case "ok":
				out.okPkgs++
			case "?":
				out.noTestPkgs++
			}
			continue
		}

		// Build-failure header ("# pkg" / "# [pkg]") names the package for the
		// error lines that follow. Take the first token, stripping brackets.
		if strings.HasPrefix(trimmed, "# ") {
			if fields := strings.Fields(trimmed[2:]); len(fields) > 0 {
				currentPackage = strings.Trim(fields[0], "[]")
			}
			continue
		}

		// "--- FAIL: Name" opens a test failure; "--- SKIP/PASS:" just closes
		// any open one (skips and passes are not failures).
		if m := testResultRE.FindStringSubmatch(trimmed); m != nil {
			flush()
			if m[1] == "FAIL" {
				current = &Failure{Test: peelElapsed(m[2])}
			}
			continue
		}

		// Verbose framework markers (=== RUN/CONT/PAUSE/NAME) carry no failure
		// content; drop them whether or not a failure is open.
		if strings.HasPrefix(trimmed, "=== ") {
			continue
		}

		// A bare "FAIL" ends a test's section; "PASS" and "exit status N" are
		// noise. Each closes any open failure.
		if trimmed == "FAIL" || trimmed == "PASS" || strings.HasPrefix(trimmed, "exit status") {
			flush()
			continue
		}

		// A path:line:col error line outside an open test failure is a build
		// error; attribute it to the current package. The same shape inside a
		// test failure is a t.Log line and falls through to the message below.
		if current == nil && currentPackage != "" {
			if bm := buildErrRE.FindStringSubmatch(line); bm != nil {
				if len(out.failures) >= maxFailures {
					out.capped = true
					continue
				}
				out.failures = append(out.failures, Failure{
					Package: currentPackage,
					Message: capString(trimmed, maxFailureMessage),
				})
				continue
			}
		}

		// Anything else while a test failure is open is its log or panic output.
		if current != nil && trimmed != "" {
			if current.Message != "" {
				current.Message += "\n"
			}
			current.Message += trimmed
		}
	}

	flush()
	// Attribute any test failures whose package summary never arrived (the run
	// was truncated before it) to the last package seen, rather than drop them.
	if len(pending) > 0 {
		assign(currentPackage)
	}
	return out
}

// peelElapsed strips a trailing "(...)" duration from a test result name.
func peelElapsed(s string) string {
	if m := elapsedRE.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return s
}

// capString caps s to max bytes, keeping the head and appending a marker so a
// truncated tail is self-evident. It bounds both the raw output fallback and
// each failure's captured message.
func capString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…[truncated]"
}
