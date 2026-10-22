package observability

import (
	"context"
	"strings"
	"testing"
)

// A smart combo's decision is counted with the route it chose, so an
// operator can see which destination each prompt type ends up on, and how
// often the default route had to step in.
func TestRecordSmartDecision(t *testing.T) {
	m, err := New(func() bool { return true }, "test-version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Shutdown(context.Background()) }()

	m.RecordSmartDecision(context.Background(), "combo/smart", "flash", "success")
	m.RecordSmartDecision(context.Background(), "combo/smart", "flash", "fallback")

	body := scrape(t, m)
	for _, want := range []string{
		"omnigo_smart_decisions_total",
		`omnigo_combo_name="combo/smart"`,
		`omnigo_smart_route="flash"`,
		`result="success"`,
		`result="fallback"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n---\n%s", want, body)
		}
	}
}
