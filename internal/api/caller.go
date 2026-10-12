package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// Caller is the identity of one API request, established once by the auth
// middleware and then passed explicitly (never re-derived) to everything that
// acts on the caller's behalf, including background goroutines.
type Caller struct {
	// PoolAllowed reports whether the caller may draw accounts from the shared
	// pool. It is false for bring-your-own-token callers.
	PoolAllowed bool
	// SSOToken is the caller's own upstream SSO token, if it supplied one that
	// is not a gateway key. It is the only credential a non-pool caller may use.
	SSOToken string
	// KeyID identifies the gateway key that authenticated the caller ("" for
	// open mode and for bring-your-own-token callers).
	KeyID string
}

type callerCtxKey struct{}

// withCaller stores c in the context.
func withCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerCtxKey{}, c)
}

// callerFrom returns the Caller stored in ctx. A context without one belongs
// to no authenticated request, so it gets no pool access and no token.
func callerFrom(ctx context.Context) Caller {
	c, _ := ctx.Value(callerCtxKey{}).(Caller)
	return c
}

// OwnerID is a stable, non-secret identifier used to scope per-caller
// resources (video jobs). Open-mode anonymous callers share the empty owner.
func (c Caller) OwnerID() string {
	switch {
	case c.KeyID != "":
		return "key:" + c.KeyID
	case c.SSOToken != "":
		sum := sha256.Sum256([]byte(c.SSOToken))
		return "sso:" + hex.EncodeToString(sum[:8])
	}
	return ""
}
