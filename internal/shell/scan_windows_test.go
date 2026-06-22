//go:build windows

package shell

import (
	"reflect"
	"testing"
)

func TestExternalRootsInsideWorkspaceWindows(t *testing.T) {
	ws := `C:\Users\user\project`
	got := ExternalRoots("rm -rf build/cache && cat ./src/main.go", ws)
	if len(got) != 0 {
		t.Fatalf("expected no external roots, got %v", got)
	}
}

func TestExternalRootsAbsoluteOutsideWindows(t *testing.T) {
	ws := `C:\Users\user\project`
	// Drive-qualified, forward-slash paths are recognised as absolute on Windows.
	got := ExternalRoots("cat C:/Windows/win.ini", ws)
	want := []string{`C:\Windows`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestExternalRootsParentEscapeWindows(t *testing.T) {
	ws := `C:\Users\user\project`
	got := ExternalRoots("cat ../../secret.txt", ws)
	want := []string{`C:\Users`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestExternalRootsIgnoresFlagsAndGlobsWindows(t *testing.T) {
	ws := `C:\Users\user\project`
	got := ExternalRoots("ls -la --color=auto *.go", ws)
	if len(got) != 0 {
		t.Fatalf("expected no external roots, got %v", got)
	}
}
