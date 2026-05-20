package kinbrowser

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"
)

// collapseWS — collapse runs of internal whitespace inside extracted
// PDF text. PDF tokenization tends to insert excessive spaces between
// glyphs ("e n c o d e r"); this regex normalizes them while keeping
// paragraph breaks (\n\n) intact.
var collapseWS = regexp.MustCompile(`[ \t]{2,}`)

// Word-boundary heuristics — pure-Go pdf libs strip word spacing,
// leaving "AttentionIsAllYouNeed" instead of "Attention Is All You
// Need". These regex insert spaces at likely boundaries:
//
//	lowercase→Uppercase    AttentionIs   → Attention Is
//	letter→digit           Section4      → Section 4
//	digit→letter           4Conclusion   → 4 Conclusion
//	closingPunct→letter    .Recent       → . Recent
//
// Imperfect — won't help acronym-heavy text, will sometimes wrongly
// split CamelCase identifiers — but on prose-heavy arxiv papers it
// recovers readability dramatically (32 KB compressed → readable).
var (
	splitLowerUpper  = regexp.MustCompile(`([a-z])([A-Z])`)
	splitLetterDigit = regexp.MustCompile(`([a-zA-Z])([0-9])`)
	splitDigitLetter = regexp.MustCompile(`([0-9])([a-zA-Z])`)
	splitPunctLetter = regexp.MustCompile(`([.,;:!?])([a-zA-Z])`)
)

// restoreWordBoundaries inserts spaces at likely word boundaries lost
// during pure-Go PDF text extraction. Best-effort; safe to call on
// already-spaced text (idempotent on prose-shaped input).
func restoreWordBoundaries(s string) string {
	s = splitLowerUpper.ReplaceAllString(s, "$1 $2")
	s = splitLetterDigit.ReplaceAllString(s, "$1 $2")
	s = splitDigitLetter.ReplaceAllString(s, "$1 $2")
	s = splitPunctLetter.ReplaceAllString(s, "$1 $2")
	return s
}

// extractPDF turns a PDF byte stream into a markdown Result. Used by
// fetchHTTP when the response Content-Type is application/pdf.
//
// Why arxiv-shaped:
//   - paper_scout / wanshitong / shelving agents read arxiv all day
//   - arxiv URLs (/abs/...) link to HTML abstracts, but real content
//     is on /pdf/... — without PDF support, kinbrowser was useless
//     for the swarm's #1 reading source
//
// Why pure-Go (ledongthuc/pdf):
//   - No external dep (no poppler / pdftotext install)
//   - Single-binary deployment story unchanged
//   - Markdown output isn't structured (no headings/tables) but the
//     LLM gets the raw text which is what matters for comprehension
//
// Limitations honestly: pure-Go PDF extraction misses some columnar
// layouts (multi-column papers' text order can jumble), embedded
// fonts with custom encoding, and ligatures. For most arxiv papers
// the result is good enough; for visually-complex PDFs (slides, gov
// forms) the result is partial. If quality matters more than zero
// deps, swap to pdftotext via exec — but the dep cost was decided
// against in this design.
func (b *Browser) extractPDF(rawURL string, pdfBytes []byte) (Result, error) {
	// Best path: pdftotext (poppler) if installed. Word boundaries
	// preserved correctly. Common on macOS via `brew install poppler`
	// and on Linux via the poppler-utils package. If absent, fall
	// through to pure-Go pdf lib.
	if body, totalPages, err := extractPDFViaPdftotext(pdfBytes); err == nil {
		return b.buildPDFResult(rawURL, body, totalPages, "pdftotext")
	}

	// Fallback path: pure-Go ledongthuc/pdf. Works without any system
	// dep but loses word spacing on PDFs that draw glyphs by position.
	r, err := pdf.NewReader(bytes.NewReader(pdfBytes), int64(len(pdfBytes)))
	if err != nil {
		return Result{}, fmt.Errorf("pdf parse: %w", err)
	}

	var text strings.Builder
	totalPages := r.NumPage()

	if reader, err := r.GetPlainText(); err == nil {
		if all, err := io.ReadAll(reader); err == nil {
			text.Write(all)
		}
	}

	if text.Len() == 0 {
		for i := 1; i <= totalPages; i++ {
			page := r.Page(i)
			if page.V.IsNull() {
				continue
			}
			content, err := page.GetPlainText(nil)
			if err != nil {
				continue
			}
			text.WriteString(content)
			text.WriteString("\n\n")
		}
	}

	body := strings.TrimSpace(text.String())
	if body == "" {
		return Result{}, fmt.Errorf("pdf %s: no extractable text (image-only or encrypted?)", rawURL)
	}

	// Light cleanup: collapse runs of internal whitespace, drop the
	// PDF-typical "page-break" form-feed character (\f), normalize
	// line endings.
	body = strings.ReplaceAll(body, "\f", "\n\n")
	body = collapseWS.ReplaceAllString(body, " ")
	body = strings.ReplaceAll(body, " \n", "\n")
	// Restore word boundaries lost during pure-Go PDF extraction
	// (e.g. "AttentionIsAllYouNeed" → "Attention Is All You Need").
	body = restoreWordBoundaries(body)

	// Pull title from PDF info dict if present.
	titleOverride := ""
	if t, ok := extractPDFTitle(r); ok && t != "" {
		titleOverride = t
	}
	return b.buildPDFResult(rawURL, body, totalPages, fmt.Sprintf("pure-go (degraded; install poppler for better quality)"), titleOverride)
}

