package codebuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
	"github.com/ac-kurniawan/omnigo/internal/quota"
)

var _ provider.QuotaFetcher = (*codebuddyProvider)(nil)

type userResourceResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Response struct {
			Data struct {
				TotalCount  int `json:"TotalCount"`
				TotalDosage int `json:"TotalDosage"`
				Accounts    []struct {
					AccountId           int64   `json:"AccountId"`
					PackageName         string  `json:"PackageName"`
					CapacityRemain      float64 `json:"CapacityRemain"`
					CapacitySize        float64 `json:"CapacitySize"`
					CycleStartTime      string  `json:"CycleStartTime"`
					CycleEndTime        string  `json:"CycleEndTime"`
					CycleCapacityRemain float64 `json:"CycleCapacityRemain"`
					CycleCapacitySize   float64 `json:"CycleCapacitySize"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	} `json:"data"`
}

func (p *codebuddyProvider) FetchQuota(ctx context.Context, account provider.Credentials) (quota.AccountSnapshot, error) {
	snap := quota.AccountSnapshot{
		Provider:   p.name,
		Identity:   account.Identity(),
		AccountID:  account.AccountID,
		Email:      account.Email,
		ObservedAt: time.Now(),
	}

	key := account.APIKey
	if key == "" {
		key = account.AccessToken
	}
	if key == "" {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "no api key or token configured"
		return snap, fmt.Errorf("codebuddy: no credentials")
	}

	endpoint := p.baseURL + "/billing/meter/get-user-resource"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "create request failed"
		return snap, err
	}
	p.buildHeaders(httpReq, key, false)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "upstream request failed"
		return snap, fmt.Errorf("codebuddy: fetch quota: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snap.Status = quota.StatusUnavailable
		snap.Reason = fmt.Sprintf("upstream status %d", resp.StatusCode)
		return snap, fmt.Errorf("codebuddy: quota upstream status %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "read response failed"
		return snap, err
	}

	var parsed userResourceResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		snap.Status = quota.StatusUnavailable
		snap.Reason = "decode response failed"
		return snap, err
	}

	if parsed.Code != 0 {
		snap.Status = quota.StatusUnavailable
		snap.Reason = parsed.Msg
		return snap, fmt.Errorf("codebuddy: quota code %d: %s", parsed.Code, parsed.Msg)
	}

	accounts := parsed.Data.Response.Data.Accounts
	if len(accounts) == 0 {
		snap.Status = quota.StatusAvailable
		snap.Windows = []quota.Window{{
			Name:        "credits",
			UsedPercent: 0,
		}}
		return snap, nil
	}

	var totalCapacity float64
	var totalRemain float64
	var minResetAt time.Time

	windows := make([]quota.Window, 0, len(accounts))

	for _, acc := range accounts {
		pkgName := acc.PackageName
		if pkgName == "" {
			pkgName = "Package " + strconv.FormatInt(acc.AccountId, 10)
		}

		capSize := acc.CapacitySize
		capRemain := acc.CapacityRemain
		if capSize <= 0 {
			capSize = 100
		}
		if capRemain < 0 {
			capRemain = 0
		}
		if capRemain > capSize {
			capRemain = capSize
		}

		totalCapacity += capSize
		totalRemain += capRemain

		remPct := (capRemain / capSize) * 100
		usedPct := math.Round((100-remPct)*10) / 10

		var resetAt time.Time
		if acc.CycleEndTime != "" {
			if t, err := time.Parse("2006-01-02 15:04:05", acc.CycleEndTime); err == nil {
				resetAt = t
				if minResetAt.IsZero() || t.Before(minResetAt) {
					minResetAt = t
				}
			}
		}

		windows = append(windows, quota.Window{
			Name:        pkgName,
			UsedPercent: usedPct,
			ResetAt:     resetAt,
		})
	}

	if totalCapacity <= 0 {
		totalCapacity = 100
	}
	totalRemPct := (totalRemain / totalCapacity) * 100
	if totalRemPct < 0 {
		totalRemPct = 0
	}
	if totalRemPct > 100 {
		totalRemPct = 100
	}
	totalUsedPct := math.Round((100-totalRemPct)*10) / 10

	group := quota.Group{
		Name: fmt.Sprintf("CodeBuddy Credits (%.0f/%.0f credits)", totalRemain, totalCapacity),
		Windows: append([]quota.Window{{
			Name:        "Total Credits",
			UsedPercent: totalUsedPct,
			ResetAt:     minResetAt,
		}}, windows...),
	}

	snap.Groups = []quota.Group{group}
	snap.Windows = group.Windows

	if totalRemain <= 0 {
		snap.Status = quota.StatusExhausted
		snap.Reason = "quota exhausted"
	} else {
		snap.Status = quota.StatusAvailable
	}

	return snap, nil
}
