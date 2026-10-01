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
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatal(err)
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
