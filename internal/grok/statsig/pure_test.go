package statsig

import (
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"sync"
	"testing"
	"time"
)

// verify decodes a statsig value against (seed, hex) and checks every field
// grok recomputes server-side.
func verify(t *testing.T, out, pathname, method string, seed []byte, hex string) {
	t.Helper()
	raw, err := base64.RawStdEncoding.DecodeString(out)
	if err != nil || len(raw) != 70 {
		t.Fatalf("bad statsig (%d bytes): %v", len(raw), err)
	}
	key := raw[0]
	for i := 0; i < 48; i++ {
		if raw[1+i]^key != seed[i] {
			t.Fatalf("seed byte %d mismatch", i)
		}
	}
	number := uint32(raw[49]^key) | uint32(raw[50]^key)<<8 | uint32(raw[51]^key)<<16 | uint32(raw[52]^key)<<24
	input := method + "!" + pathname + "!" + strconv.FormatUint(uint64(number), 10) + statsigSalt + hex
	sum := sha256.Sum256([]byte(input))
	for i := 0; i < 16; i++ {
		if raw[53+i]^key != sum[i] {
			t.Fatalf("sha byte %d mismatch — would be code:7", i)
		}
	}
	if raw[69]^key != statsigMark {
		t.Fatalf("tail marker = %d want 3", raw[69]^key)
	}
}

// TestGenerateValid proves Generate yields a well-formed 70-byte statsig that is
// internally self-consistent — exactly what grok recomputes and validates.
func TestGenerateValid(t *testing.T) {
	out, err := Generate("/rest/app-chat/conversations/new", "POST", time.Now().Unix())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	seed, hex := defaultGenerator.Pair()
	verify(t, out, "/rest/app-chat/conversations/new", "POST", seed, hex)
}

// TestTwoCallsDiffer ensures a random key per call.
func TestTwoCallsDiffer(t *testing.T) {
	a, _ := Generate("/rest/app-chat/conversations/new", "POST", time.Now().Unix())
	b, _ := Generate("/rest/app-chat/conversations/new", "POST", time.Now().Unix())
	if a == b {
		t.Fatal("two calls produced identical statsig (expected random key)")
	}
}

// TestEmbeddedPairIsConsistent verifies that the embedded (seed, HEX) pair is
// well-formed: 48-byte seed and the HEX that f(seed) produces.
func TestEmbeddedPairIsConsistent(t *testing.T) {
	seed, hex := NewGenerator().Pair()
	if len(seed) != 48 {
		t.Fatalf("seed length %d", len(seed))
	}
	if hex != genuineHEX {
		t.Fatalf("hex = %q", hex)
	}
}

// Generators are independent: refreshing one pair must not affect another or
// the package default.
func TestGeneratorsAreIndependent(t *testing.T) {
	a, b := NewGenerator(), NewGenerator()
	seedBefore, hexBefore := b.Pair()

	newSeed := freshSeed()
	newHEX := freshHEX(newSeed)
	if err := a.SetPair(base64.StdEncoding.EncodeToString(newSeed), newHEX); err != nil {
		t.Fatal(err)
	}
	out, err := a.Generate("/p", "GET", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	verify(t, out, "/p", "GET", newSeed, newHEX)

	seedAfter, hexAfter := b.Pair()
	if string(seedBefore) != string(seedAfter) || hexBefore != hexAfter {
		t.Fatal("SetPair on one generator changed another")
	}
	if _, defHex := defaultGenerator.Pair(); defHex != genuineHEX {
		t.Fatal("SetPair on a private generator changed the process default")
	}
}

func TestSetPairValidation(t *testing.T) {
	g := NewGenerator()
	if err := g.SetPair("not base64 !!", "ab"); err == nil {
		t.Error("garbage seed accepted")
	}
	if err := g.SetPair(base64.StdEncoding.EncodeToString(make([]byte, 10)), "ab"); err == nil {
		t.Error("short seed accepted")
	}
	if err := g.SetPair(genuineSeed, "  "); err == nil {
		t.Error("empty HEX accepted")
	}
	// The failed calls must not have clobbered the active pair.
	if _, hex := g.Pair(); hex != genuineHEX {
		t.Fatalf("pair changed after rejected SetPair: %q", hex)
	}
}

func TestGeneratorConcurrentUse(t *testing.T) {
	g := NewGenerator()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if i%4 == 0 {
					_ = g.SetPair(genuineSeed, genuineHEX)
				}
				if _, err := g.Generate("/x", "POST", time.Now().Unix()); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
