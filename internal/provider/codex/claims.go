package codex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

type Organization struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type Claims struct {
	Email         string
	AccountID     string
	UserID        string
	Plan          string
	Organizations []Organization
}

func ParseIDTokenClaims(idToken string) (Claims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Claims{}, fmt.Errorf("invalid ID token format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("decode ID token payload: %w", err)
	}
	var raw struct {
		Email         string         `json:"email"`
		Subject       string         `json:"sub"`
		Organizations []Organization `json:"organizations"`
		Profile       struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			UserID    string `json:"chatgpt_user_id"`
			AltUserID string `json:"user_id"`
			Plan      string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Claims{}, fmt.Errorf("decode ID token claims: %w", err)
	}
	claims := Claims{
		Email:         raw.Email,
		AccountID:     raw.Auth.AccountID,
		UserID:        raw.Auth.UserID,
		Plan:          raw.Auth.Plan,
		Organizations: raw.Organizations,
	}
	if claims.Email == "" {
		claims.Email = raw.Profile.Email
	}
	if claims.UserID == "" {
		claims.UserID = raw.Auth.AltUserID
	}
	if claims.UserID == "" {
		claims.UserID = raw.Subject
	}
	return claims, nil
}
