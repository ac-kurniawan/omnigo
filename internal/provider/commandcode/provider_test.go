package commandcode

import (
	"context"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestRegistered(t *testing.T) {
	if _, ok := provider.Get("commandcode"); !ok {
		t.Fatal("commandcode factory not registered")
	}
}

func TestNewDefaults(t *testing.T) {
	p := New(provider.Config{Name: "cc"}, nil).(*Provider)
	if p.Name() != "cc" {
		t.Fatalf("Name = %q", p.Name())
	}
	if p.generateURL != DefaultGenerateURL {
		t.Fatalf("generateURL = %q", p.generateURL)
	}
	if p.apiBase != DefaultBaseURL {
		t.Fatalf("apiBase = %q", p.apiBase)
	}
	if p.modelsURL != DefaultModelsURL {
		t.Fatalf("modelsURL = %q", p.modelsURL)
	}
}

func TestModelsFromConfig(t *testing.T) {
	p := New(provider.Config{Name: "cc", Models: []string{"m1"}}, nil).(*Provider)
	p.modelsURL = "http://127.0.0.1:1/unreachable"
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 || models[0].ID != "m1" {
		t.Fatalf("models = %#v", models)
	}
}
