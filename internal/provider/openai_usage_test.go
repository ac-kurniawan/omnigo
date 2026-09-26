package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func collectUsage(t *testing.T, p Provider, stream bool) (body string, err error, got []TokenUsage) {
	t.Helper()
	ctx := WithUsageSink(context.Background(), func(usage TokenUsage) {
		got = append(got, usage)
	})
	rec := httptest.NewRecorder()
	err = p.ChatCompletion(ctx, ChatRequest{
		Model: "gpt-4o", Stream: stream, Messages: []Message{{Role: "user", Content: "hi"}},
	}, rec)
	return rec.Body.String(), err, got
}

func TestOpenAINonStreamReportsChatUsage(t *testing.T) {
	const body = `{"id":"c","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	gotBody, err, got := collectUsage(t, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != body {
		t.Fatalf("body changed: %s", gotBody)
	}
	want := TokenUsage{InputTokens: 11, OutputTokens: 4, CachedTokens: 3, ReasoningTokens: 2, Present: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestOpenAINonStreamDropsUsageWhenAbsent(t *testing.T) {
	cases := map[string]string{
		"omitted":             `{"choices":[{"message":{"content":"ok"}}]}`,
		"null":                `{"choices":[],"usage":null}`,
		"array":               `{"choices":[],"usage":[]}`,
		"empty":               `{"choices":[],"usage":{}}`,
		"responses api names": `{"choices":[],"usage":{"input_tokens":9,"output_tokens":1}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			gotBody, err, got := collectUsage(t, p, false)
			if err != nil {
				t.Fatal(err)
			}
			if gotBody != body {
				t.Fatalf("body changed: %s", gotBody)
			}
			if len(got) != 0 {
				t.Fatalf("samples = %+v, want none", got)
			}
		})
	}
}

func TestOpenAINonStreamErrorDoesNotReportUsage(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":1}}`))
	}))
	_, err, got := collectUsage(t, p, false)
	if err == nil {
		t.Fatal("upstream error returned success")
	}
	if len(got) != 0 {
		t.Fatalf("samples = %+v, want none", got)
	}
}

func TestOpenAINonStreamExplicitZeroIsPresent(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	}))
	_, err, got := collectUsage(t, p, false)
	if err != nil {
		t.Fatal(err)
	}
	want := TokenUsage{Present: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestOpenAINonStreamAcceptsIntegerValuedFloats(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":11.0,"completion_tokens":4.0,"prompt_tokens_details":{"cached_tokens":3.0},"completion_tokens_details":{"reasoning_tokens":2.0}}}`))
	}))
	_, err, got := collectUsage(t, p, false)
	if err != nil {
		t.Fatal(err)
	}
	want := TokenUsage{InputTokens: 11, OutputTokens: 4, CachedTokens: 3, ReasoningTokens: 2, Present: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestOpenAINonStreamRejectsPartialUsage(t *testing.T) {
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":1.5}}`))
	}))
	gotBody, err, got := collectUsage(t, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"prompt_tokens":11`) {
		t.Fatalf("body = %s", gotBody)
	}
	if len(got) != 0 {
		t.Fatalf("samples = %+v, want none when a count is fractional", got)
	}
}

func TestOpenAINonStreamExactCapStillReports(t *testing.T) {
	pad := maxUsageScan - len(`{"usage":{"prompt_tokens":8,"completion_tokens":1},"pad":""}`)
	if pad < 0 {
		t.Fatal("fixture larger than the scan cap")
	}
	body := `{"usage":{"prompt_tokens":8,"completion_tokens":1},"pad":"` + strings.Repeat("x", pad) + `"}`
	if len(body) != maxUsageScan {
		t.Fatalf("fixture len = %d, want %d", len(body), maxUsageScan)
	}
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	gotBody, err, got := collectUsage(t, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != body {
		t.Fatalf("copied %d bytes, want %d", len(gotBody), len(body))
	}
	want := TokenUsage{InputTokens: 8, OutputTokens: 1, Present: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestOpenAINonStreamOverCapStillCopiesAndSkipsUsage(t *testing.T) {
	body := `{"usage":{"prompt_tokens":8,"completion_tokens":1},"pad":"` + strings.Repeat("x", maxUsageScan) + `"}`
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	gotBody, err, got := collectUsage(t, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != body {
		t.Fatalf("copied %d bytes, want %d", len(gotBody), len(body))
	}
	if len(got) != 0 {
		t.Fatalf("samples = %+v, want none above the scan cap", got)
	}
}

func TestOpenAIStreamReportsUsageAfterFinishChunk(t *testing.T) {
	const body = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":4,\"prompt_tokens_details\":{\"cached_tokens\":3},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n" +
		"data: [DONE]\n\n"
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	gotBody, err, got := collectUsage(t, p, true)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != body {
		t.Fatalf("proxied body = %q", gotBody)
	}
	want := TokenUsage{InputTokens: 11, OutputTokens: 4, CachedTokens: 3, ReasoningTokens: 2, Present: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestOpenAIStreamReportsUsageOnFinishChunk(t *testing.T) {
	const body = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\n"
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	_, err, got := collectUsage(t, p, true)
	if err != nil {
		t.Fatal(err)
	}
	want := TokenUsage{InputTokens: 5, OutputTokens: 1, Present: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestOpenAIStreamWithoutUsageReportsNothing(t *testing.T) {
	const body = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	gotBody, err, got := collectUsage(t, p, true)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != body {
		t.Fatalf("proxied body = %q", gotBody)
	}
	if len(got) != 0 {
		t.Fatalf("samples = %+v, want none", got)
	}
}

func TestOpenAIStreamIncompleteDropsUsage(t *testing.T) {
	const body = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2}}\n\n"
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	_, err, got := collectUsage(t, p, true)
	if err == nil {
		t.Fatal("incomplete stream returned success")
	}
	if len(got) != 0 {
		t.Fatalf("samples = %+v, want none", got)
	}
}

func TestOpenAIStreamLastUsageWins(t *testing.T) {
	const body = "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	_, err, got := collectUsage(t, p, true)
	if err != nil {
		t.Fatal(err)
	}
	want := TokenUsage{InputTokens: 9, OutputTokens: 3, Present: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestOpenAIStreamBadDataLineDoesNotInventUsage(t *testing.T) {
	const body = ": keepalive\n\n" +
		"data: not-json {\"usage\"}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	gotBody, err, got := collectUsage(t, p, true)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != body {
		t.Fatalf("proxied body = %q", gotBody)
	}
	if len(got) != 0 {
		t.Fatalf("samples = %+v, want none", got)
	}
}

func TestOpenAIStreamUsageOnUnterminatedLine(t *testing.T) {
	const body = "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}"
	p := newOpenAI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	gotBody, err, got := collectUsage(t, p, true)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != body {
		t.Fatalf("proxied body = %q", gotBody)
	}
	want := TokenUsage{InputTokens: 2, OutputTokens: 1, Present: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}
