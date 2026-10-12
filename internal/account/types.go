// Package account implements the multi-account pool: the persistent record
// model and quota windows (types.go, quota.go), the storage repository
// (repository.go, txt.go), the in-memory runtime directory used for fast
// account selection (directory.go, select.go, feedback.go) and the upstream
// quota refresh pipeline (refresh.go).
package account

import (
	"encoding/json"
	"sort"
	"strings"
)

// Status is the persistent lifecycle status of an account.
type Status string

const (
	StatusActive   Status = "active"
	StatusCooling  Status = "cooling"
	StatusExpired  Status = "expired"
	StatusDisabled Status = "disabled"
)

// StatusID is the dataplane integer encoding of a status.
type StatusID int

const (
	StatusIDActive   StatusID = 0
	StatusIDCooling  StatusID = 1
	StatusIDExpired  StatusID = 2
	StatusIDDisabled StatusID = 3
)

func (s Status) ID() StatusID {
	switch s {
	case StatusActive:
		return StatusIDActive
	case StatusCooling:
		return StatusIDCooling
	case StatusExpired:
		return StatusIDExpired
	case StatusDisabled:
		return StatusIDDisabled
	}
	return StatusIDActive
}

// PoolID is the integer pool tier (0=basic, 1=super, 2=heavy).
type PoolID int

const (
	PoolBasic PoolID = 0
	PoolSuper PoolID = 1
	PoolHeavy PoolID = 2
)

// PoolFromName resolves a pool name case-insensitively; "" means basic. The
// bool is false for an unknown name (the returned PoolID is then PoolBasic).
func PoolFromName(name string) (PoolID, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "basic", "":
		return PoolBasic, true
	case "super":
		return PoolSuper, true
	case "heavy":
		return PoolHeavy, true
	}
	return PoolBasic, false
}

func (p PoolID) Name() string {
	switch p {
	case PoolSuper:
		return "super"
	case PoolHeavy:
		return "heavy"
	}
	return "basic"
}

// FeedbackKind is the outcome of one upstream request.
type FeedbackKind string

const (
	FbSuccess      FeedbackKind = "success"
	FbUnauthorized FeedbackKind = "unauthorized"
	FbForbidden    FeedbackKind = "forbidden"
	FbRateLimited  FeedbackKind = "rate_limited"
	FbServerError  FeedbackKind = "server_error"
	FbDisable      FeedbackKind = "disable"
	FbDelete       FeedbackKind = "delete"
	FbRestore      FeedbackKind = "restore"
)

// FeedbackKindFromStatus maps an HTTP status code to a feedback kind.
func FeedbackKindFromStatus(status int) FeedbackKind {
	switch {
	case status == 401:
		return FbUnauthorized
	case status == 403:
		return FbForbidden
	case status == 429:
		return FbRateLimited
	case status >= 500:
		return FbServerError
	case status >= 200 && status < 300:
		return FbSuccess
	}
	return FbServerError
}

// QuotaSource tracks how reliable a quota window value is.
type QuotaSource int

const (
	SourceDefault   QuotaSource = 0
	SourceReal      QuotaSource = 1
	SourceEstimated QuotaSource = 2
)

// QuotaWindow is the per-mode rate-limit window.
type QuotaWindow struct {
	Remaining     int         `json:"remaining"`
	Total         int         `json:"total"`
	WindowSeconds int         `json:"window_seconds"`
	ResetAt       *int64      `json:"reset_at"`
	SyncedAt      *int64      `json:"synced_at"`
	Source        QuotaSource `json:"source"`
}

func (w QuotaWindow) IsExhausted() bool { return w.Remaining <= 0 }

func (w QuotaWindow) IsWindowExpired(nowMs int64) bool {
	if w.ResetAt == nil {
		return false
	}
	return nowMs >= *w.ResetAt
}

// ToMap / FromMap helpers for JSON serialization in the ext/quota columns.
func (w QuotaWindow) ToMap() map[string]any {
	m := map[string]any{
		"remaining":      w.Remaining,
		"total":          w.Total,
		"window_seconds": w.WindowSeconds,
		"source":         int(w.Source),
	}
	if w.ResetAt != nil {
		m["reset_at"] = *w.ResetAt
	}
	if w.SyncedAt != nil {
		m["synced_at"] = *w.SyncedAt
	}
	return m
}

func QuotaWindowFromMap(m map[string]any) QuotaWindow {
	var w QuotaWindow
	if v, ok := m["remaining"]; ok {
		w.Remaining = toInt(v)
	}
	if v, ok := m["total"]; ok {
		w.Total = toInt(v)
	}
	if v, ok := m["window_seconds"]; ok {
		w.WindowSeconds = toInt(v)
	}
	if v, ok := m["source"]; ok {
		w.Source = QuotaSource(toInt(v))
	}
	if v, ok := m["reset_at"]; ok {
		n := toInt64(v)
		w.ResetAt = &n
	}
	if v, ok := m["synced_at"]; ok {
		n := toInt64(v)
		w.SyncedAt = &n
	}
	return w
}

// QuotaSet holds one window per mode (0..5).
type QuotaSet struct {
	Auto    QuotaWindow
	Fast    QuotaWindow
	Expert  QuotaWindow
	Heavy   *QuotaWindow
	Grok43  *QuotaWindow
	Console *QuotaWindow
}

func (q *QuotaSet) Get(modeID int) *QuotaWindow {
	switch modeID {
	case 0:
		return &q.Auto
	case 1:
		return &q.Fast
	case 2:
		return &q.Expert
	case 3:
		return q.Heavy
	case 4:
		return q.Grok43
	case 5:
		return q.Console
	}
	return nil
}

