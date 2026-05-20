package kinbrowser

import (
	"fmt"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// diffArchived compares an archived markdown body against a freshly-
// fetched one and returns one of three Results:
//
//  1. Unchanged (similarity ≥ minSimilarity): return the archived
//     Result as-is. Save the LLM a re-read.
//  2. Drift (some lines changed): return a header noting the change +
//     a line-level diff + the new content. LLM can decide whether
//     the diff matters or treat the fresh as authoritative.
//  3. Major rewrite (similarity below threshold): treat as new content,
//     return fresh with a "this page was rewritten since archive" note.
//
// Similarity is measured by line-level Jaccard (shared / union). Cheap,
// no embeddings, sensitive to real edits, robust to whitespace.
//
// minSimilarity tuned at 0.85: 85% of lines unchanged → "unchanged".
// Catches typo fixes, minor revisions; flags substantive rewrites.
const minSimilarity = 0.85

func (b *Browser) reconcileArchived(archived Result, fresh Result) Result {
	if archived.Markdown == "" {
		return fresh
	}
	if fresh.Markdown == "" {
		return archived
	}

	sim := lineSimilarity(archived.Markdown, fresh.Markdown)
	if sim >= minSimilarity {
		// Unchanged enough to treat as cache hit. Preserve the original
		// FetchedAt (when archive was made) so the LLM sees how stale
		// the content really is.
		archived.Markdown = fmt.Sprintf(
			"> [UNCHANGED since archive — %.0f%% line match. archived %s]\n\n%s",
			sim*100,
			archived.FetchedAt.Format("2006-01-02 15:04 MST"),
			archived.Markdown,
		)
		return archived
	}

	// Content drifted enough to warn the LLM. Show a compact diff so
	// the model can reason about WHAT changed without re-reading the
	// whole page. Cap the diff size to keep tool-result reasonable.
	diff := computeDiff(archived.Markdown, fresh.Markdown, 4000)
	fresh.Markdown = fmt.Sprintf(
		"> [CHANGED since archive — %.0f%% line match. archived %s]\n"+
			"> Below: unified diff (truncated to 4 KB), then full new content.\n\n"+
			"```diff\n%s\n```\n\n%s",
		sim*100,
		archived.FetchedAt.Format("2006-01-02 15:04 MST"),
		diff,
		fresh.Markdown,
	)
	return fresh
}

// lineSimilarity returns the Jaccard similarity of the line sets of a
// and b. 1.0 = identical, 0.0 = no shared lines. Cheap O(n).
func lineSimilarity(a, b string) float64 {
	aLines := splitNonEmpty(a)
	bLines := splitNonEmpty(b)
	if len(aLines) == 0 && len(bLines) == 0 {
		return 1.0
	}
	aSet := make(map[string]struct{}, len(aLines))
	for _, l := range aLines {
		aSet[l] = struct{}{}
	}
	intersect := 0
	bSet := make(map[string]struct{}, len(bLines))
	for _, l := range bLines {
		if _, ok := aSet[l]; ok {
			intersect++
		}
		bSet[l] = struct{}{}
	}
	union := len(aSet) + len(bSet) - intersect
	if union == 0 {
		return 1.0
	}
	return float64(intersect) / float64(union)
}

// splitNonEmpty splits on newline and drops empty/whitespace-only
// lines. Frontmatter / blank lines don't move the similarity needle.
func splitNonEmpty(s string) []string {
	raw := strings.Split(s, "\n")
	out := raw[:0]
	for _, l := range raw {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// computeDiff returns a unified-style diff between archived and fresh
// markdown, truncated to maxBytes. We use diffmatchpatch's line-mode
// for stable, human-readable output (vs char-level which is too noisy
// for prose).
func computeDiff(archived, fresh string, maxBytes int) string {
	dmp := diffmatchpatch.New()

	// Convert to line-mode (DiffLinesToChars) before computing; map back
	// after. Standard pattern for line-granularity diffs that the human
	// (or LLM) actually wants to read.
	a, b, lineArray := dmp.DiffLinesToChars(archived, fresh)
	diffs := dmp.DiffMain(a, b, false)
	diffs = dmp.DiffCharsToLines(diffs, lineArray)

	var sb strings.Builder
	for _, d := range diffs {
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			for _, line := range strings.SplitAfter(d.Text, "\n") {
				if line == "" {
					continue
				}
				sb.WriteString("+ ")
				sb.WriteString(line)
			}
		case diffmatchpatch.DiffDelete:
			for _, line := range strings.SplitAfter(d.Text, "\n") {
				if line == "" {
					continue
				}
				sb.WriteString("- ")
				sb.WriteString(line)
			}
			// DiffEqual omitted — would balloon the output.
		}
		if sb.Len() > maxBytes {
			sb.WriteString("\n... [diff truncated at " + fmt.Sprintf("%d bytes", maxBytes) + "]\n")
			break
		}
	}
	return sb.String()
}
