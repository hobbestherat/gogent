// Package diagnostics runs a project's compiler/linter and turns its output into
// structured diagnostics (file:line:column + severity + message). It backs the
// model-callable "diagnostics" tool (issue #42): push-button "did it compile /
// typecheck?" feedback without the model shelling out to the compiler by hand.
//
// It executes the configured command with an explicit argument vector (never a
// shell string, so there is no shell-injection surface), bounds it with a
// timeout, and parses the compiler's line-oriented diagnostics. The default
// command is `go vet ./...`, which typechecks Go packages and surfaces both
// compile errors and vet findings in the standard `path:line:col: message`
// format. Output from other tools in the same format is parsed the same way.
package diagnostics

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultCommand is the diagnostics command used when Config.Command is empty:
// `go vet ./...` typechecks every Go package in the workspace and reports vet
// findings, surfacing compile errors and vet issues alike.
var DefaultCommand = []string{"go", "vet", "./..."}

// DefaultTimeout bounds a diagnostics run when Config.Timeout is non-positive.
// Compiling/vetting a project can be slower than git plumbing, so it is more
// generous than vcs.DefaultTimeout.
const DefaultTimeout = 2 * time.Minute

// maxDiagnostics caps how many diagnostics Run returns, so a broken build with
// hundreds of errors cannot flood the model's context.
const maxDiagnostics = 50

// maxOutput caps the raw combined output retained for context (anything the line
// parser misses: package headers, caret context, …). The retained text keeps the
// head, where the first errors and package context usually appear.
const maxOutput = 4 * 1024

// Severity classifies a diagnostic.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Diagnostic is one compiler/linter finding.
type Diagnostic struct {
	Path     string   `json:"path"`
	Line     int      `json:"line,omitempty"`
	Column   int      `json:"column,omitempty"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// Config controls a diagnostics run.
type Config struct {
	// Dir is the directory the command runs in (the workspace root). Empty uses
	// the process working directory.
	Dir string
	// Command is the argument vector to run; empty falls back to DefaultCommand.
	Command []string
	// WarningPattern, when set, is a regular expression tested against each
	// parsed message; a match marks the diagnostic a warning rather than an
	// error. Empty treats every diagnostic as an error (the common case for
	// `go vet`/`go build`).
	WarningPattern string
	// Timeout bounds the command. Non-positive falls back to DefaultTimeout.
	Timeout time.Duration
}

// Report is the outcome of a diagnostics run.
type Report struct {
	Command     []string     `json:"command"`
	ExitCode    int          `json:"exit_code"`
	OK          bool         `json:"ok"`      // ran to completion with exit 0
	Timeout     bool         `json:"timeout"` // the deadline fired
	Diagnostics []Diagnostic `json:"diagnostics"`
	Count       int          `json:"count"`
	Output      string       `json:"output,omitempty"`    // truncated raw combined output
	Truncated   bool         `json:"truncated,omitempty"` // the run or diagnostics were capped
}

// Run executes the configured diagnostics command and returns a Report. A
// non-zero exit (the normal case when there are findings) is reported via
// Report.ExitCode / Report.Diagnostics, not as a Go error; an error is returned
// only when the command cannot be launched at all (not installed), the
// WarningPattern is invalid, or the run fails to start for an unexpected reason.
func Run(cfg Config) (*Report, error) {
	cmd := cfg.Command
	if len(cmd) == 0 {
		cmd = DefaultCommand
	}
	if _, err := exec.LookPath(cmd[0]); err != nil {
		return nil, fmt.Errorf("diagnostics command not found: %s", cmd[0])
	}

	var warn *regexp.Regexp
	if cfg.WarningPattern != "" {
		w, err := regexp.Compile(cfg.WarningPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid warning_pattern: %w", err)
		}
		warn = w
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
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
		Output:  truncateOutput(combined),
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
				// above). Surface it rather than report a misleading clean run.
				return nil, fmt.Errorf("failed to run %s: %w", strings.Join(cmd, " "), runErr)
			}
		}
	}

	diags, diagCapped := parse(combined, warn)
	rep.Diagnostics = diags
	rep.Truncated = diagCapped || rep.Timeout
	rep.Count = len(rep.Diagnostics)
	rep.OK = !rep.Timeout && runErr == nil
	return rep, nil
}

// diagLineRE matches the `path:line:col: message` (or `path:line: message`)
// format emitted by `go vet`, `go build`, `gofmt`-style tools and most
// compilers/linters that follow the de-facto standard. The column is optional.
// An optional leading `tool: ` prefix (as `go vet` adds, e.g. "vet: ./f.go:…")
// is tolerated so vet findings parse as cleanly as build errors. Package header
// lines (`# pkg`), `exit status N` and source/caret context lines do not match
// and are ignored.
var diagLineRE = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9_-]*: )?([^:]+):(\d+):(?:(\d+):)?\s*(.*)$`)

// parse extracts diagnostics from compiler/linter output. It returns at most
// maxDiagnostics findings; when more are present, capped is true. warn, when
// non-nil, marks matching messages as warnings rather than errors.
func parse(output string, warn *regexp.Regexp) (diags []Diagnostic, capped bool) {
	for _, line := range strings.Split(output, "\n") {
		m := diagLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		severity := SeverityError
		if warn != nil && warn.MatchString(m[4]) {
			severity = SeverityWarning
		}
		diags = append(diags, Diagnostic{
			Path:     strings.TrimSpace(strings.TrimPrefix(m[1], "./")),
			Line:     atoiOrZero(m[2]),
			Column:   atoiOrZero(m[3]),
			Severity: severity,
			Message:  strings.TrimSpace(m[4]),
		})
		if len(diags) >= maxDiagnostics {
			return diags, true
		}
	}
	return diags, false
}

// atoiOrZero parses s as an int, returning 0 when s is empty or not a number
// (the optional column capture is empty when the tool emitted none).
func atoiOrZero(s string) int {
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

// truncateOutput caps combined output to maxOutput bytes, keeping the head and
// appending a marker so a truncated tail is self-evident.
func truncateOutput(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + "\n…[output truncated]"
}