func (q *QuotaSet) Set(modeID int, w QuotaWindow) {
	switch modeID {
	case 0:
		q.Auto = w
	case 1:
		q.Fast = w
	case 2:
		q.Expert = w
	case 3:
		cw := w
		q.Heavy = &cw
	case 4:
		cw := w
		q.Grok43 = &cw
	case 5:
		cw := w
		q.Console = &cw
	}
}

// ToMap serializes the quota set (only non-empty windows for heavy/grok43/console).
func (q QuotaSet) ToMap() map[string]any {
	m := map[string]any{
		QuotaKey(ModeAuto):   q.Auto.ToMap(),
		QuotaKey(ModeFast):   q.Fast.ToMap(),
		QuotaKey(ModeExpert): q.Expert.ToMap(),
	}
	for modeID := ModeHeavy; modeID <= ModeConsole; modeID++ {
		if w := q.Get(modeID); w != nil {
			m[QuotaKey(modeID)] = w.ToMap()
		}
	}
	return m
}

func QuotaSetFromMap(m map[string]any) QuotaSet {
	var q QuotaSet
	for modeID := ModeAuto; modeID <= ModeConsole; modeID++ {
		if v, ok := m[QuotaKey(modeID)].(map[string]any); ok {
			q.Set(modeID, QuotaWindowFromMap(v))
		}
	}
	return q
}

// Record is the single source of truth for an account across all backends.
type Record struct {
	Token          string         `json:"token"`
	Pool           string         `json:"pool"`
	Status         Status         `json:"status"`
	CreatedAt      int64          `json:"created_at"`
	UpdatedAt      int64          `json:"updated_at"`
	Tags           []string       `json:"tags"`
	Quota          map[string]any `json:"quota"`
	UsageUseCount  int            `json:"usage_use_count"`
	UsageFailCount int            `json:"usage_fail_count"`
	UsageSyncCount int            `json:"usage_sync_count"`
	LastUseAt      *int64         `json:"last_use_at"`
	LastFailAt     *int64         `json:"last_fail_at"`
	LastFailReason *string        `json:"last_fail_reason"`
	LastSyncAt     *int64         `json:"last_sync_at"`
	LastClearAt    *int64         `json:"last_clear_at"`
	StateReason    *string        `json:"state_reason"`
	DeletedAt      *int64         `json:"deleted_at"`
	Ext            map[string]any `json:"ext"`
	Revision       int            `json:"revision"`
}

// Clone returns a deep copy of the record: Tags, Quota, Ext (including nested
// maps/slices) and all pointer fields are duplicated, so the copy can be read or
// mutated without holding the repository lock.
func (r *Record) Clone() *Record {
	if r == nil {
		return nil
	}
	cp := *r
	if r.Tags != nil {
		cp.Tags = make([]string, len(r.Tags))
		copy(cp.Tags, r.Tags)
	}
	cp.Quota = cloneAnyMap(r.Quota)
	cp.Ext = cloneAnyMap(r.Ext)
	cp.LastUseAt = cloneInt64Ptr(r.LastUseAt)
	cp.LastFailAt = cloneInt64Ptr(r.LastFailAt)
	cp.LastSyncAt = cloneInt64Ptr(r.LastSyncAt)
	cp.LastClearAt = cloneInt64Ptr(r.LastClearAt)
	cp.DeletedAt = cloneInt64Ptr(r.DeletedAt)
	if r.LastFailReason != nil {
		v := *r.LastFailReason
		cp.LastFailReason = &v
	}
	if r.StateReason != nil {
		v := *r.StateReason
		cp.StateReason = &v
	}
	return &cp
}

func cloneInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneAnyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneAny(e)
		}
		return out
	case []string:
		out := make([]string, len(t))
		copy(out, t)
		return out
	default:
		return v
	}
}

func (r *Record) QuotaSet() QuotaSet {
	if r.Quota == nil {
		return DefaultQuotaSet(r.Pool)
	}
	return QuotaSetFromMap(r.Quota)
}

func (r *Record) IsDeleted() bool { return r.DeletedAt != nil }

// DeriveStatus resolves the effective status (COOLING may become ACTIVE once
// cooldown expires).
func (r *Record) DeriveStatus(nowMs int64) Status {
	if r.Status != StatusCooling {
		return r.Status
	}
	if r.Ext == nil {
		return StatusCooling
	}
	until := toInt64(r.Ext[ExtCooldownUntil])
	if until == 0 {
		return StatusCooling
	}
	if nowMs >= until {
		return StatusActive
	}
	return StatusCooling
}

// SortTags dedups and sorts tags in place.
func SortTags(tags []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// MergeTags applies add/remove set arithmetic to a tag slice.
func MergeTags(current []string, add, remove []string) []string {
	set := map[string]bool{}
	for _, t := range current {
		if t != "" {
			set[t] = true
		}
	}
	for _, t := range add {
		if t != "" {
			set[t] = true
		}
	}
	for _, t := range remove {
		delete(set, t)
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// --- helpers ---

func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return 0
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case int32:
		return int64(t)
	case float64:
		return int64(t)
	case float32:
		return int64(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n
		}
		if f, err := t.Float64(); err == nil {
			return int64(f)
		}
	}
	return 0
}

func ptrInt(v int) *int       { return &v }
func ptrInt64(v int64) *int64 { return &v }
