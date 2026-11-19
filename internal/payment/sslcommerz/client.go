// Package sslcommerz is a small client for SSLCommerz's hosted checkout:
// create a payment session, then validate the result server-to-server.
package sslcommerz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	sandboxBase = "https://sandbox.sslcommerz.com"
	liveBase    = "https://securepay.sslcommerz.com"
	maxBody     = 1 << 20
)

var ErrNotConfigured = errors.New("sslcommerz: store id / password not configured")

type Config struct {
	StoreID       string
	StorePassword string
	Sandbox       bool
	BaseURL       string // optional override (e.g. for tests)
}

type Client struct {
	cfg  Config
	base string
	http *http.Client
}

func New(cfg Config) *Client {
	base := liveBase
	if cfg.Sandbox {
		base = sandboxBase
	}
	if cfg.BaseURL != "" {
		base = strings.TrimRight(cfg.BaseURL, "/")
	}
	return &Client{
		cfg:  cfg,
		base: base,
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *Client) Configured() bool {
	return c.cfg.StoreID != "" && c.cfg.StorePassword != ""
}

/* ------------------------------------------------------------------ */
/* Init                                                                */
/* ------------------------------------------------------------------ */

type InitInput struct {
	TranID        string
	Amount        int64 // whole taka
	SuccessURL    string
	FailURL       string
	CancelURL     string
	IPNURL        string
	ProductName   string
	CustomerName  string
	CustomerEmail string
	CustomerPhone string
}

// Init creates a payment session and returns the hosted payment page URL.
func (c *Client) Init(ctx context.Context, in InitInput) (string, error) {
	if !c.Configured() {
		return "", ErrNotConfigured
	}
	phone := in.CustomerPhone
	if phone == "" {
		phone = "01700000000" // the gateway requires a phone; we don't collect one
	}

	form := url.Values{}
	form.Set("store_id", c.cfg.StoreID)
	form.Set("store_passwd", c.cfg.StorePassword)
	form.Set("total_amount", strconv.FormatInt(in.Amount, 10)+".00")
	form.Set("currency", "BDT")
	form.Set("tran_id", in.TranID)
	form.Set("success_url", in.SuccessURL)
	form.Set("fail_url", in.FailURL)
	form.Set("cancel_url", in.CancelURL)
	form.Set("ipn_url", in.IPNURL)
	form.Set("cus_name", in.CustomerName)
	form.Set("cus_email", in.CustomerEmail)
	form.Set("cus_add1", "Dhaka")
	form.Set("cus_city", "Dhaka")
	form.Set("cus_country", "Bangladesh")
	form.Set("cus_phone", phone)
	form.Set("shipping_method", "NO")
	form.Set("product_name", in.ProductName)
	form.Set("product_category", "Subscription")
	form.Set("product_profile", "non-physical-goods")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/gwprocess/v4/api.php", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	body, err := c.do(req)
	if err != nil {
		return "", err
	}

	var out struct {
		Status         string `json:"status"`
		FailedReason   string `json:"failedreason"`
		GatewayPageURL string `json:"GatewayPageURL"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("sslcommerz: bad init response: %w", err)
	}
	if !strings.EqualFold(out.Status, "SUCCESS") || !strings.HasPrefix(out.GatewayPageURL, "https://") {
		return "", fmt.Errorf("sslcommerz: init rejected: %s", out.FailedReason)
	}
	return out.GatewayPageURL, nil
}

/* ------------------------------------------------------------------ */
/* Validate                                                            */
/* ------------------------------------------------------------------ */

// Flex accepts a JSON string or number (the gateway uses both).
type Flex string

func (f *Flex) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*f = ""
		return nil
	}
	if len(s) > 0 && s[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = Flex(v)
		return nil
	}
	*f = Flex(s)
	return nil
}

type Validation struct {
	Status       string `json:"status"`
	TranID       string `json:"tran_id"`
	ValID        string `json:"val_id"`
	Amount       Flex   `json:"amount"`
	Currency     string `json:"currency"`
	CurrencyType string `json:"currency_type"`
	RiskLevel    Flex   `json:"risk_level"`
	BankTranID   string `json:"bank_tran_id"`
}

func (v *Validation) OK() bool {
	s := strings.ToUpper(v.Status)
	return s == "VALID" || s == "VALIDATED"
}

func (v *Validation) AmountTaka() (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(string(v.Amount)), 64)
}

// Risky reports the gateway's risk flag. Check the exact meaning in the docs;
// we treat "1" as "hold for review" and do not activate automatically.
func (v *Validation) Risky() bool {
	return strings.TrimSpace(string(v.RiskLevel)) == "1"
}

// Validate asks the gateway whether valID is a real, paid transaction.
func (c *Client) Validate(ctx context.Context, valID string) (*Validation, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(valID) == "" {
		return nil, errors.New("sslcommerz: val_id is required")
	}

	q := url.Values{}
	q.Set("val_id", valID)
	q.Set("store_id", c.cfg.StoreID)
	q.Set("store_passwd", c.cfg.StorePassword)
	q.Set("v", "1")
	q.Set("format", "json")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/validator/api/validationserverAPI.php?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}

	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var v Validation
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("sslcommerz: bad validation response: %w", err)
	}
	return &v, nil
}

/* ------------------------------------------------------------------ */

func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error embeds the full URL, which for Validate contains the
		// store password. Unwrap it so the secret never reaches the logs.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("sslcommerz: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("sslcommerz: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sslcommerz: unexpected status %d", resp.StatusCode)
	}
	return body, nil
}
