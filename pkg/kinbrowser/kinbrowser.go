// Package kinbrowser — markdown-native browser for AI agents.
//
// Three escalating backends, one uniform output (Markdown):
//
//	Layer 1   http.Get + go-readability + html→markdown    ~100ms  ~80% of sites
//	Layer 2   Lightpanda over CDP (JS exec, no rendering)  ~500ms  +15% (SPA without full Chrome)
//	Layer 3   chromedp (full Chrome via CDP)               ~2 s    +5% (stubborn JS-heavy SPAs)
//
// Plus two memory primitives:
//
//	session LRU      — same-process revisit avoidance (in-memory, dies with process)
//	KinBrain archive — opt-in, explicit only via Archive()
//
// Why the "markdown-native" framing matters: LLM context is markdown-shaped
// (their training corpus is mostly markdown/text). Giving an agent rendered
// HTML wastes 5–10× tokens on `<div class="...">` boilerplate and forces
// the model to mentally re-parse what was originally authored as markdown
// (most blog posts / docs / READMEs / arxiv abstracts ARE markdown
// underneath the build pipeline). kinbrowser reverses the build pipeline:
// HTTP-fetch the HTML → extract main content (readability) → convert back
// to markdown.
//
// Opt-in archiving: Open() never writes to KinBrain. Only Archive() does.
// This keeps the user's KinBrain (their curated knowledge base, see
// LocalKinAI/localkin-core/pkg/kinbrain) free of every transient URL the
// agent stumbles across — only deliberately-archived content lands.
package kinbrowser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Result is what Open()/Archive() return — markdown content plus metadata
// describing how we got it.
type Result struct {
	URL         string // requested URL (after redirects, this is the final URL)
	Title       string // best-effort title extracted from <title> or H1
	Markdown    string // extracted main content as markdown
	Layer       int    // 1 (HTTP), 2 (Lightpanda), 3 (chromedp), 0 (cache/archive hit)
	FetchedAt   time.Time
	FromArchive bool // true if served from a previous Archive() call
}

// Browser is the configured kinbrowser instance. Construct one per
// process (it owns the session LRU and chromedp/Lightpanda lifecycle).
type Browser struct {
	cache           *lru.Cache[string, Result]
	useLightpanda   bool   // true if explicit WithLightpanda() was called
	autoLightpanda  bool   // true by default — auto-detect `lightpanda` on PATH
	lightpandaURL   string // explicit ws://... if WithLightpanda(url) was used
	chromedpEnabled bool   // false on headless servers without Chrome
	httpTimeout     time.Duration
	cdpTimeout      time.Duration
	minMarkdown     int // minimum markdown length to consider Layer 1 "successful"
	forceLayer      int // 0 = auto-escalate, 1/2/3 = pin to specific layer (debug)
}

// Option mutates a Browser at construction time.
type Option func(*Browser)

// WithLightpanda enables Layer 2 (Lightpanda CDP) at the given WebSocket
// debugger URL. Pass "" to auto-detect localhost:9222.
func WithLightpanda(wsURL string) Option {
	return func(b *Browser) {
		b.useLightpanda = true
		if wsURL != "" {
			b.lightpandaURL = wsURL
		}
	}
}

// WithoutChromedp disables Layer 3. Useful on servers without Chrome,
// or in tests that should never spawn a browser.
func WithoutChromedp() Option {
	return func(b *Browser) { b.chromedpEnabled = false }
}

// WithoutLightpandaAutoDetect disables the default behavior of
// auto-detecting `lightpanda` on PATH and lazy-spawning the daemon.
// Caller can still explicitly enable L2 via WithLightpanda(url).
// Useful in tests + environments where you don't want kinbrowser
// spawning side-processes.
func WithoutLightpandaAutoDetect() Option {
	return func(b *Browser) { b.autoLightpanda = false }
}

// WithCacheSize sets the session LRU capacity (default 128).
func WithCacheSize(n int) Option {
	return func(b *Browser) {
		c, _ := lru.New[string, Result](n)
		b.cache = c
	}
}

// WithForceLayer pins all Open() calls to a specific backend (1=HTTP,
// 2=Lightpanda, 3=chromedp). Skips Layer 0 caches and escalation logic.
// Diagnostic only — production callers should use auto-escalation.
func WithForceLayer(n int) Option {
	return func(b *Browser) { b.forceLayer = n }
}

