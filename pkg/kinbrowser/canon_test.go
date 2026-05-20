package kinbrowser

import "testing"

func TestCanonicalize(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		// Tracking params dropped
		{
			"https://example.com/article?utm_source=twitter&utm_campaign=fall",
			"https://example.com/article",
		},
		{
			"https://example.com/x?fbclid=ABC123",
			"https://example.com/x",
		},
		{
			"https://example.com/y?gclid=def&mc_eid=ghi&_ga=jkl",
			"https://example.com/y",
		},
		// Real-world: arxiv.org/abs/... with various decorations
		{
			"https://arxiv.org/abs/1706.03762?ref=hn",
			"https://arxiv.org/abs/1706.03762",
		},
		// Non-tracking params kept (sorted for stability)
		{
			"https://example.com/search?q=cat&utm_source=x&page=2",
			"https://example.com/search?page=2&q=cat",
		},
		// Trailing slash removed (except root)
		{
			"https://example.com/article/",
			"https://example.com/article",
		},
		{
			"https://example.com/",
			"https://example.com/",
		},
		// Default port stripped
		{
			"http://example.com:80/foo",
			"http://example.com/foo",
		},
		{
			"https://example.com:443/foo",
			"https://example.com/foo",
		},
		// Fragment dropped
		{
			"https://example.com/article#section-3",
			"https://example.com/article",
		},
		// Case normalization
		{
			"HTTPS://Example.COM/Path",
			"https://example.com/Path", // host lowered, path preserved
		},
		// Whitespace trimmed
		{
			"  https://example.com/x  ",
			"https://example.com/x",
		},
		// Missing scheme defaulted to https
		{
			"example.com/foo",
			"https://example.com/foo",
		},
		// Idempotent (re-canonicalizing already-clean URL)
		{
			"https://example.com/article",
			"https://example.com/article",
		},
		// Empty input
		{
			"",
			"",
		},
	}
	for _, tt := range tests {
		got := canonicalize(tt.in)
		if got != tt.want {
			t.Errorf("canonicalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestCanonicalize_Idempotent — canonicalize twice should give the
// same result as once. Important for cache key stability.
func TestCanonicalize_Idempotent(t *testing.T) {
	cases := []string{
		"https://example.com/article?utm_source=x&q=foo",
		"http://example.com:80/foo/",
		"HTTPS://Example.com/X?fbclid=abc",
		"example.com",
	}
	for _, c := range cases {
		once := canonicalize(c)
		twice := canonicalize(once)
		if once != twice {
			t.Errorf("not idempotent for %q: %q → %q", c, once, twice)
		}
	}
}
