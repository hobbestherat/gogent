package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestPidfileLifecycle(t *testing.T) {
	p := PathsFor(t.TempDir())

	if _, err := ReadPidfile(p.Pid); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadPidfile missing error = %v, want os.ErrNotExist", err)
	}

	if err := WritePidfile(p.Pid, 12345); err != nil {
		t.Fatalf("WritePidfile: %v", err)
	}
	got, err := ReadPidfile(p.Pid)
	if err != nil {
		t.Fatalf("ReadPidfile: %v", err)
	}
	if got != 12345 {
		t.Fatalf("pid = %d, want 12345", got)
	}

	mode := statMode(t, p.Pid)
	if mode != 0o600 {
		t.Fatalf("pidfile mode = %o, want 600", mode)
	}

	if err := os.WriteFile(p.Pid, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("write bad pidfile: %v", err)
	}
	if _, err := ReadPidfile(p.Pid); err == nil || !strings.Contains(err.Error(), "parse pidfile") {
		t.Fatalf("ReadPidfile bad content error = %v, want parse error", err)
	}

	if err := RemovePidfile(p.Pid); err != nil {
		t.Fatalf("RemovePidfile: %v", err)
	}
	if err := RemovePidfile(p.Pid); err != nil {
		t.Fatalf("RemovePidfile second call: %v", err)
	}
}

func TestAcquireReleaseAndCleanStale(t *testing.T) {
	p := PathsFor(t.TempDir())

	if err := Acquire(p, "unix://"+p.Sock); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got, err := ReadPidfile(p.Pid); err != nil || got != os.Getpid() {
		t.Fatalf("pidfile = %d, %v; want current pid %d", got, err, os.Getpid())
	}
	if got := readFile(t, p.Addr); got != "unix://"+p.Sock+"\n" {
		t.Fatalf("addr file = %q, want unix socket address", got)
	}

	if err := Release(p); err != nil {
		t.Fatalf("Release: %v", err)
	}
	assertMissing(t, p.Pid)
	assertMissing(t, p.Addr)

	if err := WritePidfile(p.Pid, 99999999); err != nil {
		t.Fatalf("WritePidfile stale: %v", err)
	}
	if err := os.WriteFile(p.Addr, []byte("unix://stale\n"), 0o600); err != nil {
		t.Fatalf("write stale addr: %v", err)
	}
	if err := os.WriteFile(p.Sock, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write stale socket placeholder: %v", err)
	}
	if st := Query(p); !st.Stale || st.Running {
		t.Fatalf("Query stale = %+v, want stale and not running", st)
	}
	if err := CleanStale(p); err != nil {
		t.Fatalf("CleanStale: %v", err)
	}
	assertMissing(t, p.Pid)
	assertMissing(t, p.Addr)
	assertMissing(t, p.Sock)
}

func TestListenProbeAndQuery(t *testing.T) {
	p := PathsFor(t.TempDir())

	if Probe(p.Sock) {
		t.Fatal("Probe missing socket = true, want false")
	}

	if err := os.WriteFile(p.Sock, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}
	ln, err := Listen(p.Sock)
	if err != nil {
		t.Fatalf("Listen with stale socket: %v", err)
	}
	srv := serveHealth(t, ln, http.StatusOK)
	defer closeServer(t, srv)

	if mode := statMode(t, p.Sock); mode != 0o600 {
		t.Fatalf("socket mode = %o, want 600", mode)
	}
	if !Probe(p.Sock) {
		t.Fatal("Probe live health socket = false, want true")
	}
	if err := Acquire(p, "unix://"+p.Sock); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if st := Query(p); !st.Running || st.Stale || st.PID != os.Getpid() || st.Addr != "unix://"+p.Sock {
		t.Fatalf("Query running = %+v, want running current pid/address", st)
	}

	if _, err := Listen(p.Sock); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Listen second error = %v, want ErrAlreadyRunning", err)
	}
}

