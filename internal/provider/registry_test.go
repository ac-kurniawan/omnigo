package provider

import (
	"context"
	"net/http"
	"testing"
)

type fake struct{}

func (f fake) Name() string { return "fake" }
func (f fake) ChatCompletion(ctx context.Context, req ChatRequest, w http.ResponseWriter) error {
	return nil
}
func (f fake) Models(ctx context.Context) ([]Model, error) { return nil, nil }
func (f fake) Test(ctx context.Context) TestResult         { return TestResult{} }

func TestRegisterAndGet(t *testing.T) {
	Register("openai", func(cfg Config, store CredStore) Provider { return fake{} })
	f, ok := Get("openai")
	if !ok {
		t.Fatal("expected openai factory to be registered")
	}
	p := f(Config{Name: "openai"}, nil)
	if p.Name() != "fake" {
		t.Fatalf("name = %q", p.Name())
	}
}

func TestGetUnknown(t *testing.T) {
	if _, ok := Get("nope"); ok {
		t.Fatal("expected unknown type to be absent")
	}
}
