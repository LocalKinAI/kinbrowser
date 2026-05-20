package kinbrowser

import (
	"net/url"
	"strings"
)

// trackingParams — query parameters that don't affect content but
// fragment the cache. Stripped before cache lookup AND before fetch.
//
// utm_*           Google Analytics campaign tracking
// fbclid          Facebook click ID
// gclid           Google Ads click ID
// mc_eid, mc_cid  Mailchimp tracking
// _ga, _gl        Google Analytics v4 linker
// ref, ref_src    HN / Reddit / Twitter referrer
// igshid          Instagram share ID
// si              YouTube share ID
// spm             Alibaba/Aliexpress tracking
// share_token,
//
//	share_id      generic social share tokens
var trackingParams = map[string]bool{
	"fbclid":      true,
	"gclid":       true,
	"dclid":       true,
	"mc_eid":      true,
	"mc_cid":      true,
	"_ga":         true,
	"_gl":         true,
	"ref":         true,
	"ref_src":     true,
	"ref_url":     true,
	"igshid":      true,
	"si":          true,
	"spm":         true,
	"share_token": true,
	"share_id":    true,
	"share":       true,
	"yclid":       true, // Yandex
	"msclkid":     true, // Microsoft Ads
}

// canonicalize returns a normalized form of rawURL suitable as a cache
// key and as the input to backends. Idempotent — feeding a canonical
// URL back in returns the same string.
//
// Normalization steps:
//
//  1. Trim whitespace
//  2. Ensure scheme (default https)
//  3. Lowercase scheme and host
//  4. Strip default ports (:80 from http, :443 from https)
//  5. Drop tracking query params (utm_*, fbclid, gclid, mc_*, etc.)
//  6. Sort remaining query params (stable cache key under reorder)
//  7. Remove fragment (#section — never travels server-side)
//  8. Strip trailing slash from path (except root "/")
//
// Returns the input unchanged if it can't be parsed (defensive — we
// don't want to break weird-but-valid URLs the user typed).
func canonicalize(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return rawURL
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)

	// Strip default ports.
	if (u.Scheme == "http" && strings.HasSuffix(u.Host, ":80")) ||
		(u.Scheme == "https" && strings.HasSuffix(u.Host, ":443")) {
		u.Host = u.Host[:strings.LastIndex(u.Host, ":")]
	}

	// Strip tracking params; keep others sorted for stability.
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			if trackingParams[k] || strings.HasPrefix(k, "utm_") {
				q.Del(k)
			}
		}
		u.RawQuery = q.Encode() // url.Values.Encode sorts keys
	}

	// Fragments are client-side; server can't know them. Drop.
	u.Fragment = ""

	// Trailing slash: keep for root, drop for everything else. This
	// matches Chrome's normalization (and what readability/HTTP cache
	// implicitly expect).
	if len(u.Path) > 1 && strings.HasSuffix(u.Path, "/") {
		u.Path = strings.TrimRight(u.Path, "/")
	}

	return u.String()
}
