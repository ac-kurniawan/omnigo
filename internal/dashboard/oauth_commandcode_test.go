package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider/commandcode"
	"github.com/ac-kurniawan/omnigo/internal/vault"
)

func commandCodeTestServer(t *testing.T, validate func(ctx context.Context, apiBase, key string) (commandcode.WhoAmI, error)) (*Server, *vault.Store) {
	t.Helper()
	store := vault.NewMemoryStore(&vault.Vault{})
	cfg := &config.Config{Providers: []config.Provider{{Name: "cc", Type: "commandcode"}}}
	s := newServer(func() *config.Config { return cfg }, store, nil)
	s.commandcodeValidate = validate
	return s, store
}

func okValidate(commandcode.WhoAmI) func(context.Context, string, string) (commandcode.WhoAmI, error) {
	return func(_ context.Context, _, key string) (commandcode.WhoAmI, error) {
		if key != "user_testkey" {
			return commandcode.WhoAmI{}, errors.New("bad key")
		}
		return commandcode.WhoAmI{UserID: "user-1", UserName: "tester", Email: "tester@example.com", OrgID: "org-1", PlanID: "go"}, nil
	}
}

func extractCCState(t *testing.T, body string) string {
	t.Helper()
	u, err := url.Parse(extractHref(t, body))
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("studio link missing state: %s", u)
	}
	return state
}

func extractHref(t *testing.T, body string) string {
	t.Helper()
	idx := strings.Index(body, "https://commandcode.ai/studio/auth/cli?")
	if idx < 0 {
		t.Fatalf("studio link missing in body:\n%s", body)
	}
	end := strings.Index(body[idx:], `"`)
	if end < 0 {
		t.Fatal("unterminated href")
	}
	href := body[idx : idx+end]
	return strings.ReplaceAll(href, "&amp;", "&")
}

