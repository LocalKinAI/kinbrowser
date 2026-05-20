# kinbrowser

> **Markdown-native browser for AI agents.**
> Chrome renders pixels you don't read. kinbrowser extracts markdown you do.

A single Go binary that fetches any URL and returns its main content as
clean markdown — usable from any LLM agent, CLI, or Go program. Three
escalating backends, one uniform markdown output.

```bash
go install github.com/LocalKinAI/kinbrowser/cmd/kinbrowser@latest
kinbrowser open https://arxiv.org/abs/1706.03762
```

```
[kinbrowser] L1 http | Attention Is All You Need | 1963 chars | 183ms

[View PDF](https://arxiv.org/pdf/1706.03762)

> Abstract:The dominant sequence transduction models are based on
> complex recurrent or convolutional neural networks in an
> encoder-decoder configuration. ...
```

## Why a browser FOR agents (not adapted from human ones)

| | Chrome / Playwright / chromedp | **kinbrowser** |
|---|---|---|
| Output | Pixels + DOM tree | **Markdown** (LLM-native) |
| Token cost / page | 5-10× HTML boilerplate | minimal — just the content |
| Memory / instance | ~500 MB | ~50 MB (Layer 1) — ~500 MB only for fallback Layer 3 |
| Renders CSS / fonts / GPU | yes | **no** (agents don't read pixels) |
| Default action | Fetch + render | **Fetch + extract main content** |
| Memory across visits | none | optional [KinBrain](https://github.com/LocalKinAI/localkin-core) integration |

## Three-layer escalation

```
kinbrowser.Open(url)
  ↓
[L0a] Session LRU cache (in-memory, dies with process)
[L0b] KinBrain archive — was this URL previously archived?
  ↓
[L1] HTTP GET + go-readability + html→markdown    ~100ms   ~80% of sites
  ↓ (only if Layer 1 returns thin content)
[L2] Lightpanda (CDP, JS exec, no rendering)       ~500ms   +15%
  ↓ (only if Layer 2 still thin)
[L3] chromedp (full Chrome via CDP)                ~2 s     +5%
  ↓
Markdown out
```

**Same markdown shape regardless of which layer produced it** — the agent
never knows or cares which backend ran. Layers are pure escalation; the
contract is "URL in, markdown out".

## Layers in detail

### Layer 1: HTTP + readability + html→markdown

Plain `http.Get`, then [go-shiori/go-readability](https://github.com/go-shiori/go-readability)
to strip nav/footer/ads, then [JohannesKaufmann/html-to-markdown](https://github.com/JohannesKaufmann/html-to-markdown)
to convert. ~100 ms per page. Handles arxiv abstracts, GitHub READMEs,
Hacker News, Wikipedia, most blogs, all docs sites that ship HTML
without JS-only content. ~80% of "what an agent wants to read".

### Layer 2: Lightpanda (optional, opt-in)

[Lightpanda](https://github.com/lightpanda-io/browser) is a from-scratch
agent-native browser engine (Zig, 23k stars). Skips rendering entirely
but runs a V8 JS engine for SPA hydration. ~50 MB / instance, ~500 ms
per page. Enable via `--lightpanda ws://localhost:9222` after starting
Lightpanda separately.

### Layer 3: chromedp (full Chrome)

[chromedp](https://github.com/chromedp/chromedp) spawning a real
headless Chrome via CDP. The last-resort backend — slow (~2 s), heavy
(~500 MB), but handles everything from Cloudflare challenges to complex
React SPAs to login walls.

**Layers 2 and 3 share 95% of the code** — both speak CDP. The only
difference is which binary is at the other end of the WebSocket.

## Two memory primitives

### Session LRU

Same-process revisit avoidance. If an agent calls `Open(url)` twice in
the same session, the second call hits in-memory cache (~µs). Dies with
the process. Capacity 128 URLs by default.

### KinBrain archive (opt-in)

Optional integration with [KinBrain](https://github.com/LocalKinAI/localkin-core/tree/main/pkg/kinbrain),
the user's personal knowledge base. `Archive(url)` fetches + saves to
`~/.kinbrain/notes/<date>/web/`. `Open(url)` checks if a URL was
previously archived and returns the archive on hit.

**Opt-in is critical**: `Open()` never writes to KinBrain on its own.
Auto-saving every URL an agent fetches would pollute the user's
curated knowledge base with marketing pages, expired articles, and
search result junk. The archive call is explicit, agent-decided —
analogous to a human bookmarking a page.

Works without KinBrain installed (graceful no-op).

## Install

```bash
# CLI + library (Go 1.22+)
go install github.com/LocalKinAI/kinbrowser/cmd/kinbrowser@latest

# Optional but recommended for the L2 path:
brew tap lightpanda-io/tap
brew install lightpanda
```

## CLI

```bash
kinbrowser open <url>           # read, return markdown
kinbrowser archive <url>        # read + save to KinBrain
kinbrowser version              # print version + build info
kinbrowser help                 # full usage

# Flags
--lightpanda ws://localhost:9222   # enable Layer 2 (Lightpanda CDP)
--no-chrome                        # disable Layer 3 (chromedp)
--quiet                            # suppress stderr layer/timing
--cache N                          # session LRU size (default 128)
--timeout SECONDS                  # per-backend timeout
```

## Go library

```go
package main

import (
    "context"
    "fmt"

    "github.com/LocalKinAI/kinbrowser/pkg/kinbrowser"
)

func main() {
    b, _ := kinbrowser.New(
        kinbrowser.WithLightpanda("ws://localhost:9222"), // optional
        kinbrowser.WithCacheSize(256),
    )
    r, err := b.Open(context.Background(), "https://arxiv.org/abs/1706.03762")
    if err != nil { panic(err) }
    fmt.Printf("Title: %s\nLayer: L%d\nLength: %d chars\n\n%s\n",
        r.Title, r.Layer, len(r.Markdown), r.Markdown)
}
```

## Real-world numbers (2026-05-20, MacBook Air M2)

| URL | Layer | Time | Markdown size |
|---|---|---|---|
| https://arxiv.org/abs/1706.03762 (Attention paper) | L1 | 183ms | 1.9 KB |
| https://github.com/lightpanda-io/browser README | L1 | 965ms | 13.2 KB |
| https://news.ycombinator.com (30 posts) | L1 | 267ms | 15.8 KB |

Layer 1 (HTTP + readability + html2md) is the workhorse. Layer 3 only
fires on JS-only SPAs, paywalls, anti-bot challenges.

## Design lineage

Built on a four-paper architectural thesis from
[LocalKinAI](https://github.com/LocalKinAI):

| Paper | Contribution to kinbrowser |
|------|---------------------------|
| [#4 Grep Is All You Need](https://doi.org/10.5281/zenodo.19777260) | LLMs don't need vector embeddings or rendered pages — markdown is enough |
| [#5 Thin Soul, Fat Skill](https://doi.org/10.5281/zenodo.20094232) | One simple Open/Archive interface, escalating backends underneath |
| [#11 Grep-Routed Agents](https://doi.org/10.5281/zenodo.20131046) | Fast-path / slow-path routing — Layer 1 first, escalate on miss |
| GBrain (Garry Tan) | The "browser as personal memory annex" pattern |

## What this is NOT

- ❌ Not a Chromium fork (use [BrowserOS](https://github.com/browseros-ai/BrowserOS) for that)
- ❌ Not a new browser engine (use [Lightpanda](https://github.com/lightpanda-io/browser) for that — kinbrowser uses it as Layer 2 backend)
- ❌ Not a full automation framework (use [browser-use](https://github.com/browser-use/browser-use) or [Playwright](https://playwright.dev/) for that — those are heavier)
- ❌ Not a Web UI (CLI + Go library only)

kinbrowser is the **thin composition layer**: takes existing tech (HTTP,
readability, chromedp, Lightpanda, KinBrain) and stitches them into one
markdown-native interface agents can consume.

## License

Apache 2.0. See [LICENSE](LICENSE).

## Project family

- [kinclaw](https://github.com/LocalKinAI/kinclaw) — the 5-claw macOS computer-use agent
- [kinkit](https://github.com/LocalKinAI/kinkit) — STT + TTS for kin-* agents
- [localkin-core](https://github.com/LocalKinAI/localkin-core) — the agent kernel + KinBrain
- [notebooklm-go](https://github.com/LocalKinAI/notebooklm-go) — Go client for NotebookLM
- **kinbrowser** — markdown-native browser ← you are here
