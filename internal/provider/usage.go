package provider

import "context"

// TokenUsage is the accounting one completed upstream response reported.
// Cached and cache-creation tokens are slices of input, and reasoning tokens
// are a slice of output: each field is the count the upstream named, not a
// remainder. A field the upstream omitted stays zero.
//
// Account is the credential identity that served the response, so two logins
// in one workspace stay distinct. It is empty when the provider has no
// account identity.
//
// Present is false when the response carried no usage object. Callers drop
// that sample: a missing count is not zero tokens.
type TokenUsage struct {
	InputTokens              int
	OutputTokens             int
	CachedTokens             int
	CacheCreationInputTokens int
	ReasoningTokens          int
	Account                  string
	Present                  bool
}

type usageSinkKey struct{}

// WithUsageSink returns a context that delivers one TokenUsage per completed
// upstream response to report. The callback runs on the request goroutine
// before ChatCompletion returns. A nil report leaves ctx unchanged.
func WithUsageSink(ctx context.Context, report func(TokenUsage)) context.Context {
	if report == nil {
		return ctx
	}
	return context.WithValue(ctx, usageSinkKey{}, report)
}

// ReportUsage delivers usage when ctx carries a sink and the response
// actually reported one. A nil context, a missing sink, or an absent usage
// object is a no-op, so providers stay safe to call without instrumentation.
func ReportUsage(ctx context.Context, usage TokenUsage) {
	if ctx == nil || !usage.Present {
		return
	}
	report, _ := ctx.Value(usageSinkKey{}).(func(TokenUsage))
	if report == nil {
		return
	}
	report(usage)
}
