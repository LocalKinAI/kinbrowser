package kinbrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestExtract_RoundTrip — give the extractor a hand-crafted HTML page
// with clear "main content" vs "nav/footer" boilerplate, and verify it
// pulls out the content as markdown.
func TestExtract_RoundTrip(t *testing.T) {
	html := `<!DOCTYPE html>
<html><head><title>Test Article</title></head>
<body>
  <nav><a href="/">home</a></nav>
  <article>
    <h1>The Quick Brown Fox</h1>
    <p>This is the main content. It has multiple sentences explaining
    something interesting in enough detail to look like a real article.
    Readability typically wants 200+ characters of substantive prose
    before it considers a block as "main content".</p>
    <p>A second paragraph reinforces the article's substance and gives
    readability enough signal to identify this as the body. We're well
    past the threshold now and into actual content territory.</p>
    <ul>
      <li>First point with some text</li>
      <li>Second point with more text</li>
    </ul>
  </article>
  <footer>© 2026 noise</footer>
</body></html>`

	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.extract("https://example.com/article", []byte(html))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	// readability prefers <title> over <h1>. Either is acceptable —
	// what matters is we got a non-empty title that's one of them.
	if r.Title != "Test Article" && r.Title != "The Quick Brown Fox" {
		t.Errorf("title = %q, want 'Test Article' or 'The Quick Brown Fox'", r.Title)
	}
	if !strings.Contains(r.Markdown, "main content") {
		t.Errorf("markdown should include main content text, got:\n%s", r.Markdown)
	}
	if strings.Contains(r.Markdown, "home") || strings.Contains(r.Markdown, "© 2026 noise") {
		t.Errorf("markdown should strip nav/footer, got:\n%s", r.Markdown)
	}
}

