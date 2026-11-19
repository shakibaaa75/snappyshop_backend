package courier

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	SteadfastID = "steadfast"
	// Base URL from the Steadfast Courier API Documentation V1.
	steadfastBaseURL = "https://portal.packzy.com/api/v1"
	maxBody          = 64 << 10
)

// Steadfast implements Provider for Steadfast Courier Limited.
//
// Per the documentation, every request carries Api-Key and Secret-Key
// headers. The document has no dedicated "validate credentials" endpoint, so
// Verify calls GET /get_balance, an authenticated, read-only call whose
// documented success body is {"status":200,"current_balance":<n>}.
//
// The document does not say what a wrong key returns, so Verify only reports
// success on that exact documented shape, treats HTTP 401/403 (or a body
// status of 401/403) as rejection, and treats everything else as
// "unavailable" rather than guessing. Confirm the rejection behaviour once
// with a real account.
type Steadfast struct {
	baseURL string
	client  *http.Client
}

// NewSteadfast builds the adapter. baseURL "" uses the documented production
// URL; tests pass an httptest server URL.
func NewSteadfast(baseURL string, client *http.Client) *Steadfast {
	if baseURL == "" {
		baseURL = steadfastBaseURL
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	c := *client
	// Never follow redirects: they could carry the key headers elsewhere.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Steadfast{baseURL: strings.TrimRight(baseURL, "/"), client: &c}
}

func (s *Steadfast) ID() string   { return SteadfastID }
func (s *Steadfast) Name() string { return "Steadfast Courier" }

func (s *Steadfast) Fields() []CredentialField {
	return []CredentialField{
		{Key: "apiKey", Label: "API Key", Type: FieldPassword, Placeholder: "Paste your Steadfast API key", Required: true},
		{Key: "secretKey", Label: "Secret Key", Type: FieldPassword, Placeholder: "Paste your Steadfast secret key", Required: true},
	}
}

func (s *Steadfast) Help() []string {
	return []string{
		"Steadfast Courier issues an API Key and a Secret Key for your merchant account.",
		"Find them in your Steadfast merchant account's API / integration section, or ask Steadfast support to enable API access.",
		"Paste both values here. We check them with Steadfast before saving.",
	}
}

func (s *Steadfast) DocsURL() string { return "" }

func (s *Steadfast) Identity(c map[string]string) string { return c["apiKey"] }
func (s *Steadfast) Hint(c map[string]string) string     { return maskTail(c["apiKey"]) }

type balanceResponse struct {
	Status         json.Number  `json:"status"`
	CurrentBalance *json.Number `json:"current_balance"`
}

func (s *Steadfast) Verify(ctx context.Context, c map[string]string) error {
	key, secret := c["apiKey"], c["secretKey"]
	// Reject anything that could break out of a header value.
	if key == "" || secret == "" || strings.ContainsAny(key+secret, "\r\n\x00") {
		return ErrInvalidCredentials
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/get_balance", nil)
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Api-Key", key)
	req.Header.Set("Secret-Key", secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		// err can embed the URL but never header values; still, do not wrap it.
		return ErrUnavailable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrInvalidCredentials
	case resp.StatusCode != http.StatusOK:
		return ErrUnavailable
	}

	var out balanceResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return ErrUnavailable
	}
	switch out.Status.String() {
	case "401", "403":
		return ErrInvalidCredentials
	case "200":
		if out.CurrentBalance != nil {
			return nil
		}
	}
	return errors.Join(ErrUnavailable) // unrecognised shape: do not claim success
}
