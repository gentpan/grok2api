package grok

import (
	"encoding/base64"
	"testing"

	"github.com/gentpan/grok2api/internal/grok/statsig/svgfingerprint"
)

// metaTag builds the verification meta tag; the tag name contains U+2015
// HORIZONTAL BAR, written as an escape so the test cannot be mis-typed.
func metaTag(dash, content string) []byte {
	return []byte(`<meta name="grok-site` + dash + `verification" content="` + content + `">`)
}

func TestExtractStatsigSeedFromHTML(t *testing.T) {
	seed := make([]byte, 48)
	seed[5] = 2
	seedB64 := base64.StdEncoding.EncodeToString(seed)
	html := append([]byte(`<html><head>`), metaTag("―", seedB64)...)
	html = append(html, `<meta name="viewport"></head><body></body></html>`...)

	got := extractStatsigSeedFromHTML(html)
	if got != seedB64 {
		t.Fatalf("got %q want %q", got, seedB64)
	}
}

func TestExtractStatsigSeedIsStrictAboutTheDashCharacter(t *testing.T) {
	// U+2011 (non-breaking hyphen), U+2013 and a plain hyphen are not the tag name.
	for _, dash := range []string{"‑", "–", "-"} {
		if got := extractStatsigSeedFromHTML(metaTag(dash, "SEED")); got != "" {
			t.Errorf("dash %q matched: %q", dash, got)
		}
	}
	// Attribute order and quote style do not matter.
	html := []byte("<meta content='SEED' name='grok-site―verification'>")
	if got := extractStatsigSeedFromHTML(html); got != "SEED" {
		t.Errorf("reordered attributes: %q", got)
	}
}

func TestStatsigPairFromHTML(t *testing.T) {
	seed := make([]byte, 48)
	seed[5] = 3
	seed[22], seed[23], seed[24] = 3, 5, 7
	seedB64 := base64.StdEncoding.EncodeToString(seed)

	gotSeed, gotHex, err := statsigPairFromHTML(metaTag("―", seedB64))
	if err != nil {
		t.Fatal(err)
	}
	want, err := svgfingerprint.ComputeHEXForSeed(seed)
	if err != nil {
		t.Fatalf("ComputeHEXForSeed: %v", err)
	}
	if gotSeed != seedB64 || gotHex != want {
		t.Fatalf("seed=%q hex=%q want hex %q", gotSeed, gotHex, want)
	}

	// No tag: no pair, no error.
	if s, h, err := statsigPairFromHTML([]byte("<html></html>")); s != "" || h != "" || err != nil {
		t.Fatalf("no tag: %q %q %v", s, h, err)
	}
	// A seed that cannot be fingerprinted is an error that keeps the seed.
	if _, _, err := statsigPairFromHTML(metaTag("―", "short")); err == nil {
		t.Fatal("invalid seed accepted")
	}
}

func TestGeneratedHEXFromExtractedSeed(t *testing.T) {
	seed := make([]byte, 48)
	seed[5] = 3
	seed[22] = 3
	seed[23] = 5
	seed[24] = 7
	seedB64 := base64.StdEncoding.EncodeToString(seed)

	hx, err := svgfingerprint.ComputeHEXForSeedB64(seedB64)
	if err != nil {
		t.Fatalf("ComputeHEXForSeedB64: %v", err)
	}
	want, err := svgfingerprint.ComputeHEXForSeed(seed)
	if err != nil {
		t.Fatalf("ComputeHEXForSeed: %v", err)
	}
	if hx != want {
		t.Fatalf("got %s want %s", hx, want)
	}
}
