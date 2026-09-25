package provider

import (
	"context"
	"testing"
)

func TestReportUsageDeliversOnlyPresentSamples(t *testing.T) {
	var got []TokenUsage
	ctx := WithUsageSink(context.Background(), func(usage TokenUsage) {
		got = append(got, usage)
	})

	ReportUsage(ctx, TokenUsage{InputTokens: 10, OutputTokens: 4, Present: true})
	ReportUsage(ctx, TokenUsage{InputTokens: 9})
	ReportUsage(context.Background(), TokenUsage{InputTokens: 1, Present: true})
	ReportUsage(nil, TokenUsage{InputTokens: 1, Present: true})

	if len(got) != 1 {
		t.Fatalf("samples = %+v, want the one present sample", got)
	}
	if got[0].InputTokens != 10 || got[0].OutputTokens != 4 {
		t.Fatalf("sample = %+v", got[0])
	}
}

func TestWithUsageSinkIgnoresNilReporter(t *testing.T) {
	ctx := WithUsageSink(context.Background(), nil)
	ReportUsage(ctx, TokenUsage{InputTokens: 1, Present: true})
}