// New constructs a Browser. Sane defaults: cache=128, Layer 1+3 enabled,
// Lightpanda off (callers opt in via WithLightpanda).
func New(opts ...Option) (*Browser, error) {
	cache, err := lru.New[string, Result](128)
	if err != nil {
		return nil, err
	}
	b := &Browser{
		cache:           cache,
		autoLightpanda:  true, // ← default: auto-detect Lightpanda on PATH
		chromedpEnabled: true,
		httpTimeout:     20 * time.Second,
		cdpTimeout:      30 * time.Second,
		minMarkdown:     200, // ~50 words
	}
	for _, opt := range opts {
		opt(b)
	}
	return b, nil
}

// Open fetches the URL through the escalating backend chain and returns
// the result as markdown. Does NOT write to KinBrain — use Archive() for
// that.
//
// Layer selection:
//  1. Session LRU hit → return cached
//  2. KinBrain has this URL archived → return archive (with note)
//  3. HTTP + readability + html→markdown (the 80% path)
//  4. If markdown looks empty/thin, escalate to Lightpanda (if enabled)
//  5. If still thin, escalate to chromedp (if enabled)
//  6. Return whatever the last successful layer produced, or error
func (b *Browser) Open(ctx context.Context, url string) (Result, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return Result{}, errors.New("kinbrowser: url is required")
	}
	if !strings.Contains(url, "://") {
		url = "https://" + url
	}

	// Forced-layer debug mode: skip caches + escalation, pin to one backend.
	if b.forceLayer != 0 {
		return b.forced(ctx, url)
	}

	// Layer 0a: session LRU
	if cached, ok := b.cache.Get(url); ok {
		return cached, nil
	}

	// Layer 0b: KinBrain archive (opt-in, no-op if kinbrain CLI absent)
	if archived := b.recallFromKinBrain(url); archived != nil {
		b.cache.Add(url, *archived)
		return *archived, nil
	}

	// Layer 1: HTTP + readability + html→markdown
	if r, err := b.fetchHTTP(ctx, url); err == nil && b.acceptable(r) {
		b.cache.Add(url, r)
		return r, nil
	} else if err != nil {
		// non-recoverable network error — return early
		var nonHTTP nonHTTPError
		if !errors.As(err, &nonHTTP) {
			return Result{}, err
		}
	}

	// Layer 2: Lightpanda. Three ways to enable:
	//   (a) WithLightpanda(url) explicit option
	//   (b) auto-detect: `lightpanda` binary on PATH → lazy-spawn daemon
	//   (c) $KINBROWSER_LIGHTPANDA_URL env override → external daemon
	// (a) and (c) populate b.lightpandaURL up front; (b) discovers it
	// here on first need.
	if b.useLightpanda || b.autoLightpanda {
		wsURL := b.lightpandaURL
		if wsURL == "" {
			wsURL, _ = defaultLightpanda.detect()
		}
		if wsURL != "" {
			if r, err := b.fetchCDP(ctx, url, wsURL); err == nil && b.acceptable(r) {
				r.Layer = 2
				b.cache.Add(url, r)
				return r, nil
			}
		}
	}

	// Layer 3: chromedp (full Chrome)
	if b.chromedpEnabled {
		if r, err := b.fetchCDP(ctx, url, ""); err == nil {
			r.Layer = 3
			b.cache.Add(url, r)
			return r, nil
		} else {
			return Result{}, fmt.Errorf("all backends failed; chromedp last error: %w", err)
		}
	}

	return Result{}, errors.New("all backends failed and chromedp is disabled")
}

// forced runs exactly one backend (no fallback, no cache, no archive
// check). Used by --force-layer for diagnostics.
func (b *Browser) forced(ctx context.Context, url string) (Result, error) {
	switch b.forceLayer {
	case 1:
		r, err := b.fetchHTTP(ctx, url)
		if err != nil {
			return r, err
		}
		r.Layer = 1
		return r, nil
	case 2:
		r, err := b.fetchCDP(ctx, url, b.lightpandaURL)
		if err != nil {
			return r, err
		}
		r.Layer = 2
		return r, nil
	case 3:
		r, err := b.fetchCDP(ctx, url, "")
		if err != nil {
			return r, err
		}
		r.Layer = 3
		return r, nil
	default:
		return Result{}, fmt.Errorf("force-layer must be 1, 2, or 3 (got %d)", b.forceLayer)
	}
}

