package kinbrowser

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown/v2"
	readability "github.com/go-shiori/go-readability"
)

// fetchHTTP implements Layer 1: plain HTTP GET → readability → html→markdown.
// Returns a sentinel nonHTTPError on parse/extract failure (so the caller
// knows to escalate). Network/HTTP errors are returned as-is.
func (b *Browser) fetchHTTP(ctx context.Context, rawURL string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, b.httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return Result{}, err
	}
	// Identify ourselves honestly. Some sites (Cloudflare-protected, etc.)
	// will refuse generic Go user agents — those will fail Layer 1 and
	// escalate to Layer 3 (chromedp with a real Chrome UA).
	req.Header.Set("User-Agent", "kinbrowser/0.1 (+https://github.com/LocalKinAI/kinbrowser)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en,zh;q=0.9")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Treat as recoverable — chromedp might get past it (auth, JS challenge).
		return Result{}, nonHTTPError{inner: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10 MB cap
	if err != nil {
		return Result{}, err
	}

	return b.extract(rawURL, body)
}

// extract takes raw HTML bytes and produces a Result by running
// readability (strip nav/footer/ads, find main content) then converting
// the cleaned HTML to markdown.
func (b *Browser) extract(rawURL string, htmlBytes []byte) (Result, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return Result{}, nonHTTPError{inner: err}
	}

	article, err := readability.FromReader(strings.NewReader(string(htmlBytes)), parsed)
	if err != nil {
		return Result{}, nonHTTPError{inner: fmt.Errorf("readability: %w", err)}
	}

	mdBody, err := md.ConvertString(article.Content)
	if err != nil {
		return Result{}, nonHTTPError{inner: fmt.Errorf("html→markdown: %w", err)}
	}

	title := article.Title
	if title == "" {
		title = parsed.Host + parsed.Path
	}

	return Result{
		URL:       rawURL,
		Title:     title,
		Markdown:  strings.TrimSpace(mdBody),
		Layer:     1,
		FetchedAt: time.Now(),
	}, nil
}
