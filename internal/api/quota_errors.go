package api

import "errors"

// Errors reported by the quota adapters. They are internal signals: the syncer
// converts any fetch error into an unavailable snapshot, which is display-only
// and never drains an account.
var (
	errNoConfig            = errors.New("quota: config unavailable")
	errProviderUnavailable = errors.New("quota: provider unavailable")
	errNoQuotaEndpoint     = errors.New("quota: provider has no quota endpoint")
	errAccountGone         = errors.New("quota: account no longer configured")
)
