package kinbrowser

import (
	"strings"
	"testing"
	"time"
)

func TestLineSimilarity(t *testing.T) {
	tests := []struct {
		a, b    string
		wantMin float64 // similarity must be >= this
		wantMax float64 // and <= this
	}{
		// Identical
		{"foo\nbar\nbaz", "foo\nbar\nbaz", 1.0, 1.0},
		// No overlap
		{"foo\nbar", "qux\nzot", 0.0, 0.0},
		// 2/3 lines shared (intersect=2, union=4) → 0.5
		{"foo\nbar\nbaz", "foo\nbar\nqux", 0.45, 0.55},
		// Blank lines don't count
		{"foo\n\n\nbar", "foo\nbar", 0.99, 1.0},
		// Both empty
		{"", "", 1.0, 1.0},
	}
	for _, tt := range tests {
		got := lineSimilarity(tt.a, tt.b)
		if got < tt.wantMin || got > tt.wantMax {
			t.Errorf("lineSimilarity(%q, %q) = %.3f, want %.3f..%.3f",
				tt.a, tt.b, got, tt.wantMin, tt.wantMax)
		}
	}
}

func TestReconcileArchived_Unchanged(t *testing.T) {
	b, _ := New()
	archived := Result{
		Markdown:    "Line one.\nLine two.\nLine three.\nLine four.\nLine five.",
		FetchedAt:   time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC),
		FromArchive: true,
	}
	fresh := Result{
		Markdown: "Line one.\nLine two.\nLine three.\nLine four.\nLine five.",
		Layer:    1,
	}
	out := b.reconcileArchived(archived, fresh)
	if !strings.Contains(out.Markdown, "[UNCHANGED") {
		t.Errorf("identical content should be marked UNCHANGED, got:\n%s", out.Markdown)
	}
	if !strings.Contains(out.Markdown, "Line one.") {
		t.Errorf("body should still be present")
	}
}

func TestReconcileArchived_Drift(t *testing.T) {
	b, _ := New()
	// 60% line match → below 0.85 threshold → drift detected.
	archived := Result{
		Markdown:  "Line one.\nLine two.\nLine three.\nLine four.\nLine five.",
		FetchedAt: time.Now().Add(-24 * time.Hour),
	}
	fresh := Result{
		Markdown: "Line one.\nLine two.\nDIFFERENT three.\nDIFFERENT four.\nLine five.",
		Layer:    1,
	}
	out := b.reconcileArchived(archived, fresh)
	if !strings.Contains(out.Markdown, "[CHANGED") {
		t.Errorf("drift should be marked CHANGED, got:\n%s", out.Markdown)
	}
	if !strings.Contains(out.Markdown, "DIFFERENT") {
		t.Errorf("new content should appear in output")
	}
	if !strings.Contains(out.Markdown, "```diff") {
		t.Errorf("should include unified diff block")
	}
}

func TestReconcileArchived_EmptyArchive(t *testing.T) {
	b, _ := New()
	fresh := Result{Markdown: "fresh content", Layer: 1}
	out := b.reconcileArchived(Result{}, fresh)
	if out.Markdown != "fresh content" {
		t.Errorf("empty archive should return fresh as-is, got: %q", out.Markdown)
	}
}
