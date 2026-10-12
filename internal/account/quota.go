package account

// Default quota windows per pool/mode, mirroring the upstream defaults.
//
//   auto    fast    expert    heavy    grok_4_3   console
// basic  —      30       —        —         —          20     (fast win=86400s; console win=3600s)
// super 50     140      50        —         50          —      (win=7200s)
// heavy 150    400     150       20        150          —      (win=7200s)

// Mode IDs as used by QuotaSet.Get and the pool x mode matrix. They equal the
// model.ModeId values (pinned by TestModeConstantsMatchModel).
const (
	ModeAuto    = 0
	ModeFast    = 1
	ModeExpert  = 2
	ModeHeavy   = 3
	ModeGrok43  = 4
	ModeConsole = 5
)

// quotaKeys are the Record.Quota map keys per mode ID.
var quotaKeys = [...]string{
	ModeAuto:    "auto",
	ModeFast:    "fast",
	ModeExpert:  "expert",
	ModeHeavy:   "heavy",
	ModeGrok43:  "grok_4_3",
	ModeConsole: "console",
}

// QuotaKey returns the key of a mode's window in Record.Quota ("" if unknown).
func QuotaKey(modeID int) string {
	if modeID < 0 || modeID >= len(quotaKeys) {
		return ""
	}
	return quotaKeys[modeID]
}

const (
	BasicFastLimit        = 30
	BasicFastWindowSec    = 86400
	BasicConsoleLimit     = 20
	BasicConsoleWindowSec = 3600
	SuperWindowSec        = 7200
	HeavyWindowSec        = 7200
)

// DefaultQuotaWindow returns the built-in default window for a pool/mode.
func DefaultQuotaWindow(pool string, modeID int) QuotaWindow {
	switch pool {
	case "basic":
		switch modeID {
		case 1:
			return QuotaWindow{Remaining: BasicFastLimit, Total: BasicFastLimit, WindowSeconds: BasicFastWindowSec, Source: SourceDefault}
		case 5:
			return QuotaWindow{Remaining: BasicConsoleLimit, Total: BasicConsoleLimit, WindowSeconds: BasicConsoleWindowSec, Source: SourceDefault}
		}
		return QuotaWindow{}
	case "super":
		switch modeID {
		case 0:
			return QuotaWindow{Remaining: 50, Total: 50, WindowSeconds: SuperWindowSec, Source: SourceDefault}
		case 1:
			return QuotaWindow{Remaining: 140, Total: 140, WindowSeconds: SuperWindowSec, Source: SourceDefault}
		case 2:
			return QuotaWindow{Remaining: 50, Total: 50, WindowSeconds: SuperWindowSec, Source: SourceDefault}
		case 4:
			return QuotaWindow{Remaining: 50, Total: 50, WindowSeconds: SuperWindowSec, Source: SourceDefault}
		}
		return QuotaWindow{}
	case "heavy":
		switch modeID {
		case 0:
			return QuotaWindow{Remaining: 150, Total: 150, WindowSeconds: HeavyWindowSec, Source: SourceDefault}
		case 1:
			return QuotaWindow{Remaining: 400, Total: 400, WindowSeconds: HeavyWindowSec, Source: SourceDefault}
		case 2:
			return QuotaWindow{Remaining: 150, Total: 150, WindowSeconds: HeavyWindowSec, Source: SourceDefault}
		case 3:
			return QuotaWindow{Remaining: 20, Total: 20, WindowSeconds: HeavyWindowSec, Source: SourceDefault}
		case 4:
			return QuotaWindow{Remaining: 150, Total: 150, WindowSeconds: HeavyWindowSec, Source: SourceDefault}
		}
		return QuotaWindow{}
	}
	return QuotaWindow{}
}

// DefaultQuotaSet returns the full quota set for a pool (only supported modes).
func DefaultQuotaSet(pool string) QuotaSet {
	qs := QuotaSet{}
	for _, modeID := range SupportedModeIDs(pool) {
		w := DefaultQuotaWindow(pool, modeID)
		if w.WindowSeconds > 0 {
			qs.Set(modeID, w)
		}
	}
	return qs
}

// SupportedModeIDs returns the modes supported by a pool in stable order 0..5.
func SupportedModeIDs(pool string) []int {
	switch pool {
	case "basic":
		return []int{1, 5}
	case "super":
		return []int{0, 1, 2, 4, 5}
	case "heavy":
		return []int{0, 1, 2, 3, 4, 5}
	}
	return []int{1, 5}
}

// SupportsMode reports whether a pool can serve the given mode.
func SupportsMode(pool string, modeID int) bool {
	for _, m := range SupportedModeIDs(pool) {
		if m == modeID {
			return true
		}
	}
	return false
}

// NormalizeQuotaWindow clamps a basic-pool window to the documented limits.
func NormalizeQuotaWindow(pool string, modeID int, w QuotaWindow) QuotaWindow {
	if pool == "basic" {
		switch modeID {
		case 1:
			if w.Total <= 0 || w.Total > BasicFastLimit {
				w.Total = BasicFastLimit
			}
			if w.WindowSeconds <= 0 {
				w.WindowSeconds = BasicFastWindowSec
			}
		case 5:
			if w.Total <= 0 || w.Total > BasicConsoleLimit {
				w.Total = BasicConsoleLimit
			}
			if w.WindowSeconds <= 0 {
				w.WindowSeconds = BasicConsoleWindowSec
			}
		}
	}
	return w
}

// NormalizeQuotaSet normalizes every window in the set for the pool.
func NormalizeQuotaSet(pool string, qs QuotaSet) QuotaSet {
	for _, modeID := range SupportedModeIDs(pool) {
		w := qs.Get(modeID)
		if w == nil || w.WindowSeconds <= 0 {
			continue
		}
		qs.Set(modeID, NormalizeQuotaWindow(pool, modeID, *w))
	}
	return qs
}

// InferPool infers the pool tier from quota window totals.
// Basic pool: fast=30, no auto/expert/heavy.
// Super pool: auto=50.
// Heavy pool: auto=150.
func InferPool(qs QuotaSet) string {
	switch {
	case qs.Auto.Total == 150:
		return "heavy"
	case qs.Auto.Total == 50:
		return "super"
	case qs.Fast.Total == 30 && qs.Auto.Total == 0:
		return "basic"
	}
	return ""
}