// buildPDFResult is the shared "wrap extracted body as Result"
// helper used by both pdftotext and pure-Go paths.
func (b *Browser) buildPDFResult(rawURL, body string, totalPages int, extractor string, titleOverride ...string) (Result, error) {
	title := titleFromURL(rawURL)
	if len(titleOverride) > 0 && titleOverride[0] != "" {
		title = titleOverride[0]
	}
	header := fmt.Sprintf("> **PDF source** — %d pages from %s (extracted via %s)\n\n",
		totalPages, rawURL, extractor)
	return Result{
		URL:       rawURL,
		Title:     title,
		Markdown:  header + body,
		Layer:     1,
		FetchedAt: time.Now(),
	}, nil
}

// extractPDFViaPdftotext shells out to the poppler-utils `pdftotext`
// binary (brew install poppler on macOS, apt install poppler-utils on
// Linux). It pipes the PDF bytes in via stdin and reads plain text
// out — fast, accurate word spacing, page count.
//
// Returns sentinel error if pdftotext isn't on PATH (so the caller
// falls through to pure-Go).
func extractPDFViaPdftotext(pdfBytes []byte) (body string, pages int, err error) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		return "", 0, fmt.Errorf("pdftotext not on PATH")
	}
	// -layout preserves the document's reading order (better for
	//   multi-column papers than the default left-to-right flow)
	// -enc UTF-8 ensures non-ASCII (CJK, math) comes through cleanly
	// -nopgbrk drops the \f form-feed between pages — we add \n\n
	//   later via the standard cleanup
	cmd := exec.Command("pdftotext", "-layout", "-enc", "UTF-8", "-nopgbrk", "-", "-")
	cmd.Stdin = bytes.NewReader(pdfBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", 0, fmt.Errorf("pdftotext: %w (%s)", err, stderr.String())
	}
	body = strings.TrimSpace(stdout.String())
	if body == "" {
		return "", 0, fmt.Errorf("pdftotext returned empty (encrypted PDF? scan-only?)")
	}
	// Cheap page-count estimate from the PDF header. We could call
	// pdfinfo but that doubles the subprocess cost. The actual count
	// is only displayed in the frontmatter header — close-enough is fine.
	pages = bytes.Count(pdfBytes, []byte("/Type /Page")) + bytes.Count(pdfBytes, []byte("/Type/Page"))
	if pages < 1 {
		pages = 1
	}
	return body, pages, nil
}

// extractPDFTitle pulls Title from the PDF document info dict if set.
// Most arxiv PDFs have this; many older PDFs don't.
func extractPDFTitle(r *pdf.Reader) (string, bool) {
	defer func() { _ = recover() }() // pdf lib panics on some malformed docs
	trailer := r.Trailer()
	info := trailer.Key("Info")
	if info.IsNull() {
		return "", false
	}
	title := info.Key("Title").Text()
	title = strings.TrimSpace(title)
	if title == "" {
		return "", false
	}
	return title, true
}

// titleFromURL — fallback title generator when PDF metadata is empty.
// Pulls the last path component, strips extension, replaces dashes/
// underscores with spaces. e.g.:
//
//	https://arxiv.org/pdf/1706.03762.pdf  →  1706.03762
//	https://example.com/papers/foo_bar.pdf →  foo bar
func titleFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 {
		return u.Host
	}
	last := parts[len(parts)-1]
	last = strings.TrimSuffix(last, ".pdf")
	last = strings.ReplaceAll(last, "-", " ")
	last = strings.ReplaceAll(last, "_", " ")
	return last
}
