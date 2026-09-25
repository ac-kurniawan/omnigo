package observability

import (
	"context"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/auth"
	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func TestRecordTokenUsageExposesKindSeries(t *testing.T) {
	m, err := New(func() bool { return true }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	caller := &auth.Caller{}
	caller.Set(vault.ClientKey{ID: "a1b2c3d4e5f60718"})
	ctx := auth.WithCaller(context.Background(), caller)

	m.RecordTokenUsage(ctx, "codex", "gpt-5.3-codex", "acc-1", "auto", provider.TokenUsage{
		InputTokens:              10,
		OutputTokens:             7,
		CachedTokens:             3,
		CacheCreationInputTokens: 1,
		ReasoningTokens:          2,
		Present:                  true,
	})
	m.RecordTokenUsage(ctx, "codex", "gpt-5.3-codex", "acc-1", "auto", provider.TokenUsage{InputTokens: 4})

	body := scrape(t, m)
	for _, want := range []string{
		"omnigo_tokens_total",
		`gen_ai_system="codex"`,
		`gen_ai_request_model="gpt-5.3-codex"`,
		`account="acc-1"`,
		`api_key_id="a1b2c3d4e5f60718"`,
		`omnigo_combo_name="auto"`,
		`omnigo_token_kind="input"`,
		`omnigo_token_kind="output"`,
		`omnigo_token_kind="cached"`,
		`omnigo_token_kind="cache_creation"`,
		`omnigo_token_kind="reasoning"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, body)
		}
	}
	if strings.Count(body, `omnigo_token_kind="input"`) != 1 {
		t.Errorf("absent usage must not add a series\n%s", body)
	}
	for _, want := range []struct {
		kind  string
		value string
	}{
		{kind: "input", value: "10"},
		{kind: "output", value: "7"},
		{kind: "cached", value: "3"},
		{kind: "cache_creation", value: "1"},
		{kind: "reasoning", value: "2"},
	} {
		line := ""
		for _, row := range strings.Split(body, "\n") {
			if strings.HasPrefix(row, "omnigo_tokens_total{") && strings.Contains(row, `omnigo_token_kind="`+want.kind+`"`) {
				line = row
				break
			}
		}
		if line == "" || !strings.HasSuffix(line, " "+want.value) {
			t.Errorf("kind %s = %q, want value %s", want.kind, line, want.value)
		}
	}
}

func TestRecordTokenUsageDisabledRecordsNothing(t *testing.T) {
	m := Disabled()
	m.RecordTokenUsage(context.Background(), "codex", "gpt", "acc", "", provider.TokenUsage{
		InputTokens: 10, OutputTokens: 2, Present: true,
	})
}

func TestRecordTokenUsageDirectCallUsesNoneCombo(t *testing.T) {
	m, err := New(func() bool { return true }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	m.RecordTokenUsage(context.Background(), "codex", "gpt", "user@example.com", "", provider.TokenUsage{
		InputTokens: 1, Present: true,
	})
	body := scrape(t, m)
	if !strings.Contains(body, `omnigo_combo_name="none"`) {
		t.Fatalf("direct call did not collapse the combo label\n%s", body)
	}
	if strings.Contains(body, "@") {
		t.Fatalf("account email kept a raw @ in a label\n%s", body)
	}
	if !strings.Contains(body, `account="user_at_example.com"`) {
		t.Fatalf("account label was not normalized\n%s", body)
	}
}
