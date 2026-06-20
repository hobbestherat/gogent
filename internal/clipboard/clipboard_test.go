package clipboard

import (
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestOSC52Sequence(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"plain text", "hello world"},
		{"empty", ""},
		{"newlines and quotes", "line one\nline \"two\"\ttab"},
		{"non-ascii", "Gögent — ✓"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := osc52Sequence(tc.text)
			wantPrefix := "\x1b]52;c;"
			wantSuffix := bel
			if !strings.HasPrefix(got, wantPrefix) {
				t.Fatalf("missing OSC 52 prefix: %q", got)
			}
			if !strings.HasSuffix(got, wantSuffix) {
				t.Fatalf("missing BEL terminator: %q", got)
			}
			payload := strings.TrimSuffix(strings.TrimPrefix(got, wantPrefix), wantSuffix)
			decoded, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				t.Fatalf("payload is not valid base64 (%q): %v", payload, err)
			}
			if string(decoded) != tc.text {
				t.Errorf("decoded payload = %q, want %q", decoded, tc.text)
			}
		})
	}
}

func TestOSC52SequenceTruncates(t *testing.T) {
	huge := strings.Repeat("x", maxBytes*2)
	got := osc52Sequence(huge)
	payload := strings.TrimSuffix(strings.TrimPrefix(got, "\x1b]52;c;"), bel)
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload not valid base64: %v", err)
	}
	if len(decoded) != maxBytes {
		t.Errorf("truncated payload len = %d, want %d", len(decoded), maxBytes)
	}
}

func TestNativeCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		goos     string
		look     lookPathFunc
		wantName string
	}{
		{"macos pbcopy", "darwin", fakeLookPath("pbcopy"), "pbcopy"},
		{"macos none installed", "darwin", fakeLookPath(), ""},
		{"linux wayland preferred", "linux", fakeLookPath("xclip", "wl-copy"), "wl-copy"},
		{"linux xclip fallback", "linux", fakeLookPath("xclip"), "xclip"},
		{"linux none installed", "linux", fakeLookPath(), ""},
		{"windows clip", "windows", fakeLookPath("clip"), "clip"},
		{"unsupported goos", "solaris", fakeLookPath("pbcopy"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, args := nativeCommand(tc.goos, tc.look)
			if tc.wantName == "" {
				if name != "" {
					t.Fatalf("nativeCommand = %q %v, want no utility", name, args)
				}
				return
			}
			if !strings.HasSuffix(name, tc.wantName) {
				t.Fatalf("nativeCommand name = %q, want *%q", name, tc.wantName)
			}
			// xclip selects the clipboard explicitly; the others need no args.
			if tc.wantName == "xclip" {
				if len(args) != 2 || args[0] != "-selection" || args[1] != "clipboard" {
					t.Errorf("xclip args = %v, want [-selection clipboard]", args)
				}
			}
		})
	}
}

func TestCopyWritesOSC52AndRunsNative(t *testing.T) {
	done := make(chan string, 1)
	board := &Board{
		out:    &buf{},
		native: func(text string) error { done <- text; return nil },
	}

	board.Copy("copy me")

	out := board.out.(*buf).String()
	if !strings.HasPrefix(out, "\x1b]52;c;") || !strings.HasSuffix(out, bel) {
		t.Errorf("Copy did not write an OSC 52 sequence: %q", out)
	}
	// The native runner runs in its own goroutine; wait for it to fire.
	select {
	case ran := <-done:
		if ran != "copy me" {
			t.Errorf("native runner got %q, want %q", ran, "copy me")
		}
	case <-time.After(time.Second):
		t.Fatal("native runner was not invoked")
	}
}

func TestCopyNilBoardIsNoOp(t *testing.T) {
	var b *Board
	b.Copy("anything") // must not panic
}

func TestCopyNilNativeOnlyWritesSequence(t *testing.T) {
	board := &Board{out: &buf{}, native: nil}
	board.Copy("just osc")
	out := board.out.(*buf).String()
	if !strings.HasPrefix(out, "\x1b]52;c;") {
		t.Errorf("Copy did not write OSC 52 without a native runner: %q", out)
	}
}

// buf is a minimal io.Writer for tests.
type buf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *buf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