func TestListenLockIsReleasedOnClose(t *testing.T) {
	p := PathsFor(t.TempDir())

	ln, err := Listen(p.Sock)
	if err != nil {
		t.Fatalf("Listen first: %v", err)
	}
	if _, err := Listen(p.Sock); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Listen while lock held error = %v, want ErrAlreadyRunning", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close first listener: %v", err)
	}

	ln2, err := Listen(p.Sock)
	if err != nil {
		t.Fatalf("Listen after close: %v", err)
	}
	if err := ln2.Close(); err != nil {
		t.Fatalf("close second listener: %v", err)
	}
	if _, err := os.Stat(p.Lock); err != nil {
		t.Fatalf("lock file after close = %v, want persistent lock file", err)
	}
}

func TestListenConcurrentStartOnlyOneWins(t *testing.T) {
	p := PathsFor(t.TempDir())

	const workers = 16
	start := make(chan struct{})
	results := make(chan listenResult, workers)
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			ln, err := Listen(p.Sock)
			results <- listenResult{ln: ln, err: err}
		}()
	}
	close(start)

	var winners []net.Listener
	var alreadyRunning int
	for i := 0; i < workers; i++ {
		res := <-results
		switch {
		case res.err == nil:
			winners = append(winners, res.ln)
		case errors.Is(res.err, ErrAlreadyRunning):
			alreadyRunning++
		default:
			t.Fatalf("Listen concurrent unexpected error: %v", res.err)
		}
	}
	for _, ln := range winners {
		if err := ln.Close(); err != nil {
			t.Fatalf("close winning listener: %v", err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("winners = %d, want 1", len(winners))
	}
	if alreadyRunning != workers-1 {
		t.Fatalf("ErrAlreadyRunning count = %d, want %d", alreadyRunning, workers-1)
	}
}

func TestProbeRejectsNonHealthySocket(t *testing.T) {
	p := PathsFor(t.TempDir())
	ln, err := net.Listen("unix", p.Sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := serveHealth(t, ln, http.StatusNotFound)
	defer closeServer(t, srv)

	if Probe(p.Sock) {
		t.Fatal("Probe 404 health socket = true, want false")
	}
}

func TestQueryClassifiesStaleResidue(t *testing.T) {
	t.Run("dead pidfile", func(t *testing.T) {
		p := PathsFor(t.TempDir())
		if err := WritePidfile(p.Pid, 99999999); err != nil {
			t.Fatalf("WritePidfile: %v", err)
		}
		st := Query(p)
		if !st.Stale || st.Running || st.PID != 99999999 {
			t.Fatalf("Query = %+v, want stale dead pidfile", st)
		}
	})

	t.Run("socket without pidfile", func(t *testing.T) {
		p := PathsFor(t.TempDir())
		ln, err := Listen(p.Sock)
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		defer func() { _ = ln.Close() }()
		st := Query(p)
		if !st.Stale || st.Running || st.PID != 0 {
			t.Fatalf("Query = %+v, want stale socket residue", st)
		}
	})

	t.Run("pid alive but no socket", func(t *testing.T) {
		p := PathsFor(t.TempDir())
		if err := WritePidfile(p.Pid, os.Getpid()); err != nil {
			t.Fatalf("WritePidfile: %v", err)
		}
		st := Query(p)
		if !st.Stale || st.Running || st.PID != os.Getpid() {
			t.Fatalf("Query = %+v, want stale because socket is absent", st)
		}
	})
}

func TestStopCleansStaleWhenNotRunning(t *testing.T) {
	p := PathsFor(t.TempDir())
	if err := WritePidfile(p.Pid, 99999999); err != nil {
		t.Fatalf("WritePidfile: %v", err)
	}
	if err := os.WriteFile(p.Sock, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}

	if err := Stop(p, 10*time.Millisecond, false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop error = %v, want ErrNotRunning", err)
	}
	assertMissing(t, p.Pid)
	assertMissing(t, p.Sock)
}

func TestStopUsesSocketExitWhenPidfileIsBad(t *testing.T) {
	cases := []struct {
		name       string
		writeSetup func(t *testing.T, p Paths)
	}{
		{
			name: "missing pidfile",
			writeSetup: func(t *testing.T, p Paths) {
				t.Helper()
			},
		},
		{
			name: "stale pidfile",
			writeSetup: func(t *testing.T, p Paths) {
				t.Helper()
				if err := WritePidfile(p.Pid, 99999999); err != nil {
					t.Fatalf("WritePidfile stale: %v", err)
				}
			},
		},
		{
			name: "corrupt pidfile",
			writeSetup: func(t *testing.T, p Paths) {
				t.Helper()
				if err := os.WriteFile(p.Pid, []byte("not-a-pid\n"), 0o600); err != nil {
					t.Fatalf("write corrupt pidfile: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := PathsFor(t.TempDir())
			ln, err := Listen(p.Sock)
			if err != nil {
				t.Fatalf("Listen: %v", err)
			}
			srv := serveHealthAndExit(t, ln, http.StatusOK, true)
			defer closeServer(t, srv)
			if err := writeAddr(p, "unix://"+p.Sock); err != nil {
				t.Fatalf("writeAddr: %v", err)
			}
			tc.writeSetup(t, p)

			if err := Stop(p, 2*time.Second, false); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			assertMissing(t, p.Pid)
			assertMissing(t, p.Sock)
			assertMissing(t, p.Addr)
		})
	}
}

func TestStopDoesNotCleanLiveSocketWhenExitFailsAndPidIsUnusable(t *testing.T) {
	cases := []struct {
		name       string
		writeSetup func(t *testing.T, p Paths)
	}{
		{
			name: "missing pidfile",
			writeSetup: func(t *testing.T, p Paths) {
				t.Helper()
			},
		},
		{
			name: "dead pidfile",
			writeSetup: func(t *testing.T, p Paths) {
				t.Helper()
				if err := WritePidfile(p.Pid, 99999999); err != nil {
					t.Fatalf("WritePidfile stale: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := PathsFor(t.TempDir())
			ln, err := Listen(p.Sock)
			if err != nil {
				t.Fatalf("Listen: %v", err)
			}
			srv := serveHealthAndExit(t, ln, http.StatusForbidden, false)
			defer closeServer(t, srv)
			if err := writeAddr(p, "unix://"+p.Sock); err != nil {
				t.Fatalf("writeAddr: %v", err)
			}
			tc.writeSetup(t, p)

			err = Stop(p, 50*time.Millisecond, false)
			if err == nil || !strings.Contains(err.Error(), "pidfile is missing or invalid") {
				t.Fatalf("Stop error = %v, want invalid pidfile/live socket error", err)
			}
			if !Probe(p.Sock) {
				t.Fatal("socket was cleaned or stopped after failed /exit with unusable pid; want live socket preserved")
			}
			if _, err := os.Stat(p.Sock); err != nil {
				t.Fatalf("socket file after failed Stop: %v", err)
			}
		})
	}
}

func TestStopGracefulAndForced(t *testing.T) {
	t.Run("graceful", func(t *testing.T) {
		p := PathsFor(t.TempDir())
		cmd := startShellDaemon(t, `trap "exit 0" TERM; while :; do sleep 1 & wait $!; done`)
		reap := reapCommand(t, cmd)
		needsCleanup := true
		defer func() {
			if needsCleanup {
				cleanupProcess(t, cmd, reap)
			}
		}()
		startLiveStatus(t, p, cmd.Process.Pid)

		if err := Stop(p, 2*time.Second, false); err != nil {
			t.Fatalf("Stop graceful: %v", err)
		}
		assertMissing(t, p.Pid)
		assertMissing(t, p.Sock)
		if err := <-reap; err != nil {
			t.Fatalf("process wait after graceful stop: %v", err)
		}
		needsCleanup = false
	})

	t.Run("timeout without force leaves lifecycle files", func(t *testing.T) {
		p := PathsFor(t.TempDir())
		cmd := startShellDaemon(t, `trap "" TERM; while :; do sleep 1 & wait $!; done`)
		reap := reapCommand(t, cmd)
		defer cleanupProcess(t, cmd, reap)
		startLiveStatus(t, p, cmd.Process.Pid)

		err := Stop(p, 100*time.Millisecond, false)
		if err == nil || !strings.Contains(err.Error(), "did not exit") {
			t.Fatalf("Stop timeout error = %v, want did-not-exit error", err)
		}
		if _, err := os.Stat(p.Pid); err != nil {
			t.Fatalf("pidfile after timeout: %v", err)
		}
		if _, err := os.Stat(p.Sock); err != nil {
			t.Fatalf("socket after timeout: %v", err)
		}
	})

	t.Run("force kills and cleans lifecycle files", func(t *testing.T) {
		p := PathsFor(t.TempDir())
		cmd := startShellDaemon(t, `trap "" TERM; while :; do sleep 1 & wait $!; done`)
		reap := reapCommand(t, cmd)
		needsCleanup := true
		defer func() {
			if needsCleanup {
				cleanupProcess(t, cmd, reap)
			}
		}()
		startLiveStatus(t, p, cmd.Process.Pid)

		if err := Stop(p, 100*time.Millisecond, true); err != nil {
			t.Fatalf("Stop force: %v", err)
		}
		assertMissing(t, p.Pid)
		assertMissing(t, p.Sock)
		if err := <-reap; err == nil {
			t.Fatal("process wait after force returned nil, want signal error")
		}
		needsCleanup = false
	})
}

func startLiveStatus(t *testing.T, p Paths, pid int) {
	t.Helper()
	ln, err := Listen(p.Sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := serveHealth(t, ln, http.StatusOK)
	t.Cleanup(func() { closeServer(t, srv) })
	if err := WritePidfile(p.Pid, pid); err != nil {
		t.Fatalf("WritePidfile: %v", err)
	}
	if err := writeAddr(p, "unix://"+p.Sock); err != nil {
		t.Fatalf("writeAddr: %v", err)
	}
	if st := Query(p); !st.Running {
		t.Fatalf("test setup Query = %+v, want running", st)
	}
}

func serveHealth(t *testing.T, ln net.Listener, status int) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
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

func serveHealthAndExit(t *testing.T, ln net.Listener, exitStatus int, closeOnExit bool) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	var srv *http.Server
	var once sync.Once
	mux.HandleFunc(exitPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(exitStatus)
		if closeOnExit {
			once.Do(func() {
				go func() {
					_ = srv.Close()
				}()
			})
		}
	})

	srv = &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	return srv
}

func closeServer(t *testing.T, srv *http.Server) {
	t.Helper()
	if err := srv.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("server close: %v", err)
	}
}

func startShellDaemon(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	return cmd
}

func reapCommand(t *testing.T, cmd *exec.Cmd) <-chan error {
	t.Helper()
	ch := make(chan error, 1)
	go func() {
		ch <- cmd.Wait()
	}()
	return ch
}

func cleanupProcess(t *testing.T, cmd *exec.Cmd, reap <-chan error) {
	t.Helper()
	if cmd.Process == nil {
		return
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-reap:
	case <-time.After(time.Second):
		t.Fatalf("helper process pid %d did not exit during cleanup", cmd.Process.Pid)
	}
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s error = %v, want os.ErrNotExist", path, err)
	}
}

type listenResult struct {
	ln  net.Listener
	err error
}

func TestPathsFor(t *testing.T) {
	dir := t.TempDir()
	p := PathsFor(dir)
	if p.Dir != dir {
		t.Fatalf("Dir = %q, want %q", p.Dir, dir)
	}
	for name, got := range map[string]string{
		"Pid":  p.Pid,
		"Sock": p.Sock,
		"Addr": p.Addr,
		"Log":  p.Log,
		"Lock": p.Lock,
	} {
		if !strings.HasPrefix(got, dir+string(filepath.Separator)) {
			t.Fatalf("%s path = %q, want under %q", name, got, dir)
		}
	}
}
