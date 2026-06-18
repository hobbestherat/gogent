package notify

import (
	"strings"
	"sync"
	"testing"
	"time"

	"gogent/internal/config"
)

// fakeLookPath returns a lookPathFunc that "finds" exactly the binaries in the
// given set, so nativeCommand can be tested without touching the real $PATH.
func fakeLookPath(found ...string) lookPathFunc {
	set := make(map[string]bool, len(found))
	for _, b := range found {
		set[b] = true
	}
	return func(file string) (string, error) {
		if set[file] {
			return "/usr/bin/" + file, nil
		}
		return "", errNotFound
	}
}

// errNotFound stands in for exec.LookPath's "not found" error in tests.
var errNotFound = lookPathError{}

type lookPathError struct{}

func (lookPathError) Error() string { return "executable file not found" }

func TestSanitize(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"plain text unchanged", "Task complete", "Task complete"},
		{"newline becomes space", "line one\nline two", "line one line two"},
		{"carriage return becomes space", "a\rb", "a b"},
		{"tab becomes space", "a\tb", "a b"},
		{"BEL stripped to space", "a\x07b", "a b"},
		{"ESC stripped to space", "a\x1bb", "a b"},
		{"other control bytes dropped", "a\x01\x02b", "ab"},
		{"DEL dropped", "a\x7fb", "ab"},
		{"empty stays empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitize(tc.in); got != tc.want {
				t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDesktopSequence(t *testing.T) {
	// Both OSC 9 (combined message) and OSC 777 (notify;title;body) are emitted,
	// each terminated by BEL.
	got := desktopSequence("Task complete", "Done")
	if !strings.Contains(got, "\x1b]9;Task complete — Done\x07") {
		t.Errorf("missing OSC 9 sequence in %q", got)
	}
	if !strings.Contains(got, "\x1b]777;notify;Task complete;Done\x07") {
		t.Errorf("missing OSC 777 sequence in %q", got)
	}

	// A title with a newline must not be able to break out of the OSC payload.
	safe := desktopSequence("a\nb\x07c", "body")
	if strings.Contains(safe, "\n") || strings.Contains(safe, "\x07b") {
		t.Errorf("control char leaked into desktop sequence: %q", safe)
	}

	// An empty title yields a body-only OSC 9 message and an empty title field.
	got = desktopSequence("", "Just body")
	if !strings.Contains(got, "\x1b]9;Just body\x07") {
		t.Errorf("empty-title OSC 9 should use body only, got %q", got)
	}
	if !strings.Contains(got, "\x1b]777;notify;;Just body\x07") {
		t.Errorf("empty-title OSC 777 should carry an empty title, got %q", got)
	}
}

func TestNativeCommand(t *testing.T) {
	for _, tc := range []struct {
		name       string
		goos       string
		look       lookPathFunc
		title      string
		body       string
		wantName   string
		wantArgs   []string
		wantAbsent bool
	}{
		{
			name:  "linux notify-send",
			goos:  "linux",
			look:  fakeLookPath("notify-send"),
			title: "Done", body: "all good",
			wantName: "/usr/bin/notify-send",
			wantArgs: []string{"--app-name=Gogent", "Done", "all good"},
		},
		{
			name:  "freebsd uses notify-send too",
			goos:  "freebsd",
			look:  fakeLookPath("notify-send"),
			title: "T", body: "B",
			wantName: "/usr/bin/notify-send",
			wantArgs: []string{"--app-name=Gogent", "T", "B"},
		},
		{
			name:  "macos terminal-notifier",
			goos:  "darwin",
			look:  fakeLookPath("terminal-notifier"),
			title: "Done", body: "all good",
			wantName: "/usr/bin/terminal-notifier",
			wantArgs: []string{"-title", "Gogent", "-subtitle", "Done", "-message", "all good"},
		},
		{
			name:       "linux without notify-send -> none",
			goos:       "linux",
			look:       fakeLookPath(),
			wantAbsent: true,
		},
		{
			name:       "unsupported platform -> none",
			goos:       "windows",
			look:       fakeLookPath("notify-send", "terminal-notifier"),
			wantAbsent: true,
		},
		{
			name:  "title sanitized before arg build",
			goos:  "linux",
			look:  fakeLookPath("notify-send"),
			title: "a\nb", body: "c\rd",
			wantName: "/usr/bin/notify-send",
			wantArgs: []string{"--app-name=Gogent", "a b", "c d"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, args := nativeCommand(tc.goos, tc.look, tc.title, tc.body)
			if tc.wantAbsent {
				if name != "" {
					t.Errorf("expected no native command, got %q %v", name, args)
				}
				return
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if len(args) != len(tc.wantArgs) {
				t.Fatalf("args = %v, want %v", args, tc.wantArgs)
			}
			for i := range tc.wantArgs {
				if args[i] != tc.wantArgs[i] {
					t.Errorf("args[%d] = %q, want %q", i, args[i], tc.wantArgs[i])
				}
			}
		})
	}
}

func TestShouldNotify(t *testing.T) {
	all := config.DefaultNotifyConfig() // enabled, every event on, no suppress
	suppressed := all
	suppressed.SuppressWhenFocused = true
	disabled := all
	disabled.Enabled = false
	completeOff := all
	completeOff.OnComplete = false

	for _, tc := range []struct {
		name    string
		cfg     config.NotifyConfig
		reason  Reason
		focused bool
		want    bool
	}{
		{"enabled unfocused fires", all, ReasonComplete, false, true},
		{"enabled focused fires when not suppressed", all, ReasonComplete, true, true},
		{"enabled focused suppressed", suppressed, ReasonComplete, true, false},
		{"enabled unfocused not suppressed even with flag", suppressed, ReasonError, false, true},
		{"master disabled", disabled, ReasonError, false, false},
		{"per-event off", completeOff, ReasonComplete, false, false},
		{"other event still on when one off", completeOff, ReasonError, false, true},
		// Suppression is uniform across reasons; approval is no exception. The UI
		// passes focused=false for approval (the permission prompter has no
		// session context), so this branch never fires in practice, but the
		// contract stays simple and predictable.
		{"approval suppressed like any reason when focused", suppressed, ReasonApproval, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := New(tc.cfg, &strings.Builder{})
			if got := n.ShouldNotify(tc.reason, tc.focused); got != tc.want {
				t.Errorf("ShouldNotify(%q, focused=%v) = %v, want %v", tc.reason, tc.focused, got, tc.want)
			}
		})
	}
}

func TestReasonEnabledIgnoresFocus(t *testing.T) {
	suppressed := config.DefaultNotifyConfig()
	suppressed.SuppressWhenFocused = true
	n := New(suppressed, &strings.Builder{})
	// ReasonEnabled must not be affected by SuppressWhenFocused; only ShouldNotify
	// applies focus gating.
	if !n.ReasonEnabled(ReasonComplete) {
		t.Error("ReasonComplete should be enabled regardless of focus suppression")
	}
}

// recordingNative is a nativeRunner that records every invocation under a lock
// so concurrent Notify calls can be observed safely.
type recordingNative struct {
	mu    sync.Mutex
	calls []nativeCall
}

type nativeCall struct {
	title, body string
}

func (r *recordingNative) run(title, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, nativeCall{title, body})
	return nil
}

