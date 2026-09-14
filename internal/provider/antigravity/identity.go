package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

var userinfoURL = "https://openidconnect.googleapis.com/v1/userinfo"

type Identity struct {
	AccountID string
	Email     string
}

func DiscoverIdentity(ctx context.Context, accessToken string) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoURL, nil)
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := unaryClient.Do(req)
	if err != nil {
		return Identity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("userinfo: status %d", resp.StatusCode)
	}
	var out struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Identity{}, err
	}
	if out.Sub == "" {
		return Identity{}, fmt.Errorf("userinfo: missing subject")
	}
	return Identity{AccountID: out.Sub, Email: out.Email}, nil
}
