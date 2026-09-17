package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestParseTraceparent(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		wantTrace  string
		wantParent string
		wantSample bool
		wantOK     bool
	}{
		{
			name:       "valid standard header",
			header:     "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			wantTrace:  "4bf92f3577b34da6a3ce929d0e0e4736",
			wantParent: "00f067aa0ba902b7",
			wantSample: true,
			wantOK:     true,
		},
		{
			name:       "valid unsampled header",
			header:     "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00",
			wantTrace:  "4bf92f3577b34da6a3ce929d0e0e4736",
			wantParent: "00f067aa0ba902b7",
			wantSample: false,
			wantOK:     true,
		},
		{
			name:   "invalid empty",
			header: "",
			wantOK: false,
		},
		{
			name:   "invalid length",
			header: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",
			wantOK: false,
		},
		{
			name:   "invalid all-zero trace id",
			header: "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
			wantOK: false,
		},
		{
			name:   "invalid all-zero parent id",
			header: "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
			wantOK: false,
		},
		{
			name:   "invalid non-hex characters",
			header: "00-4bf92f3577b34da6a3ce929d0e0e473z-00f067aa0ba902b7-01",
			wantOK: false,
		},
		{
			name:   "invalid delimiter",
			header: "00_4bf92f3577b34da6a3ce929d0e0e4736_00f067aa0ba902b7_01",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			traceID, parentID, sampled, ok := ParseTraceparent(tt.header)
			if ok != tt.wantOK {
				t.Fatalf("ParseTraceparent() ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if traceID != tt.wantTrace {
				t.Errorf("traceID = %q, want %q", traceID, tt.wantTrace)
			}
			if parentID != tt.wantParent {
				t.Errorf("parentID = %q, want %q", parentID, tt.wantParent)
			}
			if sampled != tt.wantSample {
				t.Errorf("sampled = %v, want %v", sampled, tt.wantSample)
			}
		})
	}
}

func TestNewTraceContext(t *testing.T) {
	t.Run("generates fresh trace when missing", func(t *testing.T) {
		tc := NewTraceContext("")
		if len(tc.TraceID) != 32 {
			t.Errorf("expected 32-char hex traceID, got %q", tc.TraceID)
		}
		if len(tc.SpanID) != 16 {
			t.Errorf("expected 16-char hex spanID, got %q", tc.SpanID)
		}
		if tc.TraceID == "00000000000000000000000000000000" {
			t.Errorf("traceID cannot be all zeroes")
		}
		tp := tc.Traceparent()
		if !strings.HasPrefix(tp, "00-"+tc.TraceID+"-"+tc.SpanID+"-") {
			t.Errorf("unexpected Traceparent format: %q", tp)
		}
	})

	t.Run("preserves inbound traceID and spawns child spanID", func(t *testing.T) {
		inbound := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		tc := NewTraceContext(inbound)
		if tc.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Errorf("expected preserved traceID, got %q", tc.TraceID)
		}
		if tc.SpanID == "00f067aa0ba902b7" {
			t.Errorf("spanID must be a new child spanID, got same as parent")
		}
		if len(tc.SpanID) != 16 {
			t.Errorf("expected 16-char hex spanID, got %q", tc.SpanID)
		}
	})
}

func TestFormatServerTiming(t *testing.T) {
	st := FormatServerTiming("antigravity", "gemini-2.5-pro", 150*time.Millisecond)
	if !strings.Contains(st, `gen_ai.system;desc="antigravity"`) {
		t.Errorf("expected gen_ai.system attribute, got %q", st)
	}
	if !strings.Contains(st, `gen_ai.response.model;desc="gemini-2.5-pro"`) {
		t.Errorf("expected gen_ai.response.model attribute, got %q", st)
	}
	if !strings.Contains(st, "dur=150.00") {
		t.Errorf("expected dur=150.00, got %q", st)
	}
}

func TestHandleChatEmitsTraceAndTelemetryHeaders(t *testing.T) {
	provider.Register("telemetry-test", func(cfg provider.Config, store provider.CredStore) provider.Provider {
		return &fakeProvider{
			name: cfg.Name,
			chat: func(r provider.ChatRequest) error {
				return nil
			},
		}
	})

	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "telemetry-test", Type: "telemetry-test", Models: []string{"test-model"}},
		},
	}

	reg := newProviderRegistry(nil)
	reg.ensure(cfg)

	h := handleChat(func() *config.Config { return cfg }, reg, nil, nil)

	reqBody := `{"model":"telemetry-test/test-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	inboundTrace := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	req.Header.Set("traceparent", inboundTrace)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	resp := w.Result()
	tp := resp.Header.Get("traceparent")
	if !strings.HasPrefix(tp, "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
		t.Errorf("expected response traceparent to preserve traceID, got %q", tp)
	}

	st := resp.Header.Get("Server-Timing")
	if !strings.Contains(st, `gen_ai.system;desc="telemetry-test"`) {
		t.Errorf("expected Server-Timing with gen_ai.system, got %q", st)
	}
	if !strings.Contains(st, `gen_ai.response.model;desc="test-model"`) {
		t.Errorf("expected Server-Timing with gen_ai.response.model, got %q", st)
	}

	provHeader := resp.Header.Get("X-OmniGo-Provider")
	if provHeader != "telemetry-test" {
		t.Errorf("expected X-OmniGo-Provider to be telemetry-test, got %q", provHeader)
	}
	modelHeader := resp.Header.Get("X-OmniGo-Model")
	if modelHeader != "test-model" {
		t.Errorf("expected X-OmniGo-Model to be test-model, got %q", modelHeader)
	}
}