func TestCommandCodeLoginRendersStudioLink(t *testing.T) {
	s, _ := commandCodeTestServer(t, okValidate(commandcode.WhoAmI{}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/login/cc", nil)
	req.Host = "localhost:8080"
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "https://commandcode.ai/studio/auth/cli?") {
		t.Fatalf("studio link missing:\n%s", body)
	}
	state := extractCCState(t, body)
	if st, ok := s.getCommandCodePending(state); !ok || st.status != "waiting" {
		t.Fatalf("pending = %+v, ok=%v", st, ok)
	}
	if !strings.Contains(body, "/oauth/commandcode/pending?state=") {
		t.Fatal("pending poll URL missing")
	}
}

func TestCommandCodeCallbackFlow(t *testing.T) {
	s, store := commandCodeTestServer(t, okValidate(commandcode.WhoAmI{}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/login/cc", nil)
	req.Host = "localhost:8080"
	s.routes().ServeHTTP(rr, req)
	state := extractCCState(t, rr.Body.String())

	pendingRR := httptest.NewRecorder()
	s.routes().ServeHTTP(pendingRR, httptest.NewRequest("GET", "/oauth/commandcode/pending?state="+state, nil))
	var pending map[string]string
	if err := json.Unmarshal(pendingRR.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if pending["status"] != "waiting" {
		t.Fatalf("pending = %v", pending)
	}

	payload := `{"apiKey":"user_testkey","state":"` + state + `","userId":"user-1","userName":"tester"}`
	cbReq := httptest.NewRequest("POST", "/oauth/commandcode/callback", strings.NewReader(payload))
	cbRR := httptest.NewRecorder()
	s.routes().ServeHTTP(cbRR, cbReq)
	if cbRR.Code != http.StatusOK {
		t.Fatalf("callback status = %d, body %s", cbRR.Code, cbRR.Body.String())
	}
	var okBody map[string]bool
	if err := json.Unmarshal(cbRR.Body.Bytes(), &okBody); err != nil || !okBody["success"] {
		t.Fatalf("callback body = %s", cbRR.Body.String())
	}
	if origin := cbRR.Header().Get("Access-Control-Allow-Origin"); origin != "https://commandcode.ai" {
		t.Fatalf("CORS origin = %q", origin)
	}
	if strings.Contains(cbRR.Body.String(), "user_testkey") {
		t.Fatal("key leaked in response")
	}

	pendingRR2 := httptest.NewRecorder()
	s.routes().ServeHTTP(pendingRR2, httptest.NewRequest("GET", "/oauth/commandcode/pending?state="+state, nil))
	var pending2 map[string]string
	if err := json.Unmarshal(pendingRR2.Body.Bytes(), &pending2); err != nil {
		t.Fatal(err)
	}
	if pending2["status"] != "connected" {
		t.Fatalf("pending2 = %v", pending2)
	}

	accounts := store.Get().Accounts("cc")
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v", accounts)
	}
	sec := accounts[0]
	if sec.APIKey != "user_testkey" || sec.AccountID != "user-1" || sec.Email != "tester@example.com" || sec.ProjectID != "org-1" {
		t.Fatalf("secret = %+v", sec)
	}
}
func TestCommandCodeCallbackRejectsSecondKey(t *testing.T) {
	s, _ := commandCodeTestServer(t, okValidate(commandcode.WhoAmI{}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/login/cc", nil)
	req.Host = "localhost:8080"
	s.routes().ServeHTTP(rr, req)
	state := extractCCState(t, rr.Body.String())

	payload := `{"apiKey":"user_testkey","state":"` + state + `"}`
	first := httptest.NewRecorder()
	s.routes().ServeHTTP(first, httptest.NewRequest("POST", "/oauth/commandcode/callback", strings.NewReader(payload)))
	if first.Code != http.StatusOK {
		t.Fatalf("first callback status = %d", first.Code)
	}
	s.commandcodeValidate = func(context.Context, string, string) (commandcode.WhoAmI, error) {
		return commandcode.WhoAmI{UserID: "evil", UserName: "evil", Email: "evil@example.com"}, nil
	}
	second := httptest.NewRecorder()
	s.routes().ServeHTTP(second, httptest.NewRequest("POST", "/oauth/commandcode/callback", strings.NewReader(`{"apiKey":"user_other","state":"`+state+`"}`)))
	if second.Code != http.StatusConflict {
		t.Fatalf("second callback status = %d, want 409", second.Code)
	}
}

func TestCommandCodeCallbackBadState(t *testing.T) {
	s, store := commandCodeTestServer(t, okValidate(commandcode.WhoAmI{}))
	payload := `{"apiKey":"user_testkey","state":"nope"}`
	cbRR := httptest.NewRecorder()
	s.routes().ServeHTTP(cbRR, httptest.NewRequest("POST", "/oauth/commandcode/callback", strings.NewReader(payload)))
	if cbRR.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", cbRR.Code)
	}
	if len(store.Get().Accounts("cc")) != 0 {
		t.Fatal("must not persist on bad state")
	}
}

func TestCommandCodeCallbackWhoamiFail(t *testing.T) {
	s, store := commandCodeTestServer(t, func(_ context.Context, _, _ string) (commandcode.WhoAmI, error) {
		return commandcode.WhoAmI{}, errors.New("whoami down")
	})
	s.setCommandCodePending("st1", "cc", "waiting", "")
	payload := `{"apiKey":"user_testkey","state":"st1"}`
	cbRR := httptest.NewRecorder()
	s.routes().ServeHTTP(cbRR, httptest.NewRequest("POST", "/oauth/commandcode/callback", strings.NewReader(payload)))
	if cbRR.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s", cbRR.Code, cbRR.Body.String())
	}
	if strings.Contains(cbRR.Body.String(), "user_testkey") {
		t.Fatal("key leaked in error response")
	}
	if len(store.Get().Accounts("cc")) != 0 {
		t.Fatal("must not persist when whoami fails")
	}
	pending, ok := s.getCommandCodePending("st1")
	if !ok || pending.status != "failed" {
		t.Fatalf("pending = %+v ok=%v", pending, ok)
	}
}

func TestCommandCodePasteKey(t *testing.T) {
	s, store := commandCodeTestServer(t, okValidate(commandcode.WhoAmI{}))
	form := url.Values{"callback": {"user_testkey"}}
	req := httptest.NewRequest("POST", "/oauth/cc/paste-callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "CommandCode Connected") {
		t.Fatalf("body = %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "user_testkey") {
		t.Fatal("key leaked in success fragment")
	}
	accounts := store.Get().Accounts("cc")
	if len(accounts) != 1 || accounts[0].APIKey != "user_testkey" {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestCommandCodeOptionsCORS(t *testing.T) {
	s, _ := commandCodeTestServer(t, okValidate(commandcode.WhoAmI{}))
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest("OPTIONS", "/oauth/commandcode/callback", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rr.Code)
	}
	if origin := rr.Header().Get("Access-Control-Allow-Origin"); origin != "https://commandcode.ai" {
		t.Fatalf("origin = %q", origin)
	}
	if allow := rr.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(allow, "POST") {
		t.Fatalf("allow = %q", allow)
	}
}
