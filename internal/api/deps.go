package api

import (
	"context"
	"io"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/config"
	"github.com/gentpan/grok2api/internal/grok"
	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/storage"
)

// The interfaces in this file are consumer-side: they list exactly what the
// HTTP layer needs from the packages it sits on top of. The concrete
// *account.Directory, *account.RefreshService, *grok.Transport, ... satisfy
// them unchanged; tests substitute fakes.

// Pool is the runtime account table (account.Directory).
type Pool interface {
	Reserve(poolCandidates []int, modeID int, excludeTokens []string, preferTags []string) (*account.Lease, int)
	Release(l *account.Lease)
	Feedback(token string, kind account.FeedbackKind, modeID int, remaining *int, resetAtMs *int64)
	HasPoolMode(poolID, modeID int) bool
	Size() int
	Revision() int
	Strategy() account.Strategy
	SetStrategy(s account.Strategy)
	SyncIfChanged(ctx context.Context) (bool, error)
}

// Refresher is the upstream quota refresh / failure recording service
// (account.RefreshService).
type Refresher interface {
	RefreshTokens(ctx context.Context, tokens []string) (refreshed, failed int, err error)
	RefreshOnDemand(ctx context.Context) error
	RecordFailure(ctx context.Context, token string, modeID int, err error)
}

// Upstream is everything the handlers ask of grok.com / console.x.ai.
type Upstream interface {
	PostStream(ctx context.Context, urlStr, token string, payload []byte, opts ...grok.RequestOption) (io.ReadCloser, error)
	GetJSON(ctx context.Context, urlStr, token string, opts ...grok.RequestOption) (map[string]any, error)
	GetBytes(ctx context.Context, urlStr, token string, opts ...grok.RequestOption) (io.ReadCloser, error)

	// StreamGatewayChat drives the grok-4.6 WebSocket gateway.
	StreamGatewayChat(ctx context.Context, token, modelName, prompt string, attachments []string) <-chan grok.GatewayEvent
	// StreamImages drives the WebSocket imagine endpoint.
	StreamImages(ctx context.Context, token, prompt, aspectRatio string, n int, enableNSFW, enablePro bool) <-chan grok.ImagineEvent

	ConsoleImageGeneration(ctx context.Context, token string, payload map[string]any) (map[string]any, error)
	ConsoleImageEdit(ctx context.Context, token string, payload map[string]any) (map[string]any, error)
	ConsoleVideoGeneration(ctx context.Context, token string, payload map[string]any) (map[string]any, error)
	ConsoleVideoStatus(ctx context.Context, token, requestID string) (map[string]any, error)
	DownloadConsoleVideo(ctx context.Context, rawURL string) (io.ReadCloser, error)

	// Account asset / NSFW management (admin batch operations).
	ListAssets(ctx context.Context, token string) (map[string]any, error)
	DeleteAsset(ctx context.Context, token, assetID string) (map[string]any, error)
	NSFWSequence(ctx context.Context, token string) error
	DisableNSFW(ctx context.Context, token string) error

	// UsageFetcher returns the quota fetcher bound to this upstream.
	UsageFetcher() account.UsageFetcher
}

// grokUpstream adapts *grok.Transport (plus the grok package functions that
// take a transport argument) to Upstream.
type grokUpstream struct{ *grok.Transport }

func (g grokUpstream) StreamImages(ctx context.Context, token, prompt, aspectRatio string, n int, enableNSFW, enablePro bool) <-chan grok.ImagineEvent {
	return grok.NewImagineStream(token).StreamImagesContext(ctx, prompt, aspectRatio, n, enableNSFW, enablePro)
}

func (g grokUpstream) ListAssets(ctx context.Context, token string) (map[string]any, error) {
	return grok.ListAssets(ctx, g.Transport, token)
}

func (g grokUpstream) DeleteAsset(ctx context.Context, token, assetID string) (map[string]any, error) {
	return grok.DeleteAsset(ctx, g.Transport, token, assetID)
}

func (g grokUpstream) NSFWSequence(ctx context.Context, token string) error {
	return grok.NSFWSequence(ctx, g.Transport, token)
}

func (g grokUpstream) DisableNSFW(ctx context.Context, token string) error {
	_, err := grok.DisableNSFW(ctx, g.Transport, token)
	return err
}

func (g grokUpstream) UsageFetcher() account.UsageFetcher { return grok.NewUsageFetcher(g.Transport) }

// MediaStore is the local media cache (storage.LocalMediaCacheStore).
type MediaStore interface {
	SaveVideo(raw []byte, fileID string) (string, error)
	Delete(mediaType storage.MediaType, name string) (bool, error)
	Clear(mediaType storage.MediaType) (int, error)
}

// ConversationStore tracks upstream conversation continuation per account
// (grok.ConversationTracker).
type ConversationStore interface {
	Get(token string) *grok.ConversationState
	Set(token, conversationID, responseID string)
	Clear(token string)
}

// ConfigStore is the runtime configuration (config.Snapshot).
type ConfigStore interface {
	GetStr(key, def string) string
	GetInt(key string, def int) int
	GetBool(key string, def bool) bool
	GetList(key string, def []string) []string
	Raw() map[string]any
	Update(patch map[string]any) error
	Load() error
}

// ModelRegistry is the model catalogue. The default implementation is the
// process-wide registry of package model; the Server owns every mutation of
// it (custom models added from the panel).
type ModelRegistry interface {
	Resolve(name string) (*model.Spec, bool)
	Get(name string) *model.Spec
	ListEnabled() []*model.Spec
	All() []*model.Spec
	Register(s model.Spec) error
	Remove(name string) bool
	SetEnabled(name string, enabled bool) bool
}

// globalModels delegates to the package-level registry of package model.
type globalModels struct{}

func (globalModels) Resolve(name string) (*model.Spec, bool) { return model.Resolve(name) }
func (globalModels) Get(name string) *model.Spec             { return model.Get(name) }
func (globalModels) ListEnabled() []*model.Spec              { return model.ListEnabled() }
func (globalModels) All() []*model.Spec                      { return model.All() }
func (globalModels) Register(s model.Spec) error             { return model.Register(s) }
func (globalModels) Remove(name string) bool                 { return model.Remove(name) }
func (globalModels) SetEnabled(name string, e bool) bool     { return model.SetEnabled(name, e) }

// globalConfig delegates to the process-wide config singleton, resolved on
// every call so config.SetPaths in tests is honoured.
type globalConfig struct{}

func (globalConfig) snap() *config.Snapshot { return config.Global() }

func (g globalConfig) GetStr(key, def string) string             { return g.snap().GetStr(key, def) }
func (g globalConfig) GetInt(key string, def int) int            { return g.snap().GetInt(key, def) }
func (g globalConfig) GetBool(key string, def bool) bool         { return g.snap().GetBool(key, def) }
func (g globalConfig) GetList(key string, def []string) []string { return g.snap().GetList(key, def) }
func (g globalConfig) Raw() map[string]any                       { return g.snap().Raw() }
func (g globalConfig) Update(patch map[string]any) error         { return g.snap().Update(patch) }
func (g globalConfig) Load() error                               { return g.snap().Load() }
