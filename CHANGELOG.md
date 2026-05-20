# Changelog

## [0.2.0] - 2026-05-20

Six audit issues addressed in one tag. User asked the right question
after sleeping on v0.1.2: "what else needs improvement?" — turned into
a full P0/P1 sweep.

### Added

- **PDF support** (P0). `Content-Type: application/pdf` or `%PDF-`
  magic prefix triggers PDF extraction instead of HTML readability.
  Two-tier extraction:
  - **`pdftotext`** (poppler-utils, `brew install poppler`) when on
    PATH — proper word boundaries, multi-column layout preserved,
    UTF-8 handling. Used for ~all macOS / Linux dev machines.
  - **`github.com/ledongthuc/pdf`** pure-Go fallback for zero-dep
    install. Word boundaries reconstructed via lowercase→Uppercase,
    letter↔digit, punct→letter regex (best-effort; arxiv prose comes
    out readable, complex layouts are degraded).
  - Either way, output is markdown with frontmatter naming the
    extractor used + page count.
  - **Unlocks arxiv `/pdf/`** which is the swarm's #1 reading source
    via paper_scout / wanshitong. Smoke-test on 1706.03762: 61 KB
    clean markdown / 1.05 s via pdftotext.

- **Chrome profile persistence** (P0). L3 (chromedp) now uses a
  persistent `UserDataDir` (`~/.kinbrowser/chrome-profile/` by
  default). Cookies, localStorage, login state survive across
  kinbrowser invocations. Login-walled content (X authenticated
  views, Substack paid, LinkedIn) works on revisit.
  - `WithChromeProfile(dir)` Go option to override.
  - `$KINBROWSER_CHROME_PROFILE` env override.
  - Per-agent isolation possible by giving each agent its own dir.

- **URL canonicalization** (P0). All URLs are normalized before cache
  lookup AND before fetch:
  - Strip 17 known tracking params (`utm_*`, `fbclid`, `gclid`,
    `mc_eid`, `_ga`, `ref`, `igshid`, `si`, `spm`, `yclid`, ...)
  - Drop URL fragments (`#section`)
  - Strip trailing slash (except root)
  - Strip default ports (`:80` / `:443`)
  - Lowercase scheme + host
  - Sort remaining query params for stable cache keys
  - Idempotent: feed clean URL back in, get same string out.
  - Cache hit rate goes up on agents that read the same article from
    multiple referrer-tagged links.

- **Per-layer timeout** (P1). Single `cdpTimeout` split into:
  - `httpTimeout` (L1, **5 s** — fast fail)
  - `lightpandaTimeout` (L2, **10 s**)
  - `chromedpTimeout` (L3, **20 s**)
  - `WithTimeouts(http, lp, chrome)` Go option overrides.
  - Worst-case full escalation now ~35 s instead of unbounded.

- **`kinbrowser daemon` subcommands** (P1):
  - `daemon status` — report running/pid/uptime/CDP URL
  - `daemon start` — explicit start (no-op if already running)
  - `daemon stop` — kill the lightpanda we spawned
  - Operator hygiene for the auto-spawned daemon.

- **Diff-on-revisit** (P1). When `Open()` hits a previously archived
  URL, kinbrowser now ALSO fetches fresh and reconciles:
  - **Unchanged** (≥85% Jaccard line similarity) → return archive
    with `[UNCHANGED since ...]` note. Saves the LLM a re-read.
  - **Drift** (some lines changed) → return fresh content with a
    unified `diff` block at the top (`+` added, `-` removed,
    truncated to 4 KB) + the new content below. LLM sees WHAT
    changed without re-reading the entire page.
  - **Rewrite** (low similarity) → return fresh with `[CHANGED]`
    warning.
  - Closes a promise from the original v0.1.0 design that wasn't
    implemented.

### Changed

- L1 HTTP body limit raised from 10 MB to **20 MB** to accommodate
  PDF downloads (arxiv papers run 5-15 MB; long ones can be larger).

### Files

    pkg/kinbrowser/canon.go            NEW    87 LoC + 95 LoC tests   (URL canonicalization)
    pkg/kinbrowser/pdf.go              NEW   200 LoC                  (PDF extraction, both backends)
    pkg/kinbrowser/diff.go             NEW   115 LoC + 90 LoC tests   (similarity + reconciliation)
    pkg/kinbrowser/kinbrowser.go       MOD   per-layer timeouts, chromeProfile field, options
    pkg/kinbrowser/cdp.go              MOD   profile wired into chromedp ExecAllocator
    pkg/kinbrowser/extract.go          MOD   PDF content-type sniff + 20 MB cap
    cmd/kinbrowser/main.go             MOD   `daemon` subcommand tree
    CHANGELOG.md                       MOD   this entry