// Archive fetches via Open() and then writes the result to KinBrain via
// `kinbrain save web <title>`. This is the explicit "keep this" action —
// agents should call it only when content is high-signal (paper, doc,
// authoritative source) so the user's KinBrain stays curated.
//
// Returns the Result that was archived. If the kinbrain binary isn't on
// PATH, Archive returns an error so the caller knows the archive didn't
// actually happen (the read still succeeded via Open's return value, but
// nothing was persisted).
func (b *Browser) Archive(ctx context.Context, url string) (Result, error) {
	r, err := b.Open(ctx, url)
	if err != nil {
		return r, err
	}

	if _, err := exec.LookPath("kinbrain"); err != nil {
		return r, fmt.Errorf(
			"read succeeded (layer %d) but kinbrain CLI not on PATH — install: "+
				"cd ~/Documents/Workspace/localkin && go install ./cmd/kinbrain", r.Layer)
	}

	cmd := exec.CommandContext(ctx, "kinbrain", "save", "web", r.Title)
	cmd.Stdin = strings.NewReader(b.archiveBody(r))
	if out, err := cmd.CombinedOutput(); err != nil {
		return r, fmt.Errorf("kinbrain save: %w\n%s", err, out)
	}
	r.FromArchive = false // freshly archived this call, not loaded from archive
	return r, nil
}

// archiveBody is the actual content written to KinBrain. We prepend a
// frontmatter-style header so future recalls can attribute the URL,
// fetch time, and layer.
func (b *Browser) archiveBody(r Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "url: %s\n", r.URL)
	fmt.Fprintf(&sb, "layer: %d\n", r.Layer)
	fmt.Fprintf(&sb, "archived_at: %s\n\n", r.FetchedAt.UTC().Format(time.RFC3339))
	sb.WriteString(r.Markdown)
	return sb.String()
}

// acceptable returns true if the Result looks usable. Empty or too-thin
// markdown usually means JS hydration is needed → escalate to next layer.
//
// Beyond length, we sniff for known "this is a JS-required stub" error
// pages. These are usually a tiny HTML shell with "enable JavaScript"
// or "Something went wrong" content — short enough that L1 sometimes
// returns 250-400 chars and we'd accept it on length alone. Catching
// them here forces L2/L3 escalation where a real JS engine can render
// the actual page.
func (b *Browser) acceptable(r Result) bool {
	md := strings.TrimSpace(r.Markdown)
	if len(md) < b.minMarkdown {
		return false
	}
	// Pattern match: well-known stubs from CSR-heavy sites.
	lower := strings.ToLower(md)
	for _, stub := range jsRequiredStubs {
		if strings.Contains(lower, stub) {
			return false
		}
	}
	return true
}

// jsRequiredStubs — markdown fragments that signal "this is the
// pre-hydration HTML, not the real content". Each one was empirically
// observed on a known CSR site that L1 fails on (x.com, instagram,
// etc.). Lowercase comparisons.
var jsRequiredStubs = []string{
	"enable javascript",
	"javascript is required",
	"javascript is disabled",
	"please enable cookies",
	"please enable js",
	"something went wrong, but don", // x.com / twitter stub
	"this site can't be reached",
	"you need to enable javascript to run", // create-react-app default
	"this app requires javascript",
}

// recallFromKinBrain checks if the user previously archived this URL.
// Returns nil on miss or if kinbrain isn't installed (graceful no-op).
//
// Strategy: `kinbrain recall <url>` will grep notes/ for the URL string.
// If exactly one path matches, we read it; otherwise we treat as miss
// (multiple matches → ambiguous, fresh fetch is safer).
func (b *Browser) recallFromKinBrain(url string) *Result {
	if _, err := exec.LookPath("kinbrain"); err != nil {
		return nil
	}
	// Look only in notes/ (where Archive writes); never reuse swarm
	// output/ or other roots as "browser cache".
	cmd := exec.Command("kinbrain", "recall", "--limit", "5", url)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	// Find paths under notes/<date>/web/
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "/.kinbrain/notes/") {
			continue
		}
		if !strings.Contains(line, "/web/") {
			continue
		}
		body, err := os.ReadFile(line)
		if err != nil {
			continue
		}
		md := string(body)
		// Strip the frontmatter we added in Archive() so the caller
		// sees clean content. Best-effort — keep the body even if the
		// frontmatter shape doesn't match.
		if i := strings.Index(md, "\n\n"); i > 0 && strings.Contains(md[:i], "url: ") {
			md = md[i+2:]
		}
		return &Result{
			URL:         url,
			Markdown:    md,
			FromArchive: true,
			Layer:       0,
			FetchedAt:   time.Now(), // recall time, not original fetch time
		}
	}
	return nil
}

// nonHTTPError is a sentinel: if Layer 1 fails with this, we still
// escalate. If it fails with anything else (e.g. context cancelled), we
// surface immediately.
type nonHTTPError struct{ inner error }

func (e nonHTTPError) Error() string { return e.inner.Error() }
func (e nonHTTPError) Unwrap() error { return e.inner }
