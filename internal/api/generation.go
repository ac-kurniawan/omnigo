package api

import (
	"context"
	"net/http"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/observability/tracing"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// Generation is one upstream dispatch as the tracing backend sees it. It is
// aliased so call sites name one type; the tracer owns the definition.
type Generation = tracing.Generation

// dispatchCapture accumulates what one dispatch reported. usage is nil until
// the provider reports it; output is nil when content capture is off.
type dispatchCapture struct {
	usage  *provider.TokenUsage
	found  bool
	output *observingWriter
}

// CaptureGeneration reports whether prompt and completion text is exported.
// The switch lives in config so a reload flips it without a restart.
func CaptureGeneration(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	l := cfg.Observability.Langfuse
	return l.EnabledOrDefault() && l.CaptureContentEnabled()
}

// trackAttempt installs the single usage sink for one dispatch and returns the
// writer the provider must write to. Exactly one sink exists per attempt: a
// second WithUsageSink would replace the first and silently drop the token
// counters.
func trackAttempt(ctx context.Context, metrics Metrics, providerName, model, combo string, w http.ResponseWriter, capture bool) (*dispatchCapture, context.Context, http.ResponseWriter) {
	cap := &dispatchCapture{}
	if capture {
		cap.output = &observingWriter{ResponseWriter: w}
		w = cap.output
	}
	ctx = provider.WithUsageSink(ctx, func(usage provider.TokenUsage) {
		u := usage
		cap.usage, cap.found = &u, true
		metrics.RecordTokenUsage(ctx, providerName, model, u.Account, combo, u)
	})
	return cap, ctx, w
}

// generationFrom assembles the generation record for one finished dispatch.
func generationFrom(start, end time.Time, op, provName, model, combo string, cap *dispatchCapture, result string, req provider.ChatRequest, capture bool) Generation {
	g := Generation{
		Provider:    provName,
		Model:       model,
		Combo:       combo,
		Operation:   op,
		Start:       start,
		End:         end,
		Result:      result,
		ModelParams: modelParams(req),
	}
	if cap != nil && cap.found {
		g.Usage = cap.usage
		g.Account = cap.usage.Account
	}
	if capture {
		g.Input = req.Raw
		if cap != nil && cap.output != nil {
			g.Output = cap.output.bytes()
			g.OutputType = cap.output.contentType()
		}
	}
	return g
}

// modelParams keeps only the request knobs worth showing on a generation.
// Parsed carries the internal body-cache slot, so it is never ranged whole.
func modelParams(req provider.ChatRequest) map[string]any {
	if req.Parsed == nil {
		return nil
	}
	out := make(map[string]any, 3)
	for _, key := range []string{"temperature", "max_tokens", "stream"} {
		if v, ok := req.Parsed[key]; ok {
			out[key] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