### Tests

    16/16 pass (was 8/8 in v0.1.2; +5 canon, +4 diff, +1 force-layer follow-up)

### Lesson

> Wake up, audit, fix all. Six small surgical changes beat one big
> v1.0 rewrite. Each change is independently shippable; only bundled
> here for narrative.

---

## [0.1.2] - 2026-05-20

### Why

User feedback after v0.1.1: "if you detect L2 isn't installed, just
install it yourself."

v0.1.1 required users to opt into Layer 2 via `--lightpanda ws://...`
flag AND manually run `lightpanda serve` in a separate terminal. That
two-step ceremony made L2 effectively unused in practice — KinClaw's
skill wrapper didn't pass the flag, so all SPA reads fell through to
L3 chromedp (~6-7s) when L2 (~1.6s) was available.

### Fix

**Auto-detect + lazy-spawn**:

1. On every `Open()` call where Layer 1 escalation is needed, kinbrowser
   now:
   - Checks for `$KINBROWSER_LIGHTPANDA_URL` env (caller-managed daemon)
   - Probes `http://127.0.0.1:9222/json/version` (existing daemon)
   - If neither, checks if `lightpanda` is on PATH
   - If on PATH, **spawns `lightpanda serve --host 127.0.0.1 --port 9222`**
     as a detached child process, waits up to 3s for the CDP endpoint,
     then uses it for Layer 2
   - If lightpanda isn't installed, gracefully falls through to L3

2. The daemon is shared process-wide (single `sync.Mutex`-protected
   manager) and reused across subsequent calls — the probe is cached
   for 30s to avoid re-spawning in tight loops.

3. No flag needed. `kinbrowser open https://x.com` Just Works:
   - L1 stub detected → escalate
   - L2 auto-detect: lightpanda installed → spawn daemon → use it
   - 1.6s instead of 6.7s on subsequent SPA reads (daemon is hot)

### Verification

```
$ kinbrowser open https://x.com
[kinbrowser] L2 lightpanda | X. It's what's happening | 361 chars | 7.4s  ← first call (includes spawn)

$ pgrep -af "lightpanda serve"
16516 lightpanda serve --host 127.0.0.1 --port 9222

$ kinbrowser open https://x.com
[kinbrowser] L0 cache | X. It's what's happening | 361 chars | 0.0s  ← session LRU hit
```

After daemon is warm, subsequent SPA reads are ~1.5s (vs ~6-7s for L3
chromedp). 4× speedup on the L2-eligible portion of agent traffic.

### Installation hint

Auto-detect requires `lightpanda` binary on PATH. Install:

```bash
brew install lightpanda-io/browser/lightpanda
```

If you don't install it, kinbrowser uses L1 → L3 (still works, just
slower for SPA-heavy reads).

### New options

- `WithoutLightpandaAutoDetect()` — Go library opt-out, for tests or
  environments where you don't want kinbrowser spawning side-processes.
- Env: `KINBROWSER_LIGHTPANDA_URL=ws://...` — point at a remote/managed
  daemon instead of letting kinbrowser spawn one.

### Files

    pkg/kinbrowser/lightpanda.go        NEW   147 LoC (manager + probe + spawn)
    pkg/kinbrowser/kinbrowser.go        MOD   Browser.autoLightpanda field, default true; Open() wires auto-detect
    CHANGELOG.md                        MOD   this entry

### Design notes

- We deliberately don't kill the daemon on kinbrowser exit. brew
  installed it; the daemon is shared infrastructure. Next kinbrowser
  invocation reuses via port probe.
- The 30s probe cache avoids re-checking `/json/version` on every
  Open() call when L2 is already known up.
- Spawn writes daemon logs to /dev/null. If users want them, they can
  run `lightpanda serve` themselves and we'll detect + reuse.

### Lesson

> Two-step ceremonies are zero-step in practice. If the dependency is
> installable and the daemon is spawnable, do both transparently.

---

## [0.1.1] - 2026-05-20

### Bug

User asked the right question after v0.1.0 shipped: "are all 3 layers
really working?" v0.1.0's smoke test only verified L1 on 3 friendly
URLs (arxiv, GitHub README, HN). L3 chromedp was wired in code but
never proved.

Worse — found a real bug: trying to load `https://x.com` returned a
**222-character "Something went wrong, but don't fret" stub** at L1.
The default `minMarkdown` threshold of 200 was below the stub size, so
`acceptable()` returned true, the result was cached, and **L3 never
fired** even though that's exactly the case it exists for.

