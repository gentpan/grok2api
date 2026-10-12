package account

import (
	"context"
	"testing"

	"github.com/gentpan/grok2api/internal/model"
)

func TestPoolFromNameIsCaseInsensitive(t *testing.T) {
	cases := map[string]struct {
		id PoolID
		ok bool
	}{
		"basic": {PoolBasic, true}, "BASIC": {PoolBasic, true}, "": {PoolBasic, true},
		" Super ": {PoolSuper, true}, "super": {PoolSuper, true},
		"Heavy": {PoolHeavy, true}, "HEAVY": {PoolHeavy, true},
		"nope": {PoolBasic, false},
	}
	for in, want := range cases {
		id, ok := PoolFromName(in)
		if id != want.id || ok != want.ok {
			t.Errorf("PoolFromName(%q) = %v,%v want %v,%v", in, id, ok, want.id, want.ok)
		}
	}
}

// The stored pool name is always the canonical lower-case one, otherwise
// pool comparisons (ListQuery.Pool, ReplacePool) would silently miss it.
func TestUpsertNormalisesPoolName(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-upper-0123456789", "SUPER")
	mustUpsert(t, r, "tok-bogus-0123456789", "platinum")
	get := func(tok string) *Record {
		recs, _ := r.GetAccounts(ctx, []string{tok})
		return recs[0]
	}
	if got := get("tok-upper-0123456789").Pool; got != "super" {
		t.Fatalf("pool = %q, want super", got)
	}
	if got := get("tok-bogus-0123456789").Pool; got != "basic" {
		t.Fatalf("pool = %q, want basic", got)
	}
	bogus := "Platinum"
	if _, err := r.PatchAccounts(ctx, []Patch{{Token: "tok-upper-0123456789", Pool: &bogus}}); err != nil {
		t.Fatal(err)
	}
	if got := get("tok-upper-0123456789").Pool; got != "super" {
		t.Fatalf("unknown pool accepted by Patch: %q", got)
	}
	heavy := "Heavy"
	if _, err := r.PatchAccounts(ctx, []Patch{{Token: "tok-upper-0123456789", Pool: &heavy}}); err != nil {
		t.Fatal(err)
	}
	if got := get("tok-upper-0123456789").Pool; got != "heavy" {
		t.Fatalf("pool = %q, want heavy", got)
	}
}

func TestModeConstantsMatchModel(t *testing.T) {
	pairs := []struct {
		got  int
		want model.ModeId
	}{
		{ModeAuto, model.ModeAuto}, {ModeFast, model.ModeFast}, {ModeExpert, model.ModeExpert},
		{ModeHeavy, model.ModeHeavy}, {ModeGrok43, model.ModeGrok43}, {ModeConsole, model.ModeConsole},
	}
	for _, p := range pairs {
		if p.got != int(p.want) {
			t.Errorf("mode constant %d != model %d", p.got, p.want)
		}
	}
}

// ClearFailures removes exactly the documented failure keys and nothing else.
func TestClearFailuresRemovesFailureKeysOnly(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-clear-0123456789", "basic")
	ext := map[string]any{"keep": "me"}
	for _, k := range failureExtKeys {
		ext[k] = 1
	}
	if _, err := r.PatchAccounts(ctx, []Patch{{Token: "tok-clear-0123456789", ExtMerge: ext}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PatchAccounts(ctx, []Patch{{Token: "tok-clear-0123456789", ClearFailures: true}}); err != nil {
		t.Fatal(err)
	}
	recs, _ := r.GetAccounts(ctx, []string{"tok-clear-0123456789"})
	for _, k := range failureExtKeys {
		if _, ok := recs[0].Ext[k]; ok {
			t.Errorf("key %q survived ClearFailures", k)
		}
	}
	if recs[0].Ext["keep"] != "me" {
		t.Fatalf("unrelated ext key removed: %v", recs[0].Ext)
	}
}