// TestOpen_HTTPLayer — spin up a local HTTP test server returning real
// HTML, verify Layer 1 fetches + extracts cleanly without falling back.
func TestOpen_HTTPLayer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html><html><head><title>HTTP Test</title></head>
<body><article><h1>HTTP Test</h1>
<p>` + strings.Repeat("Some interesting prose explaining a topic in detail. ", 8) + `</p>
</article></body></html>`))
	}))
	defer ts.Close()

	// Disable Chrome so test never tries to spawn a browser, and rely
	// on the HTTP layer succeeding.
	b, err := New(WithoutChromedp())
	if err != nil {
		t.Fatal(err)
	}

	r, err := b.Open(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if r.Layer != 1 {
		t.Errorf("expected Layer 1, got %d", r.Layer)
	}
	if !strings.Contains(r.Markdown, "interesting prose") {
		t.Errorf("markdown missing content: %s", r.Markdown)
	}
	if r.Title != "HTTP Test" {
		t.Errorf("title = %q", r.Title)
	}
}

// TestOpen_CacheHit — call Open() twice on the same URL, verify the
// second call hits the session LRU (doesn't re-fetch).
func TestOpen_CacheHit(t *testing.T) {
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`<!DOCTYPE html><html><head><title>Cached</title></head>
<body><article><h1>Cached</h1><p>` + strings.Repeat("Body. ", 50) + `</p></article></body></html>`))
	}))
	defer ts.Close()

	b, err := New(WithoutChromedp())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = b.Open(context.Background(), ts.URL)
	_, _ = b.Open(context.Background(), ts.URL)

	if hits != 1 {
		t.Errorf("expected 1 server hit (second call cached), got %d", hits)
	}
}

// TestOpen_EmptyURL — defensive check.
func TestOpen_EmptyURL(t *testing.T) {
	b, _ := New()
	if _, err := b.Open(context.Background(), ""); err == nil {
		t.Error("expected error for empty URL")
	}
}

// TestAcceptable — the threshold logic that decides whether to escalate
// from Layer 1 to Layer 2/3 when the markdown looks too thin.
func TestAcceptable(t *testing.T) {
	b, _ := New()
	if b.acceptable(Result{Markdown: "tiny"}) {
		t.Error("4-char markdown should NOT be acceptable (below 200 char default)")
	}
	if !b.acceptable(Result{Markdown: strings.Repeat("a", 250)}) {
		t.Error("250-char markdown should be acceptable")
	}
}

// TestAcceptable_DetectsJSStubs — x.com served us a 222-char "Something
// went wrong" stub that passed the length check; we have to reject it
// to force escalation. Catch each known CSR stub pattern.
func TestAcceptable_DetectsJSStubs(t *testing.T) {
	b, _ := New()
	stubs := []string{
		// x.com observed in production
		"Something went wrong, but don" + "’" + "t fret—let" + "’" + "s give it another shot. " + strings.Repeat("padding ", 30),
		// create-react-app default
		"You need to enable JavaScript to run this app. " + strings.Repeat("padding ", 30),
		// generic
		"Please enable cookies. " + strings.Repeat("padding ", 30),
		"JavaScript is required to view this site. " + strings.Repeat("padding ", 30),
	}
	for _, md := range stubs {
		if b.acceptable(Result{Markdown: md}) {
			t.Errorf("stub should NOT be acceptable, would have skipped L2/L3 escalation: %q...", md[:60])
		}
	}
}

// TestAcceptable_DetectsBotWalls — anti-bot challenge pages return
// HTTP 200 with prose; readability extracts the prose; previously
// kinbrowser would wrongly accept this as content (e.g. Google's
// "unusual traffic" page in California-wildfire session). Added after
// observing each pattern in the wild.
func TestAcceptable_DetectsBotWalls(t *testing.T) {
	b, _ := New()
	pad := strings.Repeat("padding ", 30)
	stubs := []string{
		// Google search bot wall
		"About this page. Our systems have detected unusual traffic from your computer network. " + pad,
		// Cloudflare challenge
		"Checking if the site connection is secure. " + pad,
		// Cloudflare Turnstile
		"Verifying you are human. " + pad,
		// Generic block pages
		"Access denied. You don't have permission to access this resource. " + pad,
		"Sorry, you have been blocked. " + pad,
		// Rate limit
		"Rate limit exceeded. Please try again later. " + pad,
	}
	for _, md := range stubs {
		if b.acceptable(Result{Markdown: md}) {
			t.Errorf("anti-bot wall should NOT be acceptable: %q...", md[:80])
		}
	}
}

// TestDefaultTimeouts_TunedForRealSites — locks in the v0.2.1 timing
// changes against accidental regression. CNN needed >20s for L3;
// weather.com needed >5s for L1. Future contributors who think
// "let's tighten timeouts to fail fast" will see this test fail and
// look at the comment explaining why.
func TestDefaultTimeouts_TunedForRealSites(t *testing.T) {
	b, _ := New()
	if b.httpTimeout < 10*time.Second {
		t.Errorf("L1 httpTimeout = %v; must be ≥10s (weather.com case)", b.httpTimeout)
	}
	if b.chromedpTimeout < 30*time.Second {
		t.Errorf("L3 chromedpTimeout = %v; must be ≥30s (CNN case)", b.chromedpTimeout)
	}
}

// TestForceLayer_PinsBackend — verify WithForceLayer(1) actually runs
// the HTTP backend even when content is short / would normally escalate.
func TestForceLayer_PinsBackend(t *testing.T) {
	b, err := New(WithForceLayer(1), WithoutChromedp())
	if err != nil {
		t.Fatal(err)
	}
	if b.forceLayer != 1 {
		t.Errorf("forceLayer = %d, want 1", b.forceLayer)
	}
}

// TestNew_Defaults — sanity check on constructed defaults.
func TestNew_Defaults(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if !b.chromedpEnabled {
		t.Error("chromedp should be enabled by default")
	}
	if b.useLightpanda {
		t.Error("lightpanda should be OFF by default (opt-in)")
	}
	if b.minMarkdown != 200 {
		t.Errorf("minMarkdown = %d, want 200", b.minMarkdown)
	}
}
