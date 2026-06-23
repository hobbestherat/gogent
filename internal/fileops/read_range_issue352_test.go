package fileops

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundContentIssue352LineRangeAndBounds(t *testing.T) {
	content := "one\ntwo\nthree\nfour\nfive\n"

	t.Run("line range", func(t *testing.T) {
		got := BoundContent(content, 2, 2, len(content))
		if got.Content != "two\nthree\n" {
			t.Fatalf("content = %q, want %q", got.Content, "two\nthree\n")
		}
		if got.Offset != 2 || got.Limit != 2 || got.LinesShown != 2 {
			t.Fatalf("range metadata = offset %d limit %d lines %d, want 2/2/2", got.Offset, got.Limit, got.LinesShown)
		}
		if got.TotalLines != 5 || got.TotalBytes != len(content) {
			t.Fatalf("totals = lines %d bytes %d, want 5/%d", got.TotalLines, got.TotalBytes, len(content))
		}
		if !got.Truncated || got.NextOffset != 4 {
			t.Fatalf("paging = truncated %v next %d, want true/4", got.Truncated, got.NextOffset)
		}
	})

	t.Run("offset below one clamps to first line", func(t *testing.T) {
		got := BoundContent(content, -12, 2, len(content))
		if got.Content != "one\ntwo\n" {
			t.Fatalf("content = %q, want first two lines", got.Content)
		}
		if got.Offset != 1 {
			t.Fatalf("offset = %d, want 1", got.Offset)
		}
	})

	t.Run("large limit clamps at eof", func(t *testing.T) {
		got := BoundContent(content, 4, 99, len(content))
		if got.Content != "four\nfive\n" {
			t.Fatalf("content = %q, want final two lines", got.Content)
		}
		if got.LinesShown != 2 || got.NextOffset != 0 {
			t.Fatalf("lines/next = %d/%d, want 2/0", got.LinesShown, got.NextOffset)
		}
		if !got.Truncated {
			t.Fatal("reading from a non-first offset should be marked truncated because earlier lines were skipped")
		}
	})
}

func TestBoundContentIssue352MaxLength(t *testing.T) {
	t.Run("byte cap truncates and pages at whole line boundary", func(t *testing.T) {
		got := BoundContent("alpha\nbravo\ncharlie\n", 1, 10, len("alpha\nbr"))
		if got.Content != "alpha\nbr" {
			t.Fatalf("content = %q, want capped prefix", got.Content)
		}
		if got.LinesShown != 1 {
			t.Fatalf("lines shown = %d, want only the complete first line counted", got.LinesShown)
		}
		if !got.Truncated || got.NextOffset != 2 {
			t.Fatalf("paging = truncated %v next %d, want true/2", got.Truncated, got.NextOffset)
		}
	})

	t.Run("character cap keeps valid utf8", func(t *testing.T) {
		got := BoundContent("αβγ\nnext\n", 1, 10, 3)
		if !utf8.ValidString(got.Content) {
			t.Fatalf("content is not valid utf8: %q", got.Content)
		}
		if got.Content != "αβγ" {
			t.Fatalf("content = %q, want a 3-character prefix cut on a rune boundary", got.Content)
		}
		if got.LinesShown != 0 || got.NextOffset != 1 {
			t.Fatalf("lines/next = %d/%d, want 0/1 so caller can retry with a larger max_length", got.LinesShown, got.NextOffset)
		}
	})

	t.Run("max length counts characters", func(t *testing.T) {
		got := BoundContent("αβγ\nnext\n", 1, 10, 3)
		if got.Content != "αβγ" {
			t.Fatalf("content = %q, want first 3 characters %q", got.Content, "αβγ")
		}
		if !got.Truncated || got.NextOffset != 1 {
			t.Fatalf("paging = truncated %v next %d, want true/1", got.Truncated, got.NextOffset)
		}
	})
}

func TestBoundContentIssue352OffsetPastEOFClamps(t *testing.T) {
	got := BoundContent("one\ntwo\nthree\n", 99, 2, 1024)
	if got.Offset != 3 {
		t.Fatalf("offset = %d, want clamped final line offset 3", got.Offset)
	}
	if got.Content != "three\n" {
		t.Fatalf("content = %q, want final line after clamping", got.Content)
	}
	if got.LinesShown != 1 || got.NextOffset != 0 {
		t.Fatalf("lines/next = %d/%d, want 1/0", got.LinesShown, got.NextOffset)
	}
	if !got.Truncated {
		t.Fatal("clamped past-EOF read should still report truncation because earlier lines were skipped")
	}
}

func TestBoundContentIssue352DefaultCapAndSmallFile(t *testing.T) {
	t.Run("small file unchanged with defaults", func(t *testing.T) {
		const content = "short\nfile\n"
		got := BoundContent(content, 0, 0, 0)
		if got.Content != content {
			t.Fatalf("content = %q, want unchanged %q", got.Content, content)
		}
		if got.Truncated || got.NextOffset != 0 {
			t.Fatalf("truncation = %v next %d, want false/0", got.Truncated, got.NextOffset)
		}
	})

	t.Run("default line cap bounds overlarge file", func(t *testing.T) {
		line := "x\n"
		content := strings.Repeat(line, DefaultReadMaxLines+3)
		got := BoundContent(content, 0, 0, 0)
		if got.LinesShown != DefaultReadMaxLines {
			t.Fatalf("lines shown = %d, want default cap %d", got.LinesShown, DefaultReadMaxLines)
		}
		if got.Content != strings.Repeat(line, DefaultReadMaxLines) {
			t.Fatal("content was not capped at the default line limit")
		}
		if !got.Truncated || got.NextOffset != DefaultReadMaxLines+1 {
			t.Fatalf("paging = truncated %v next %d, want true/%d", got.Truncated, got.NextOffset, DefaultReadMaxLines+1)
		}
	})

	t.Run("default character cap bounds dense ascii file", func(t *testing.T) {
		content := strings.Repeat("a", DefaultReadMaxBytes+10)
		got := BoundContent(content, 0, 0, 0)
		if len(got.Content) != DefaultReadMaxBytes {
			t.Fatalf("content bytes = %d, want default byte cap %d", len(got.Content), DefaultReadMaxBytes)
		}
		if !got.Truncated || got.NextOffset != 1 {
			t.Fatalf("paging = truncated %v next %d, want true/1", got.Truncated, got.NextOffset)
		}
	})

	t.Run("default character cap counts utf8 runes", func(t *testing.T) {
		content := strings.Repeat("α", DefaultReadMaxBytes+1)
		got := BoundContent(content, 0, 0, 0)
		if got.Content != strings.Repeat("α", DefaultReadMaxBytes) {
			t.Fatalf("content rune count = %d, want default cap %d", utf8.RuneCountInString(got.Content), DefaultReadMaxBytes)
		}
		if !got.Truncated || got.NextOffset != 1 {
			t.Fatalf("paging = truncated %v next %d, want true/1", got.Truncated, got.NextOffset)
		}
	})
}
