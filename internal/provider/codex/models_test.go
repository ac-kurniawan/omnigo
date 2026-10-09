package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestModelsRecordsCatalogProfiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-lite","visibility":"list","supported_in_api":true,"use_responses_lite":true,"default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"}]}]}`)
	}))
	defer server.Close()
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex"}, store).(*Provider)
	p.modelsURL = server.URL
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var listed provider.Model
	for _, model := range models {
		if model.ID == "gpt-lite" {
			listed = model
		}
	}
	if listed.Capabilities == nil || !listed.Capabilities.Reasoning || listed.Capabilities.DefaultEffort != "medium" || !reflect.DeepEqual(listed.Capabilities.ReasoningEfforts, []string{"low", "medium"}) {
		t.Fatalf("capabilities = %+v", listed.Capabilities)
	}
	profile, ok := p.profile("gpt-lite")
	if !ok {
		t.Fatal(`profile("gpt-lite") missing after Models()`)
	}
	want := modelProfile{Lite: true, Levels: []string{"low", "medium"}, Default: "medium"}
	if !reflect.DeepEqual(profile, want) {
		t.Fatalf("profile = %+v, want %+v", profile, want)
	}
}

func TestProfileFallsBackToBuiltinWhenCatalogUnavailable(t *testing.T) {
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex"}, store).(*Provider)
	profile, ok := p.profile("gpt-6-sol")
	if !ok || !profile.Lite {
		t.Fatalf("profile(gpt-6-sol) = %+v, %v; want lite", profile, ok)
	}
	profile, ok = p.profile("gpt-5.5")
	if !ok || profile.Lite {
		t.Fatalf("profile(gpt-5.5) = %+v, %v; want legacy", profile, ok)
	}
	if _, ok = p.profile("gpt-unknown"); ok {
		t.Fatal("profile(gpt-unknown) unexpectedly resolved")
	}
}

func TestModelsReplacesProfilesOnRefresh(t *testing.T) {
	catalogs := []string{
		`{"models":[{"slug":"gpt-x","visibility":"list","supported_in_api":true,"use_responses_lite":true}]}`,
		`{"models":[{"slug":"gpt-x","visibility":"list","supported_in_api":true}]}`,
	}
	var fetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, catalogs[fetches])
		fetches++
	}))
	defer server.Close()
	store := &memoryCredStore{creds: provider.Credentials{AccessToken: "access", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}}
	p := New(provider.Config{Name: "codex"}, store).(*Provider)
	p.modelsURL = server.URL
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if profile, ok := p.profile("gpt-x"); !ok || !profile.Lite {
		t.Fatalf("first fetch profile(gpt-x) = %+v, %v; want lite", profile, ok)
	}
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	profile, ok := p.profile("gpt-x")
	if !ok || profile.Lite {
		t.Fatalf("second fetch profile(gpt-x) = %+v, %v; want legacy", profile, ok)
	}
}

func TestFallbackModelCapabilities(t *testing.T) {
	var gpt55, astra, sol61, gpt52 provider.Model
	for _, model := range fallbackModels() {
		switch model.ID {
		case "gpt-5.5":
			gpt55 = model
		case "gpt-6-astra":
			astra = model
		case "gpt-6.1-sol":
			sol61 = model
		case "gpt-5.2":
			gpt52 = model
		}
	}
	if gpt55.Capabilities == nil || !reflect.DeepEqual(gpt55.Capabilities.ReasoningEfforts, []string{"low", "medium", "high", "xhigh"}) || gpt55.Capabilities.DefaultEffort != "medium" {
		t.Fatalf("gpt-5.5 capabilities = %+v", gpt55.Capabilities)
	}
	if astra.Capabilities == nil || !reflect.DeepEqual(astra.Capabilities.ReasoningEfforts, []string{"low", "medium", "high", "xhigh", "max", "ultra"}) || astra.Capabilities.DefaultEffort != "low" {
		t.Fatalf("gpt-6-astra capabilities = %+v", astra.Capabilities)
	}
	if sol61.Capabilities == nil || !reflect.DeepEqual(sol61.Capabilities.ReasoningEfforts, []string{"low", "medium", "high", "xhigh", "max"}) || sol61.Capabilities.DefaultEffort != "medium" {
		t.Fatalf("gpt-6.1-sol capabilities = %+v", sol61.Capabilities)
	}
	if gpt52.Capabilities != nil {
		t.Fatalf("gpt-5.2 capabilities = %+v, want nil", gpt52.Capabilities)
	}
}

func TestKnownProfile(t *testing.T) {
	profile, ok := KnownProfile("gpt-5.5")
	if !ok || profile.Default != "medium" || !reflect.DeepEqual(profile.Levels, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("KnownProfile(gpt-5.5) = %+v, %v", profile, ok)
	}
	profile61, ok := KnownProfile("gpt-6.1-sol")
	if !ok || !profile61.Lite || profile61.Default != "medium" || !reflect.DeepEqual(profile61.Levels, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("KnownProfile(gpt-6.1-sol) = %+v, %v", profile61, ok)
	}
	if _, ok := KnownProfile("gpt-5.2"); ok {
		t.Fatal("KnownProfile(gpt-5.2) unexpectedly hit")
	}
}
