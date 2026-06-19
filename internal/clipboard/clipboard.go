// Package clipboard copies text to the system clipboard (issue #62).
//
// The primary mechanism is an OSC 52 escape sequence written to the terminal,
// which capable terminal emulators honor — including over SSH, where a local
// clipboard utility would instead target the remote machine. When a native
// clipboard utility is on $PATH the text is also piped to it as a local fallback
// for terminals that ignore OSC 52. Over SSH that utility simply finds no display
// server and fails silently, so the two channels never conflict.
//
// The package is UI-agnostic: callers decide what to copy (see Board.Copy).
package clipboard

import (
	"encoding/base64"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// bel is the ASCII BEL control character, the most widely supported OSC sequence
// terminator (some terminals want ST, ESC \, but BEL is the safe common case).
const bel = "\x07"

// maxBytes caps a copied payload so an enormous yank can't emit a sequence a
// terminal rejects. OSC 52 limits vary by emulator; a few hundred KiB is
// universally safe, and well above any single message or code block.
const maxBytes = 256 * 1024

// osc52Sequence builds the OSC 52 clipboard-set escape sequence for text:
//
//	ESC ] 52 ; c ; <base64> BEL
//
// The 'c' selects the system clipboard. The payload is base64-encoded so
// newlines, quotes and control bytes are safe inside the sequence (no escaping
// of the text itself is needed). Payloads over maxBytes are truncated.
func osc52Sequence(text string) string {
	if len(text) > maxBytes {
		text = text[:maxBytes]
	}
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + bel
}

// lookPathFunc resolves a binary name to an executable path (or an error). It
// mirrors exec.LookPath and is a seam so nativeCommand can be unit tested without
// touching the real $PATH.
type lookPathFunc func(file string) (string, error)

// nativeCommand selects the platform's clipboard utility for the given GOOS,
// using look to find it on $PATH, and returns the args that read the text from
// stdin. It returns ("", nil) when the platform has no supported utility or it
// is not installed. macOS uses pbcopy; Wayland uses wl-copy (read by default)
// and X11 uses xclip -selection clipboard; Windows uses clip.
func nativeCommand(goos string, look lookPathFunc) (string, []string) {
	switch goos {
	case "darwin":
		if p, err := look("pbcopy"); err == nil {
			return p, nil
		}
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		if p, err := look("wl-copy"); err == nil {
			return p, nil
		}
		if p, err := look("xclip"); err == nil {
			return p, []string{"-selection", "clipboard"}
		}
	case "windows":
		if p, err := look("clip"); err == nil {
			return p, nil
		}
	}
	return "", nil
}

// nativeRunner pipes text to a clipboard utility. It is a seam so Board can be
// tested without spawning real processes.
type nativeRunner func(text string) error

// defaultNative runs the platform clipboard utility (a no-op when none is
// installed), feeding it the text on stdin.
func defaultNative(text string) error {
	name, args := nativeCommand(runtime.GOOS, exec.LookPath)
	if name == "" {
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// Board copies text to the system clipboard. It is safe for concurrent use:
// terminal writes are serialized so two copies can't interleave their escape
// sequences.
type Board struct {
	writeMu sync.Mutex
	out     io.Writer
	native  nativeRunner
}

// New builds a Board that writes OSC 52 to out (os.Stdout in production) and,
// when a native clipboard utility is available, pipes the text to it.
func New(out io.Writer) *Board {
	return &Board{out: out, native: defaultNative}
}

// Copy writes text to the clipboard. It emits the OSC 52 sequence synchronously
// (it is tiny) and runs the native utility, if any, in its own goroutine so a
// slow or absent binary never stalls the caller. Best-effort: errors are
// swallowed because there is no reliable channel to surface them and an
// unsupported terminal silently ignores OSC 52.
func (b *Board) Copy(text string) {
	if b == nil {
		return
	}
	b.writeMu.Lock()
	_, _ = io.WriteString(b.out, osc52Sequence(text))
	b.writeMu.Unlock()
	if b.native != nil {
		runner := b.native
		go func() { _ = runner(text) }()
	}
}
