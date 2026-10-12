package model

import (
	"reflect"
	"testing"
)

func TestTierFromNameIsCaseAndSpaceInsensitive(t *testing.T) {
	cases := []struct {
		in   string
		want Tier
		ok   bool
	}{
		{"basic", TierBasic, true},
		{"BASIC", TierBasic, true},
		{"", TierBasic, true},
		{"  Super ", TierSuper, true},
		{"SUPER", TierSuper, true},
		{"Heavy", TierHeavy, true},
		{"\theavy\n", TierHeavy, true},
		{"ultra", TierBasic, false},
	}
	for _, c := range cases {
		got, ok := TierFromName(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("TierFromName(%q) = %v,%v want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestModeFromNameRoundTripsApiStr(t *testing.T) {
	for _, m := range []ModeId{ModeAuto, ModeFast, ModeExpert, ModeHeavy, ModeGrok43, ModeConsole} {
		got, ok := ModeFromName(m.ApiStr())
		if !ok || got != m {
			t.Errorf("ModeFromName(%q) = %v,%v want %v", m.ApiStr(), got, ok, m)
		}
	}
	if got, ok := ModeFromName(" EXPERT "); !ok || got != ModeExpert {
		t.Errorf("ModeFromName(\" EXPERT \") = %v,%v", got, ok)
	}
	if _, ok := ModeFromName("turbo"); ok {
		t.Error("unknown mode must not parse")
	}
}

// Pin the account-selection priority order (pool ids are Tier values).
func TestPoolCandidatesOrder(t *testing.T) {
	cases := []struct {
		tier Tier
		best bool
		want []int
	}{
		{TierBasic, false, []int{0, 1, 2}},
		{TierBasic, true, []int{2, 1, 0}},
		{TierSuper, false, []int{1, 2}},
		{TierSuper, true, []int{2, 1}},
		{TierHeavy, false, []int{2}},
		{TierHeavy, true, []int{2}},
	}
	for _, c := range cases {
		s := &Spec{Tier: c.tier, PreferBest: c.best}
		if got := s.PoolCandidates(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("tier=%v best=%v: %v, want %v", c.tier, c.best, got, c.want)
		}
	}
}

func TestBuiltInTableIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range allModels {
		if s.ModelName == "" || s.PublicName == "" {
			t.Errorf("incomplete entry: %+v", s)
		}
		if seen[s.ModelName] {
			t.Errorf("duplicate model name %q", s.ModelName)
		}
		seen[s.ModelName] = true
		if s.Capability == 0 {
			t.Errorf("%s has no capability", s.ModelName)
		}
		if s.IsConsoleChat() && s.ModeId != ModeConsole {
			t.Errorf("%s: console chat model must use ModeConsole", s.ModelName)
		}
		if s.Custom {
			t.Errorf("%s: built-in entries must not be marked custom", s.ModelName)
		}
	}
	if len(All()) < len(allModels) {
		t.Errorf("registry lost built-in models: %d < %d", len(All()), len(allModels))
	}
}
