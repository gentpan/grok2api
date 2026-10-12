package account

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// The file is written in token order regardless of insertion order or map
// iteration order, so two flushes of the same state are byte-identical.
func TestFlushWritesRecordsSortedByToken(t *testing.T) {
	r, path := newTestRepo(t)
	for _, tok := range []string{"tok-zzzz-0123456789", "tok-mmmm-0123456789", "tok-aaaa-0123456789", "tok-kkkk-0123456789"} {
		mustUpsert(t, r, tok, "basic")
	}

	readTokens := func() []string {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var toks []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var rec Record
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Fatal(err)
			}
			toks = append(toks, rec.Token)
		}
		return toks
	}
	toks := readTokens()
	if len(toks) != 4 || !sort.StringsAreSorted(toks) {
		t.Fatalf("file not sorted by token: %v", toks)
	}

	first, _ := os.ReadFile(path)
	r.mu.Lock()
	err := r.flush()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatal("repeated flush produced different bytes")
	}
}
