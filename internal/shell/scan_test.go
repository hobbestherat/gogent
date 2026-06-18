package shell

import (
	"reflect"
	"testing"
)

func TestExternalRootsInsideWorkspace(t *testing.T) {
	ws := "/home/user/project"
	got := ExternalRoots("rm -rf build/cache && cat ./src/main.go", ws)
	if len(got) != 0 {
		t.Fatalf("expected no external roots, got %v", got)
	}
}

func TestExternalRootsAbsoluteOutside(t *testing.T) {
	ws := "/home/user/project"
	got := ExternalRoots("rm -rf /etc/passwd", ws)
	want := []string{"/etc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestExternalRootsParentEscape(t *testing.T) {
	ws := "/home/user/project"
	got := ExternalRoots("cat ../../secret.txt", ws)
	want := []string{"/home"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestExternalRootsDistinct(t *testing.T) {
	ws := "/home/user/project"
	got := ExternalRoots("cp /etc/hosts /var/log/out && cat /etc/group", ws)
	want := []string{"/etc", "/var/log"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestExternalRootsIgnoresFlagsAndGlobs(t *testing.T) {
	ws := "/home/user/project"
	got := ExternalRoots("ls -la --color=auto *.go", ws)
	if len(got) != 0 {
		t.Fatalf("expected no external roots, got %v", got)
	}
}
