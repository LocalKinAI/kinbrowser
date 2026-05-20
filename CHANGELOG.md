# Changelog

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
