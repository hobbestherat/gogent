//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestIssue358WindowsDetachUsesDetachedProcessGroup(t *testing.T) {
	attr := detachSysProcAttr()
	if attr == nil {
		t.Fatal("detachSysProcAttr() = nil")
	}
	want := uint32(createNewProcessGroup | detachedProcess)
	if attr.CreationFlags&want != want {
		t.Fatalf("CreationFlags = %#x, want both CREATE_NEW_PROCESS_GROUP and DETACHED_PROCESS (%#x)", attr.CreationFlags, want)
	}
}

func TestIssue358WindowsListenLocalUsesLoopbackTCPDiscovery(t *testing.T) {
	p := PathsFor(t.TempDir())

	ln, addr, err := ListenLocal(p)
	if err != nil {
		t.Fatalf("ListenLocal: %v", err)
	}
	defer func() { _ = ln.Close() }()

	if !strings.HasPrefix(addr, "http://127.0.0.1:") {
		t.Fatalf("ListenLocal addr = %q, want loopback http discovery address", addr)
	}
	if _, ok := ln.Addr().(*net.TCPAddr); !ok {
		t.Fatalf("ListenLocal listener addr type = %T, want *net.TCPAddr", ln.Addr())
	}
	if _, err := os.Stat(p.Sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Windows ListenLocal socket stat = %v, want no Unix socket file", err)
	}
}

func TestIssue358WindowsQueryAndDiscoveryUseAddrFileTCPHealth(t *testing.T) {
	p := PathsFor(t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	srv := serveWindowsHealth(t, ln)
	defer func() { _ = srv.Close() }()

	addr := "http://" + ln.Addr().String()
	if err := Acquire(p, addr); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = Release(p) }()

	st := Query(p)
	if !st.Running || st.Stale || st.PID != os.Getpid() || st.Addr != addr {
		t.Fatalf("Query = %+v, want running current pid at %s", st, addr)
	}
	if !ProbeLocal(p) {
		t.Fatal("ProbeLocal = false, want true for TCP /health")
	}
	if got := LocalDiscoveryAddr(p); got != addr {
		t.Fatalf("LocalDiscoveryAddr = %q, want %q", got, addr)
	}

	if err := Release(p); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := LocalDiscoveryAddr(p); got != "" {
		t.Fatalf("LocalDiscoveryAddr after Release = %q, want empty", got)
	}
}

func TestIssue358WindowsListenLocalRefusesSecondLiveInstance(t *testing.T) {
	p := PathsFor(t.TempDir())
	ln, addr, err := ListenLocal(p)
	if err != nil {
		t.Fatalf("ListenLocal first: %v", err)
	}
	srv := serveWindowsHealth(t, ln)
	defer func() { _ = srv.Close() }()

	if err := Acquire(p, addr); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = Release(p) }()

	ln2, _, err := ListenLocal(p)
	if err == nil {
		_ = ln2.Close()
		t.Fatal("ListenLocal second succeeded, want ErrAlreadyRunning")
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("ListenLocal second error = %v, want ErrAlreadyRunning", err)
	}
}

func TestIssue358WindowsStopPrefersExitEndpointWithMissingPidfile(t *testing.T) {
	p := PathsFor(t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	srv := serveWindowsHealthAndExit(t, ln)
	defer func() { _ = srv.Close() }()

	if err := writeAddr(p, "http://"+ln.Addr().String()); err != nil {
		t.Fatalf("writeAddr: %v", err)
	}

	if err := Stop(p, 5*time.Second, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(p.Addr); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("addr file after Stop = %v, want removed", err)
	}
}

func TestIssue358WindowsGracefulSignalIsNoopAndForceKillRejectsInvalidPID(t *testing.T) {
	if err := gracefulSignal(os.Getpid()); err != nil {
		t.Fatalf("gracefulSignal current pid = %v, want nil no-op", err)
	}
	if ProcessAlive(0) || ProcessAlive(-1) {
		t.Fatal("ProcessAlive for non-positive pid = true, want false")
	}
	if err := forceKill(-1); err == nil {
		t.Fatal("forceKill(-1) succeeded, want error")
	}
}

func serveWindowsHealth(t *testing.T, ln net.Listener) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	return srv
}

func serveWindowsHealthAndExit(t *testing.T, ln net.Listener) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *http.Server
	mux.HandleFunc(healthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc(exitPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		go func() { _ = srv.Close() }()
	})
	srv = &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	return srv
}
