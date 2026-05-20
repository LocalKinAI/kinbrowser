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
	chromeProfile   string // chromedp UserDataDir; persistent cookies across runs
	// Per-layer timeouts. Each layer gets its own budget so a slow L1
	// (e.g. server hanging) doesn't starve L2/L3 of time. Defaults
	// chosen so that the worst-case full escalation (L1 fail + L2 fail
	// + L3 fail) fits in ~35 s.
	httpTimeout       time.Duration // L1 (default 5s — fast fail)
	lightpandaTimeout time.Duration // L2 (default 10s)
	chromedpTimeout   time.Duration // L3 (default 20s)
	minMarkdown       int           // minimum markdown length to consider Layer 1 "successful"
	forceLayer        int           // 0 = auto-escalate, 1/2/3 = pin to specific layer (debug)
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

// WithChromeProfile sets the Chrome UserDataDir used by Layer 3
// (chromedp). Cookies, localStorage, and session data persist across
// kinbrowser invocations, enabling login-walled content fetches
// (X, Substack paid, LinkedIn, etc.) on revisit.
//
// Default: $HOME/.kinbrowser/chrome-profile (auto-created).
// Pass an empty string to use Chrome's default ephemeral profile
// (cookies lost between runs).
//
// Per-agent profiles: pass a per-agent dir like
// $HOME/.kinbrowser/chrome-profile-paul to isolate one agent's
// login state from another's.
func WithChromeProfile(dir string) Option {
	return func(b *Browser) { b.chromeProfile = dir }
}

