package kinbrowser

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

// fetchCDP implements Layer 2 (Lightpanda) AND Layer 3 (chromedp / full
// Chrome). They're the same code path — both speak CDP. The ONLY
// difference is which binary is at the other end of the WebSocket:
//
//   - lightpandaURL non-empty: connect to Lightpanda's CDP endpoint
//     (faster, no rendering, ~50 MB / process)
//   - lightpandaURL empty: spawn local Chrome via chromedp.NewExecAllocator
//     (heavier, ~500 MB, full Chromium)
//
// Both return the rendered HTML, which we then push through the same
// readability + html→markdown extract path Layer 1 uses. So the OUTPUT
// shape is identical across all 3 layers — agents see one markdown
// format regardless of which backend produced it.
func (b *Browser) fetchCDP(ctx context.Context, rawURL, lightpandaURL string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cdpTimeout)
	defer cancel()

	// Allocator: remote (Lightpanda) or local (Chrome via chromedp).
	var allocCtx context.Context
	var allocCancel context.CancelFunc
	if lightpandaURL != "" {
		allocCtx, allocCancel = chromedp.NewRemoteAllocator(ctx, lightpandaURL)
	} else {
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", false),
			// Identify as a real Chrome — Cloudflare et al. fingerprint
			// the default chromedp UA. Real chrome version doesn't
			// matter much; just don't say "HeadlessChrome".
			chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "+
				"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"),
		)
		allocCtx, allocCancel = chromedp.NewExecAllocator(ctx, opts...)
	}
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	var htmlBody string
	err := chromedp.Run(taskCtx,
		chromedp.Navigate(rawURL),
		// Wait for body — sites with progressive hydration usually have
		// body within 1-2 s even when full page takes longer.
		chromedp.WaitVisible(`body`, chromedp.ByQuery),
		// Small grace period for JS hydration. Empirically 800 ms catches
		// React/Vue/Svelte hydration on most sites without slowing down
		// static pages.
		chromedp.Sleep(800*time.Millisecond),
		chromedp.OuterHTML(`html`, &htmlBody, chromedp.ByQuery),
	)
	if err != nil {
		return Result{}, fmt.Errorf("cdp navigate %s: %w", rawURL, err)
	}

	return b.extract(rawURL, []byte(htmlBody))
}