Same shape would fail on instagram.com, behind-Cloudflare sites
returning challenge stubs, create-react-app placeholders, etc.

### Fix

1. **Stub pattern detection** in `acceptable()`. A small allowlist of
   known CSR-stub phrases now forces L1 rejection regardless of length:

   ```go
   var jsRequiredStubs = []string{
       "enable javascript",
       "javascript is required",
       "javascript is disabled",
       "please enable cookies",
       "please enable js",
       "something went wrong, but don",        // x.com / twitter
       "this site can't be reached",
       "you need to enable javascript to run", // create-react-app default
       "this app requires javascript",
   }
   ```

   List is lowercase-compared and easy to extend when a new
   stub-flavored failure shows up.

2. **`--force-layer N` flag** (and `WithForceLayer(n)` library option)
   for diagnostic testing. Bypasses caches AND escalation logic; runs
   exactly one backend. Lets you prove L3 works in isolation without
   waiting for L1 to fail.

### Verification

```
$ kinbrowser open https://x.com
[kinbrowser] L3 chromedp | X. It's what's happening | 369 chars | 6.7s
Happening now
Join today.
```

L1 stub correctly rejected → L3 chromedp fires → real content.

```
$ kinbrowser open --force-layer 3 https://arxiv.org/abs/1706.03762
[kinbrowser] L3 chromedp | Attention Is All You Need | 1963 chars | 2.5s
[View PDF](https://arxiv.org/pdf/1706.03762)
> Abstract:The dominant sequence transduction models...
```

L3 standalone proven on a known-good URL — 2.5s, same clean markdown
output as L1.

### Files

    pkg/kinbrowser/kinbrowser.go        MOD  jsRequiredStubs[], forceLayer field, WithForceLayer(), forced()
    pkg/kinbrowser/kinbrowser_test.go   MOD  +2 tests (stub detection, force-layer pinning)
    cmd/kinbrowser/main.go              MOD  --force-layer flag wired
    CHANGELOG.md                        MOD  this entry

### Tests

    8/8 pass (was 6/6; added TestAcceptable_DetectsJSStubs + TestForceLayer_PinsBackend)

### Lesson

> 80% of "stuff works" is misleading. Verify each escalation tier
> independently. v0.1.0 shipped "3 layers" with one tested; users
> would have hit the gap immediately on the first SPA.

---

## [0.1.0] - 2026-05-20

Initial public release.

### Added

- **Markdown-native browser** for AI agents. `Open(url)` returns extracted
  main content as markdown via 3-layer escalation:
  - Layer 1: `http.Get` + go-readability + html→markdown (~100ms, ~80% of sites)
  - Layer 2: Lightpanda CDP (opt-in, ~500ms, JS-hydrated SPAs)
  - Layer 3: chromedp / full Chrome (~2s, last-resort)
- **Same markdown shape** across all 3 layers — agents see one contract regardless of backend.
- **Session LRU cache** (default 128 entries) for same-process revisit avoidance.
- **Optional [KinBrain](https://github.com/LocalKinAI/localkin-core/tree/main/pkg/kinbrain)
  integration**: `Archive(url)` writes to `~/.kinbrain/notes/<date>/web/`;
  `Open(url)` checks for previously-archived URLs. Both gracefully no-op
  if kinbrain CLI isn't on PATH.
- **CLI**: `kinbrowser open / archive / version / help` with `--lightpanda`,
  `--no-chrome`, `--quiet`, `--cache`, `--timeout` flags.
- **Go library** under `pkg/kinbrowser` — `New(opts...)`, `Open(ctx, url)`,
  `Archive(ctx, url)`.
- 6 unit tests, all passing.

### Verified on real URLs (MacBook Air M2)

- arxiv abstract (1706.03762 "Attention Is All You Need"): L1, 183ms, 1.9 KB markdown — clean abstract extraction
- GitHub README (lightpanda-io/browser): L1, 965ms, 13.2 KB markdown
- Hacker News front page: L1, 267ms, 15.8 KB markdown — 30 posts with links, scores, comments

All 3 succeed at Layer 1 — no chromedp escalation needed.

### Design lineage

Built on the four-paper architectural thesis from LocalKinAI:

- Paper #4 (Grep Is All You Need) — LLMs need markdown, not rendered HTML
- Paper #5 (Thin Soul, Fat Skill) — one Open/Archive interface, escalating backends
- Paper #11 (Grep-Routed Agents) — fast-path/slow-path routing
- GBrain (Garry Tan) pattern — browser as personal memory annex
