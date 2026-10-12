package grok

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gentpan/grok2api/internal/grok/statsig/svgfingerprint"
)

// maxHTMLBytes caps the grok.com page read for the statsig seed.
const maxHTMLBytes = 4 << 20

var (
	// The meta tag name contains U+2015 HORIZONTAL BAR; it is written as an
	// escape so the pattern survives retyping and copy/paste.
	statsigMetaTagRE = regexp.MustCompile(`(?is)<meta\b[^>]*\bname=["']grok-site\x{2015}verification["'][^>]*>`)
	contentAttrRE    = regexp.MustCompile(`(?is)\bcontent=["']([^"']+)["']`)
)

// fetchStatsigPairFromHTML loads grok.com's HTML and derives a (seed, HEX)
// pair from its verification meta tag. It returns an empty seed when the page
// carries none (the tag is often absent from SSR HTML or a challenge page).
//
// The request goes through the transport's dedicated HTML pool: same proxy and
// reset policy as API traffic, but a separate cookie jar so the anonymous
// page's Set-Cookie headers never leak into authenticated API requests.
func (t *Transport) fetchStatsigPairFromHTML(ctx context.Context, p proxyProfile) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL()+"/", nil)
	if err != nil {
		return "", "", err
	}
	req.Header = p.htmlHeaders()

	resp, err := t.htmlPool.do(ctx, req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLBytes))
	if err != nil {
		return "", "", err
	}
	return statsigPairFromHTML(body)
}

// statsigPairFromHTML extracts the seed from an HTML document and computes its
// HEX.
func statsigPairFromHTML(body []byte) (seed, hex string, err error) {
	seed = extractStatsigSeedFromHTML(body)
	if seed == "" {
		return "", "", nil
	}
	hex, err = svgfingerprint.ComputeHEXForSeedB64(seed)
	if err != nil {
		return seed, "", err
	}
	return seed, hex, nil
}

func extractStatsigSeedFromHTML(body []byte) string {
	tag := statsigMetaTagRE.Find(body)
	if len(tag) == 0 {
		return ""
	}
	m := contentAttrRE.FindSubmatch(tag)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}
