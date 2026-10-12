// Package model defines the Grok model registry: mode ids, account tiers,
// capability flags and the master list of supported models.
package model

import (
	"errors"
	"strings"
	"sync"
)

// ModeId is the Grok conversation mode used in chat payloads.
type ModeId int

const (
	ModeAuto    ModeId = 0
	ModeFast    ModeId = 1
	ModeExpert  ModeId = 2
	ModeHeavy   ModeId = 3
	ModeGrok43  ModeId = 4
	ModeConsole ModeId = 5
)

// ApiStr returns the modeId string used in the grok.com chat payload.
func (m ModeId) ApiStr() string {
	switch m {
	case ModeAuto:
		return "auto"
	case ModeFast:
		return "fast"
	case ModeExpert:
		return "expert"
	case ModeHeavy:
		return "heavy"
	case ModeGrok43:
		return "grok-420-computer-use-sa"
	case ModeConsole:
		return "console"
	}
	return "auto"
}

// ModeFromName is the inverse of ApiStr: it parses a mode name
// ("auto", "fast", "expert", "heavy", "grok-420-computer-use-sa", "console")
// case-insensitively and ignoring surrounding spaces.
func ModeFromName(name string) (ModeId, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "auto":
		return ModeAuto, true
	case "fast":
		return ModeFast, true
	case "expert":
		return ModeExpert, true
	case "heavy":
		return ModeHeavy, true
	case "grok-420-computer-use-sa":
		return ModeGrok43, true
	case "console":
		return ModeConsole, true
	}
	return ModeAuto, false
}

// Tier is the account pool tier.
type Tier int

const (
	TierBasic Tier = 0
	TierSuper Tier = 1
	TierHeavy Tier = 2
)

func (t Tier) Name() string {
	switch t {
	case TierBasic:
		return "basic"
	case TierSuper:
		return "super"
	case TierHeavy:
		return "heavy"
	}
	return "basic"
}

// TierFromName parses a pool name case-insensitively, ignoring surrounding
// spaces. An empty name means the basic tier.
func TierFromName(name string) (Tier, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "basic", "":
		return TierBasic, true
	case "super":
		return TierSuper, true
	case "heavy":
		return TierHeavy, true
	}
	return TierBasic, false
}

// Capability is a bitmask of model abilities.
type Capability int

const (
	CapChat      Capability = 1
	CapImage     Capability = 2
	CapImageEdit Capability = 4
	CapVideo     Capability = 8
	// Bits 16 and 32 were reserved for voice/asset models that never
	// shipped; CapConsoleChat keeps its value.
	CapConsoleChat Capability = 64
)

// Spec describes a single model entry.
type Spec struct {
	ModelName  string
	ModeId     ModeId
	Tier       Tier
	Capability Capability
	Enabled    bool
	PublicName string
	PreferBest bool
	// Custom marks an entry added at runtime (not part of the built-in table).
	Custom bool
}

func (s *Spec) IsChat() bool        { return s.Capability&CapChat != 0 }
func (s *Spec) IsImage() bool       { return s.Capability&CapImage != 0 }
func (s *Spec) IsImageEdit() bool   { return s.Capability&CapImageEdit != 0 }
func (s *Spec) IsVideo() bool       { return s.Capability&CapVideo != 0 }
func (s *Spec) IsConsoleChat() bool { return s.Capability&CapConsoleChat != 0 }

// PoolCandidates returns the priority-ordered pool ids used for account
// reservation. When PreferBest is set, higher tiers are tried first.
func (s *Spec) PoolCandidates() []int {
	const basic, super, heavy = int(TierBasic), int(TierSuper), int(TierHeavy)
	switch s.Tier {
	case TierBasic:
		if s.PreferBest {
			return []int{heavy, super, basic}
		}
		return []int{basic, super, heavy}
	case TierSuper:
		if s.PreferBest {
			return []int{heavy, super}
		}
		return []int{super, heavy}
	case TierHeavy:
		return []int{heavy}
	}
	return []int{basic}
}

// PoolName returns the primary tier name.
func (s *Spec) PoolName() string { return s.Tier.Name() }

var registry = newRegistry()

// registryT is safe for concurrent use: the admin panel can add, remove and
// toggle models while requests are being served. Spec values are treated as
// immutable once published; changes replace the pointer.
type registryT struct {
	mu     sync.RWMutex
	specs  []*Spec
	byName map[string]*Spec
}

func newRegistry() *registryT {
	r := &registryT{byName: map[string]*Spec{}}
	for _, s := range allModels {
		cp := s
		r.specs = append(r.specs, &cp)
		r.byName[s.ModelName] = &cp
	}
	return r
}

// Get returns the spec for a model name (nil if unknown).
func Get(name string) *Spec {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return registry.byName[name]
}

// Resolve returns the spec or false if unknown/disabled.
func Resolve(name string) (*Spec, bool) {
	s := Get(name)
	if s == nil || !s.Enabled {
		return nil, false
	}
	return s, true
}

// ListEnabled returns all enabled specs in registration order.
func ListEnabled() []*Spec {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]*Spec, 0, len(registry.specs))
	for _, s := range registry.specs {
		if s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

// All returns every spec, enabled or not, in registration order.
func All() []*Spec {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return append([]*Spec(nil), registry.specs...)
}

// ErrModelExists is returned when registering a name that is already taken.
var ErrModelExists = errors.New("model already exists")

// Register adds a runtime model. Built-in names cannot be shadowed.
func Register(s Spec) error {
	name := strings.TrimSpace(s.ModelName)
	if name == "" {
		return errors.New("empty model name")
	}
	s.ModelName = name
	s.Custom = true
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, ok := registry.byName[name]; ok {
		return ErrModelExists
	}
	cp := s
	registry.specs = append(registry.specs, &cp)
	registry.byName[name] = &cp
	return nil
}

// Remove deletes a custom model; built-in models cannot be removed.
func Remove(name string) bool {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	cur := registry.byName[name]
	if cur == nil || !cur.Custom {
		return false
	}
	delete(registry.byName, name)
	for i, s := range registry.specs {
		if s == cur {
			registry.specs = append(registry.specs[:i], registry.specs[i+1:]...)
			break
		}
	}
	return true
}

// SetEnabled turns a model on or off (built-in or custom).
func SetEnabled(name string, enabled bool) bool {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	cur := registry.byName[name]
	if cur == nil {
		return false
	}
	if cur.Enabled == enabled {
		return true
	}
	cp := *cur
	cp.Enabled = enabled
	registry.byName[name] = &cp
	for i, s := range registry.specs {
		if s == cur {
			registry.specs[i] = &cp
			break
		}
	}
	return true
}
