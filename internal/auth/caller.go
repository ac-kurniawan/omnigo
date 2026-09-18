package auth

import (
	"context"

	"github.com/ac-kurniawan/omnigo/internal/vault"
)

type callerKey struct{}

// Caller is the per-request record of the gateway client key that
// authenticated a request. The instrumentation middleware that wraps the mux
// installs one with WithCaller before dispatch and reads it once the request
// completes; Middleware fills it in after the key hash validates.
//
// The outer middleware keeps the request it was handed, so a context value
// installed inside Middleware would be invisible above it. Sharing a pointer
// instead lets the identity escape the middleware that records it. The field is
// written before the handler runs and read after, on the same goroutine, so no
// lock is needed.
//
// A Caller that was never filled in records the empty id, which surfaces as a
// "none" label: requests that carry no validated key (dashboard, health, an
// unauthenticated 401) need no special handling.
type Caller struct {
	id string
}

// Set records the key that authenticated the request. It is safe to call on a
// nil Caller, so Middleware works unchanged on a router that is served without
// the instrumentation wrapper that installs the holder.
func (c *Caller) Set(key vault.ClientKey) {
	if c == nil {
		return
	}
	c.id = key.ID
}

// ID returns the id of the key that authenticated the request, or "" when the
func (c *Caller) ID() string {
	if c == nil {
		return ""
	}
	return c.id
}

// WithCaller installs c as the request's caller identity.
func WithCaller(ctx context.Context, c *Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the Caller installed on ctx, or nil when none was.
func CallerFrom(ctx context.Context) *Caller {
	c, _ := ctx.Value(callerKey{}).(*Caller)
	return c
}
