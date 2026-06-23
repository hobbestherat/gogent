package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogent/internal/daemon"
)

func TestRunDaemonRejectsMissingAndUnknownSubcommand(t *testing.T) {
	code, stdout, stderr := captureDaemonOutput(t, func() int {
		return runDaemon(nil)
	})
	if code != 2 {
		t.Fatalf("missing subcommand exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("missing subcommand stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "usage: gogent daemon <command>") {
		t.Fatalf("missing subcommand stderr = %q, want usage", stderr)
	}

	code, stdout, stderr = captureDaemonOutput(t, func() int {
		return runDaemon([]string{"bogus"})
	})
	if code != 2 {
		t.Fatalf("unknown subcommand exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("unknown subcommand stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `unknown command "bogus"`) || !strings.Contains(stderr, "usage: gogent daemon <command>") {
		t.Fatalf("unknown subcommand stderr = %q, want error and usage", stderr)
	}
}

func TestDaemonStatusReportsStoppedStaleAndRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := daemon.PathsFor(filepath.Join(home, ".gogent"))

	code, stdout, stderr := captureDaemonOutput(t, func() int {
		return daemonStatus(nil)
	})
	if code != 1 {
		t.Fatalf("stopped exit = %d, want 1", code)
	}
	if strings.TrimSpace(stdout) != "not running" {
		t.Fatalf("stopped stdout = %q, want not running", stdout)
	}
	if stderr != "" {
		t.Fatalf("stopped stderr = %q, want empty", stderr)
	}

	if err := daemon.WritePidfile(p.Pid, 99999999); err != nil {
		t.Fatalf("WritePidfile stale: %v", err)
	}
	code, stdout, stderr = captureDaemonOutput(t, func() int {
		return daemonStatus(nil)
	})
	if code != 1 {
		t.Fatalf("stale exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "not running (stale pidfile/socket present under "+p.Dir+")") {
		t.Fatalf("stale stdout = %q, want stale directory", stdout)
	}
	if stderr != "" {
		t.Fatalf("stale stderr = %q, want empty", stderr)
	}
	if err := daemon.CleanStale(p); err != nil {
		t.Fatalf("CleanStale: %v", err)
	}

	ln, err := daemon.Listen(p.Sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := serveCLIHealth(t, ln)
	defer func() { _ = srv.Close() }()
	if err := daemon.Acquire(p, "unix://"+p.Sock); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = daemon.Release(p) }()

	code, stdout, stderr = captureDaemonOutput(t, func() int {
		return daemonStatus(nil)
	})
	if code != 0 {
		t.Fatalf("running exit = %d, want 0; stderr=%q", code, stderr)
	}
	if want := "running (pid "; !strings.Contains(stdout, want) {
		t.Fatalf("running stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stdout, " at unix://"+p.Sock) {
		t.Fatalf("running stdout = %q, want socket address %q", stdout, p.Sock)
	}
	if stderr != "" {
		t.Fatalf("running stderr = %q, want empty", stderr)
	}
}

func TestDaemonStatusRejectsUnexpectedFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	code, stdout, stderr := captureDaemonOutput(t, func() int {
		return daemonStatus([]string{"--bad-flag"})
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "flag provided but not defined") {
		t.Fatalf("stderr = %q, want flag error", stderr)
	}
}

func TestTCPListenerAuthAndBindValidation(t *testing.T) {
	t.Run("loopback allowed without auth", func(t *testing.T) {
		t.Setenv("GOGENT_HTTP_TOKEN", "")
		ln, err := tcpListener("127.0.0.1", 0, "")
		if err != nil {
			t.Fatalf("tcpListener loopback: %v", err)
		}
		if err := ln.Close(); err != nil {
			t.Fatalf("close listener: %v", err)
		}
	})

	t.Run("non-loopback rejected without auth", func(t *testing.T) {
		t.Setenv("GOGENT_HTTP_TOKEN", "")
		ln, err := tcpListener("0.0.0.0", 0, "")
		if err == nil {
			_ = ln.Close()
			t.Fatal("tcpListener non-loopback without auth succeeded, want error")
		}
		if !strings.Contains(err.Error(), "refusing to bind HTTP server to non-loopback host") {
			t.Fatalf("error = %v, want non-loopback auth refusal", err)
		}
	})

	t.Run("non-loopback allowed with password", func(t *testing.T) {
		t.Setenv("GOGENT_HTTP_TOKEN", "")
		ln, err := tcpListener("0.0.0.0", 0, "secret")
		if err != nil {
			t.Fatalf("tcpListener non-loopback with password: %v", err)
		}
		if err := ln.Close(); err != nil {
			t.Fatalf("close listener: %v", err)
		}
	})

	t.Run("occupied port returns bind error", func(t *testing.T) {
		t.Setenv("GOGENT_HTTP_TOKEN", "")
		held, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("hold listener: %v", err)
		}
		defer func() { _ = held.Close() }()
		port := held.Addr().(*net.TCPAddr).Port

		ln, err := tcpListener("127.0.0.1", port, "")
		if err == nil {
			_ = ln.Close()
			t.Fatal("tcpListener on occupied port succeeded, want error")
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("listen tcp 127.0.0.1:%d", port)) {
			t.Fatalf("error = %v, want occupied bind address", err)
		}
	})
}

func captureDaemonOutput(t *testing.T, fn func() int) (int, string, string) {
	t.Helper()

	oldStdout := os.Stdout
	oldStderr := os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	os.Stdout = stdoutW
	os.Stderr = stderrW

	code := fn()

	_ = stdoutW.Close()
	_ = stderrW.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr

	var stdout, stderr bytes.Buffer
	if _, err := io.Copy(&stdout, stdoutR); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if _, err := io.Copy(&stderr, stderrR); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	_ = stdoutR.Close()
	_ = stderrR.Close()

	return code, stdout.String(), stderr.String()
}

func serveCLIHealth(t *testing.T, ln net.Listener) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	return srv
}
