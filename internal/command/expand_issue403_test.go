package command

import (
	"strings"
	"testing"

	"gogent/internal/config"
)

func TestIssue403ExpandBindsPositionalNamedMixedAndPlaceholders(t *testing.T) {
	params := []config.CommandParam{
		{Name: "name", Required: true},
		{Name: "dir", Default: "src/components"},
		{Name: "kind", Default: "view"},
	}
	template := "Create ${name}Type in $dir as $kind. Unknown stays $missing."

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "positional uses declaration order and defaults",
			args: []string{"Button"},
			want: "Create ButtonType in src/components as view. Unknown stays $missing.",
		},
		{
			name: "positional fills multiple params",
			args: []string{"Button", "src/widgets"},
			want: "Create ButtonType in src/widgets as view. Unknown stays $missing.",
		},
		{
			name: "named args can supply required and optional params",
			args: []string{"dir=app/ui", "name=Card", "kind=container"},
			want: "Create CardType in app/ui as container. Unknown stays $missing.",
		},
		{
			name: "named args override earlier positional bindings",
			args: []string{"Button", "src/widgets", "name=IconButton"},
			want: "Create IconButtonType in src/widgets as view. Unknown stays $missing.",
		},
		{
			name: "empty named value overrides optional default",
			args: []string{"Button", "dir="},
			want: "Create ButtonType in  as view. Unknown stays $missing.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Expand(template, params, tc.args)
			if err != nil {
				t.Fatalf("Expand returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expanded prompt = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIssue403ExpandMissingRequiredErrorsAndDoesNotExpand(t *testing.T) {
	_, err := Expand("Review $target in $mode", []config.CommandParam{
		{Name: "target", Required: true},
		{Name: "mode", Default: "quick"},
	}, nil)
	if err == nil {
		t.Fatal("missing required parameter should error")
	}
	if !strings.Contains(err.Error(), "target") {
		t.Fatalf("error %q should name the missing parameter", err)
	}
}

func TestIssue403ValidateTemplateWarnsForDistinctUnknownReferences(t *testing.T) {
	warnings := ValidateTemplate("$known $missing ${missing} ${other} $other", []config.CommandParam{
		{Name: "known"},
	})
	if len(warnings) != 2 {
		t.Fatalf("warnings = %#v, want two distinct unknown references", warnings)
	}
	for _, want := range []string{"missing", "other"} {
		var found bool
		for _, w := range warnings {
			if strings.Contains(w, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("warnings %#v missing reference %q", warnings, want)
		}
	}
}

func TestIssue403ReservedNamesIncludesAllBuiltinCommandFamilies(t *testing.T) {
	reserved := ReservedNames()
	for _, name := range []string{
		"undo", "rewind", "fork", "plan", "yolo", "act", "stop", "clearqueue",
		"goal", "markdown", "thinking", "watcher",
		"calc", "echo", "help",
		"read", "write", "edit",
	} {
		if !reserved[name] {
			t.Errorf("ReservedNames() missing %q", name)
		}
	}
}
