package codex

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
)

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestParseIDTokenClaims(t *testing.T) {
	token := testJWT(t, map[string]any{
		"email": "user@example.com",
		"sub":   "subject-user",
		"organizations": []any{
			map[string]any{"id": "org-1", "title": "Acme"},
		},
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "workspace-1",
			"chatgpt_user_id":    "user-1",
			"chatgpt_plan_type":  "business",
		},
	})

	claims, err := ParseIDTokenClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Email != "user@example.com" || claims.AccountID != "workspace-1" || claims.UserID != "user-1" || claims.Plan != "business" {
		t.Fatalf("claims = %+v", claims)
	}
	wantOrganizations := []Organization{{ID: "org-1", Title: "Acme"}}
	if !reflect.DeepEqual(claims.Organizations, wantOrganizations) {
		t.Fatalf("organizations = %+v", claims.Organizations)
	}
}

func TestParseIDTokenClaimsFallsBackToProfileAndSubject(t *testing.T) {
	token := testJWT(t, map[string]any{
		"sub":                            "subject-user",
		"https://api.openai.com/profile": map[string]any{"email": "profile@example.com"},
	})
	claims, err := ParseIDTokenClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Email != "profile@example.com" || claims.UserID != "subject-user" {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestParseIDTokenClaimsRejectsInvalidJWT(t *testing.T) {
	if _, err := ParseIDTokenClaims("invalid"); err == nil {
		t.Fatal("expected invalid JWT error")
	}
}
