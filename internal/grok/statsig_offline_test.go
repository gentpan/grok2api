package grok

import (
	"encoding/base64"
	"testing"

	"github.com/gentpan/grok2api/internal/grok/statsig/svgfingerprint"
)

// Offline statsig cross-checks. They used to live in statsig_e2e_test.go but
// need no network or config, so they stay in the default test run.

// TestE2E_ShardCrosscheck checks the HEX computed in Go equals the one the
// browser produced for the live-captured seed.
func TestE2E_ShardCrosscheck(t *testing.T) {
	seedB64 := "t2ODAFY4ozXd0K2Y8MdI2XfxTDiJoakZPuoaKfcQn8VuasZMcKliyhA1pJ+o1oMf"
	wantHEX := "3bab9506b851eb851eb840e8f5c28f5c28f80e8f5c28f5c28f806b851eb851eb8400"

	got, err := svgfingerprint.ComputeHEXForSeedB64(seedB64)
	if err != nil {
		t.Fatalf("ComputeHEXForSeedB64: %v", err)
	}
	if got != wantHEX {
		t.Fatalf("crosscheck mismatch:\ngot:  %s\nwant: %s", got, wantHEX)
	}
	t.Logf("✓ crosscheck passed: %s", got)
}

// TestE2E_DynamicSeedVariation verifies that different seeds produce
// different HEX values (seed-dependent, not constant).
func TestE2E_DynamicSeedVariation(t *testing.T) {
	cases := []struct {
		b5, b22, b23, b24 byte
	}{
		{5, 100, 200, 50},
		{8, 150, 25, 62},
		{12, 169, 25, 62},
		{12, 200, 100, 30},
	}
	for _, c := range cases {
		seed := make([]byte, 48)
		seed[5] = c.b5
		seed[22] = c.b22
		seed[23] = c.b23
		seed[24] = c.b24
		b64 := base64.StdEncoding.EncodeToString(seed)
		hex, err := svgfingerprint.ComputeHEXForSeedB64(b64)
		if err != nil {
			t.Fatalf("seed[%d,%d,%d,%d]: %v", c.b5, c.b22, c.b23, c.b24, err)
		}
		t.Logf("seed[%d,%d,%d,%d] → HEX=%s (len=%d)", c.b5, c.b22, c.b23, c.b24, hex, len(hex))
	}
}