// WithTimeouts overrides per-layer timeouts. Pass 0 for any field to
// keep its default (5s L1, 10s L2, 20s L3).
func WithTimeouts(http, lightpanda, chromedp time.Duration) Option {
	return func(b *Browser) {
		if http > 0 {
			b.httpTimeout = http
		}
		if lightpanda > 0 {
			b.lightpandaTimeout = lightpanda
		}
		if chromedp > 0 {
			b.chromedpTimeout = chromedp
		}
	}
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
// defaultChromeProfile returns the persistent Chrome UserDataDir
// kinbrowser uses for L3 by default. Honors $KINBROWSER_CHROME_PROFILE
// or falls back to ~/.kinbrowser/chrome-profile/. Auto-created by
// chromedp on first L3 fetch.
func defaultChromeProfile() string {
	if v := os.Getenv("KINBROWSER_CHROME_PROFILE"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return "" // no home dir → use chromedp's default ephemeral
	}
	return home + "/.kinbrowser/chrome-profile"
}

func New(opts ...Option) (*Browser, error) {
	cache, err := lru.New[string, Result](128)
	if err != nil {
		return nil, err
	}
	b := &Browser{
		cache:           cache,
		autoLightpanda:  true, // ← default: auto-detect Lightpanda on PATH
		chromedpEnabled: true,
		chromeProfile:   defaultChromeProfile(), // persistent cookies/session
		// Per-layer timeouts. Empirically tuned against real targets:
		//   - L1 5s was too aggressive for slow news servers
		//     (weather.com timed out in 5s but is otherwise reachable)
		//   - L3 20s was too aggressive for JS-heavy news
		//     (CNN couldn't DOM-stable in 20s; needed 25-30s)
		// New budgets total ~50s worst case (10+10+30), still well
		// under the CLI's outer 90s default ceiling.
		httpTimeout:       10 * time.Second, // L1 — accommodates slow servers
		lightpandaTimeout: 10 * time.Second, // L2 — JS hydration on most SPAs
		chromedpTimeout:   30 * time.Second, // L3 — full Chrome on heavy news sites
		minMarkdown:       200,              // ~50 words
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
	// Canonicalize BEFORE cache lookup so foo?utm=x and foo?utm=y both
	// hit the same cache entry and the backends both see the clean
	// version.
	url = canonicalize(url)

	// Forced-layer debug mode: skip caches + escalation, pin to one backend.
	if b.forceLayer != 0 {
		return b.forced(ctx, url)
	}

	// Layer 0a: session LRU
	if cached, ok := b.cache.Get(url); ok {
		return cached, nil
	}

	// Layer 0b: KinBrain archive. Opt-in (no-op if kinbrain CLI absent).
	//
	// When we get an archive hit, we DON'T just return it — we also
	// fetch fresh via Layer 1 and reconcile. Three outcomes:
	//   - Pages match (≥85% similarity) → return archive with [UNCHANGED] note
	//   - Some drift → return fresh with [CHANGED] note + unified diff
	//   - Major rewrite → return fresh with rewrite warning
	// This is what makes the archive useful: agents see what changed
	// since they last looked, not just stale snapshots.
	if archived := b.recallFromKinBrain(url); archived != nil {
		// Skip diff if user explicitly forced a layer; the user asked
		// for raw output, give them raw output.
		if b.forceLayer != 0 {
			b.cache.Add(url, *archived)
			return *archived, nil
		}
		// Race the fresh fetch — if it succeeds, reconcile; if it
		// fails (network down, etc.), fall back to archive alone.
		if fresh, err := b.fetchHTTP(ctx, url); err == nil && b.acceptable(fresh) {
			merged := b.reconcileArchived(*archived, fresh)
			b.cache.Add(url, merged)
			return merged, nil
		}
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

	// Layer 3: chromedp (full Chrome) — last resort.
	if b.chromedpEnabled {
		if r, err := b.fetchCDP(ctx, url, ""); err == nil {
			r.Layer = 3
			// Even L3 with persistent Chrome profile + real-Chrome UA
			// can hit anti-bot walls (Google search results, hardened
			// Cloudflare). If THAT still looks like stub/bot-wall,
			// surface a clear failure to the caller — better the LLM
			// reads "this URL is blocked" than gets fed an "unusual
			// traffic" challenge page as if it were content.
			if !b.acceptable(r) {
				return Result{}, fmt.Errorf(
					"all 3 backends returned stub/anti-bot wall for %s "+
						"(URL likely blocks automation; try a different source, "+
						"or use web_search for search queries)", url)
			}
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

// jsRequiredStubs — markdown fragments that signal "this is NOT real
// content — escalate". Two categories:
//
//  1. JS-required stubs — page is pre-hydration, LLM needs a JS engine
//     (x.com, instagram, react-app placeholders, etc.).
//
//  2. Anti-bot walls — server returned HTTP 200 with a challenge page
//     (Google "unusual traffic", Cloudflare "checking browser",
//     Akamai "access denied", etc.). readability extracts the
//     challenge text and L1 would wrongly accept it.
//
// For #2, escalation to L3 chromedp with the persistent profile
// (v0.2.0) sometimes gets past — real Chrome fingerprint + warm
// cookies fool some walls. When it doesn't, the failure surfaces
// cleanly to the LLM via the kinclaw skill's content-on-fail path.
//
// Lowercase comparisons. Add a new pattern when observed in the wild.
var jsRequiredStubs = []string{
	// JS-required stubs (observed: x.com, instagram, react placeholders)
	"enable javascript",
	"javascript is required",
	"javascript is disabled",
	"please enable cookies",
	"please enable js",
	"something went wrong, but don", // x.com / twitter stub
	"this site can't be reached",
	"you need to enable javascript to run", // create-react-app default
	"this app requires javascript",

	// Anti-bot walls (added 2026-05-20 after California-wildfire
	// session showed Google's bot challenge being returned as
	// "successful content" with HTTP 200)
	"unusual traffic from your computer network", // Google search bot wall
	"our systems have detected unusual",          // Google variant
	"checking if the site connection is secure",  // Cloudflare challenge
	"verifying you are human",                    // Cloudflare Turnstile
	"please verify you are a human",
	"please complete the security check",
	"access denied",
	"rate limit exceeded",
	"too many requests",
	"please solve this captcha",
	"sorry, you have been blocked", // Cloudflare generic block
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
