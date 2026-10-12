package account

import "context"

// Upsert is a create-or-replace account command.
type Upsert struct {
	Token string
	Pool  string
	Tags  []string
	Ext   map[string]any
}

// Patch mutates an existing account (only set fields are applied).
type Patch struct {
	Token          string
	Pool           *string
	Status         *Status
	Tags           []string
	AddTags        []string
	RemoveTags     []string
	QuotaAuto      *map[string]any
	QuotaFast      *map[string]any
	QuotaExpert    *map[string]any
	QuotaHeavy     *map[string]any
	QuotaGrok43    *map[string]any
	QuotaConsole   *map[string]any
	UsageUseDelta  *int
	UsageFailDelta *int
	UsageSyncDelta *int
	LastUseAt      *int64
	LastFailAt     *int64
	LastFailReason *string
	LastSyncAt     *int64
	LastClearAt    *int64
	StateReason    *string
	ExtMerge       map[string]any
	ClearFailures  bool
}

// ListQuery filters and paginates the account list.
type ListQuery struct {
	Page           int
	PageSize       int
	Pool           string
	Status         *Status
	Tags           []string
	IncludeDeleted bool
	SortBy         string
	SortDesc       bool
}

// Page is a paginated slice of records.
type Page struct {
	Items      []*Record
	Total      int
	Page       int
	PageSize   int
	TotalPages int
	Revision   int
	// Clamped reports that the requested PageSize exceeded MaxPageSize and was
	// reduced; PageSize holds the effective value and Total the full count.
	Clamped bool
}

// MaxPageSize is the largest page ListAccounts returns.
const MaxPageSize = 5000

// MutationResult summarizes a repository mutation.
type MutationResult struct {
	Upserted int
	Patched  int
	Deleted  int
	Revision int
}

// ChangeSet is an incremental scan result.
type ChangeSet struct {
	Revision      int
	BatchMaxRev   int
	Items         []*Record
	DeletedTokens []string
	HasMore       bool
	// Stale is set when tombstones newer than the caller's cursor were purged:
	// the caller may have missed deletions and must resynchronise from
	// RuntimeSnapshot (Directory does this automatically).
	Stale bool
}

// Snapshot is the full runtime view.
type Snapshot struct {
	Revision int
	Items    []*Record
}

// Repository is the storage contract for accounts.
//
// Implementations are safe for concurrent use and return deep copies.
// Mutations that change nothing do not consume a revision.
type Repository interface {
	Initialize(ctx context.Context) error
	GetRevision(ctx context.Context) (int, error)
	RuntimeSnapshot(ctx context.Context) (*Snapshot, error)
	ScanChanges(ctx context.Context, since int, limit int) (*ChangeSet, error)
	UpsertAccounts(ctx context.Context, items []Upsert) (*MutationResult, error)
	PatchAccounts(ctx context.Context, patches []Patch) (*MutationResult, error)
	DeleteAccounts(ctx context.Context, tokens []string) (*MutationResult, error)
	GetAccounts(ctx context.Context, tokens []string) ([]*Record, error)
	ListAccounts(ctx context.Context, query ListQuery) (*Page, error)
	ReplacePool(ctx context.Context, pool string, upserts []Upsert) (*MutationResult, error)
	Close(ctx context.Context) error
}

// ConsoleMaintainer is the optional capability of a store to run the console
// quota housekeeping jobs. RefreshService runs them when its repository
// implements it (TxtRepository does).
type ConsoleMaintainer interface {
	// ResetExpiredConsoleWindows restores the default console window of
	// accounts whose window reset time has passed; returns how many.
	ResetExpiredConsoleWindows(ctx context.Context) (int, error)
	// RecoverConsoleExpiredAccounts re-activates accounts expired for console
	// 429s long enough ago; returns how many.
	RecoverConsoleExpiredAccounts(ctx context.Context) (int, error)
}

var (
	_ Repository        = (*TxtRepository)(nil)
	_ ConsoleMaintainer = (*TxtRepository)(nil)
)

// quota returns the quota-window field for a mode (nil when unset).
func (p *Patch) quota(modeID int) *map[string]any {
	switch modeID {
	case ModeAuto:
		return p.QuotaAuto
	case ModeFast:
		return p.QuotaFast
	case ModeExpert:
		return p.QuotaExpert
	case ModeHeavy:
		return p.QuotaHeavy
	case ModeGrok43:
		return p.QuotaGrok43
	case ModeConsole:
		return p.QuotaConsole
	}
	return nil
}

// setQuota sets the quota-window field for a mode.
func (p *Patch) setQuota(modeID int, m *map[string]any) {
	switch modeID {
	case ModeAuto:
		p.QuotaAuto = m
	case ModeFast:
		p.QuotaFast = m
	case ModeExpert:
		p.QuotaExpert = m
	case ModeHeavy:
		p.QuotaHeavy = m
	case ModeGrok43:
		p.QuotaGrok43 = m
	case ModeConsole:
		p.QuotaConsole = m
	}
}