// waitForCalls polls the recorder until it has at least want native invocations
// or a short deadline passes. Native notifications run in a goroutine, so the
// call lands shortly after Notify returns.
func waitForCalls(rec *recordingNative, want int) bool {
	for i := 0; i < 200; i++ {
		rec.mu.Lock()
		n := len(rec.calls)
		rec.mu.Unlock()
		if n >= want {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestNotifyChannels(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cfg         config.NotifyConfig
		wantBell    bool
		wantDesktop bool
		wantNative  bool
	}{
		{
			name:     "bell and desktop both on",
			cfg:      config.NotifyConfig{Enabled: true, Bell: true, Desktop: true},
			wantBell: true, wantDesktop: true,
		},
		{
			name:     "bell only",
			cfg:      config.NotifyConfig{Enabled: true, Bell: true, Desktop: false, Native: false},
			wantBell: true, wantDesktop: false,
		},
		{
			name:     "desktop only",
			cfg:      config.NotifyConfig{Enabled: true, Bell: false, Desktop: true},
			wantBell: false, wantDesktop: true,
		},
		{
			name:     "native only writes nothing to terminal",
			cfg:      config.NotifyConfig{Enabled: true, Bell: false, Desktop: false, Native: true},
			wantBell: false, wantDesktop: false, wantNative: true,
		},
		{
			name: "all channels off writes nothing",
			cfg:  config.NotifyConfig{Enabled: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := &strings.Builder{}
			rec := &recordingNative{}
			n := New(tc.cfg, buf)
			n.native = rec.run
			n.Notify("Task complete", "Done")

			out := buf.String()
			// The bell is written first, so it shows up as a leading BEL. (We
			// can't detect it by substring: OSC sequences also terminate in BEL.)
			hasBell := strings.HasPrefix(out, "\x07")
			if hasBell != tc.wantBell {
				t.Errorf("bell in output=%v, want %v (output %q)", hasBell, tc.wantBell, out)
			}
			hasDesktop := strings.Contains(out, "\x1b]9;") || strings.Contains(out, "\x1b]777;")
			if hasDesktop != tc.wantDesktop {
				t.Errorf("desktop in output=%v, want %v (output %q)", hasDesktop, tc.wantDesktop, out)
			}
			gotNative := false
			if tc.wantNative {
				gotNative = waitForCalls(rec, 1)
			} else {
				rec.mu.Lock()
				gotNative = len(rec.calls) > 0
				rec.mu.Unlock()
			}
			if gotNative != tc.wantNative {
				t.Errorf("native invoked=%v, want %v", gotNative, tc.wantNative)
			}
		})
	}
}

// TestNotifyNativePayload asserts the native notifier receives the sanitized
// title/body (and waits for the goroutine via the small call count).
func TestNotifyNativePayload(t *testing.T) {
	buf := &strings.Builder{}
	rec := &recordingNative{}
	n := New(config.NotifyConfig{Enabled: true, Native: true}, buf)
	n.native = rec.run
	n.Notify("Task error", "boom\nnext")

	if !waitForCalls(rec, 1) {
		t.Fatal("native notifier was not invoked")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.calls[0].title != "Task error" {
		t.Errorf("native title = %q, want %q", rec.calls[0].title, "Task error")
	}
	if rec.calls[0].body != "boom next" {
		t.Errorf("native body = %q, want %q", rec.calls[0].body, "boom next")
	}
}

// TestSetConfigConcurrentReads is a sanity check that updating the config while
// Notify reads it does not panic or corrupt the decision (the cfg is copied out
// under the lock). Run without -race in CI, but the lock makes it correct.
func TestSetConfigConcurrentReads(t *testing.T) {
	n := New(config.DefaultNotifyConfig(), &strings.Builder{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			cfg := config.DefaultNotifyConfig()
			cfg.OnError = i%2 == 0
			n.SetConfig(cfg)
		}(i)
		go func() {
			defer wg.Done()
			_ = n.Config()
			_ = n.ShouldNotify(ReasonError, false)
		}()
	}
	wg.Wait()
}
