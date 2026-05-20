# Changelog

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
