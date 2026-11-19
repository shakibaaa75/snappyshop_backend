package fraud

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	FraudBDID      = "fraudbd"
	fraudBDBaseURL = "https://fraudbd.com"
	maxBody        = 256 << 10
)

// FraudBD implements Provider for https://fraudbd.com (POST
// /api/check-courier-info, header api_key).
type FraudBD struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewFraudBD builds the client. baseURL "" = production. Production must be
// https; tests pass an httptest URL.
func NewFraudBD(baseURL, apiKey string, client *http.Client) *FraudBD {
	if baseURL == "" {
		baseURL = fraudBDBaseURL
	}
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	c := *client
	// Never follow redirects: they could carry the api_key header elsewhere.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &FraudBD{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, client: &c}
}

func (f *FraudBD) ID() string { return FraudBDID }

type fbdLine struct {
	DataType       string  `json:"data_type"`
	CustomerRating string  `json:"customer_rating"`
	RiskLevel      string  `json:"risk_level"`
	Total          int     `json:"total"`
	Success        int     `json:"success"`
	Cancel         int     `json:"cancel"`
	SuccessRate    float64 `json:"success_rate"`
}

type fbdResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    *struct {
		Summaries    map[string]fbdLine `json:"Summaries"`
		TotalSummary struct {
			Total   int `json:"total"`
			Success int `json:"success"`
			Cancel  int `json:"cancel"`
		} `json:"totalSummary"`
	} `json:"data"`
}

func (f *FraudBD) Check(ctx context.Context, phone string) (*ProviderResult, error) {
	body, _ := json.Marshal(map[string]string{"phone_number": phone})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+"/api/check-courier-info", bytes.NewReader(body))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api_key", f.apiKey)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))

	var out fbdResponse
	_ = json.Unmarshal(raw, &out)
	msg := strings.ToLower(out.Message)

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 403 also covers "no active plan"; both mean our key cannot be used.
		return nil, ErrRejected
	case resp.StatusCode == http.StatusTooManyRequests, strings.Contains(msg, "usage limit"):
		return nil, ErrQuota
	case resp.StatusCode >= 300:
		return nil, ErrUnavailable
	case !out.Status || out.Data == nil:
		return nil, ErrUnavailable
	}

	res := &ProviderResult{Provider: FraudBDID}
	names := make([]string, 0, len(out.Data.Summaries))
	for n := range out.Data.Summaries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		l := out.Data.Summaries[n]
		line := CourierLine{Name: n, Total: l.Total, Success: l.Success, Cancel: l.Cancel}
		if l.DataType == "rating" {
			line.Rating, line.RiskLevel = l.CustomerRating, l.RiskLevel
			res.Rating, res.RiskLevel = l.CustomerRating, l.RiskLevel
		}
		res.Couriers = append(res.Couriers, line)
	}
	// totalSummary already excludes rating-based data (per the docs).
	ts := out.Data.TotalSummary
	res.Total, res.Success, res.Cancel = ts.Total, ts.Success, ts.Cancel
	return res, nil
}
