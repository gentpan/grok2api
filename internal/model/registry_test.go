package model

import (
	"sync"
	"testing"
)

func TestRegisterRemoveAndToggle(t *testing.T) {
	const name = "grok-test-custom-1"
	t.Cleanup(func() { Remove(name) })

	if _, ok := Resolve(name); ok {
		t.Fatal("custom model should not exist yet")
	}
	if err := Register(Spec{ModelName: name, ModeId: ModeConsole, Tier: TierBasic, Capability: CapConsoleChat, Enabled: true, PublicName: "Test"}); err != nil {
		t.Fatal(err)
	}
	s, ok := Resolve(name)
	if !ok || !s.Custom || !s.IsConsoleChat() {
		t.Fatalf("resolve after register: %+v ok=%v", s, ok)
	}
	if err := Register(Spec{ModelName: name, Enabled: true}); err != ErrModelExists {
		t.Fatalf("duplicate register = %v, want ErrModelExists", err)
	}
	// Built-in names can never be shadowed or removed.
	if err := Register(Spec{ModelName: "grok-4.20-fast", Enabled: true}); err != ErrModelExists {
		t.Fatalf("shadowing built-in = %v", err)
	}
	if Remove("grok-4.20-fast") {
		t.Fatal("built-in model was removed")
	}

	if !SetEnabled(name, false) {
		t.Fatal("SetEnabled failed")
	}
	if _, ok := Resolve(name); ok {
		t.Fatal("disabled model still resolves")
	}
	if Get(name) == nil {
		t.Fatal("disabled model should still be listed by Get")
	}
	// Re-enabling restores it; the earlier pointer is not mutated.
	SetEnabled(name, true)
	if s2, ok := Resolve(name); !ok || s2 == s {
		t.Fatalf("re-enable: ok=%v same-pointer=%v", ok, s2 == s)
	}
	if s.Enabled != true {
		t.Fatal("published Spec was mutated")
	}
	if !Remove(name) || Get(name) != nil {
		t.Fatal("remove failed")
	}
}

func TestBuiltInToggleDoesNotAffectOthers(t *testing.T) {
	const name = "grok-4.20-fast"
	t.Cleanup(func() { SetEnabled(name, true) })
	before := len(ListEnabled())
	SetEnabled(name, false)
	if len(ListEnabled()) != before-1 {
		t.Fatalf("enabled count %d -> %d", before, len(ListEnabled()))
	}
}

func TestRegistryConcurrentUse(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = ListEnabled()
				_, _ = Resolve("grok-4.20-fast")
			}
		}()
		go func(i int) {
			defer wg.Done()
			n := "grok-conc-" + string(rune('a'+i))
			for j := 0; j < 100; j++ {
				_ = Register(Spec{ModelName: n, Enabled: true})
				SetEnabled(n, j%2 == 0)
				Remove(n)
			}
		}(i)
	}
	wg.Wait()
}
