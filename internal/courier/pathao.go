package courier

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	PathaoID = "pathao"
	// Production base URL from the official Pathao Merchant API docs.
	// Sandbox (test credentials only): https://courier-api-sandbox.pathao.com,
	// selectable with PATHAO_BASE_URL. Live and sandbox accounts, tokens and
	// store IDs are completely separate.
	pathaoBaseURL = "https://api-hermes.pathao.com"
)

// Pathao implements Provider and Shipper for Pathao Courier (Merchant API).
//
// Auth is OAuth2 password grant: POST /aladdin/api/v1/issue-token with
// client_id, client_secret, username, password and grant_type=password
// returns an access_token used as "Authorization: Bearer <token>".
// Tokens are requested per call and never cached, so one seller's token can
// never be reused for another seller.
//
// Endpoints and fields follow the official Pathao Courier Merchant API docs.
type Pathao struct {
	baseURL string
	client  *http.Client
}

// NewPathao builds the adapter. baseURL "" uses the production URL; tests
// pass an httptest server URL.
func NewPathao(baseURL string, client *http.Client) *Pathao {
	if baseURL == "" {
		baseURL = pathaoBaseURL
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	c := *client
	// Never follow redirects: they could carry credentials elsewhere.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Pathao{baseURL: strings.TrimRight(baseURL, "/"), client: &c}
}

func (p *Pathao) ID() string   { return PathaoID }
func (p *Pathao) Name() string { return "Pathao Courier" }

func (p *Pathao) Fields() []CredentialField {
	return []CredentialField{
		{Key: "clientId", Label: "Client ID", Type: FieldText, Placeholder: "Pathao client ID", Required: true},
		{Key: "clientSecret", Label: "Client Secret", Type: FieldPassword, Placeholder: "Pathao client secret", Required: true},
		{Key: "username", Label: "Merchant Email", Type: FieldText, Placeholder: "Email you log in to Pathao with", Required: true},
		{Key: "password", Label: "Merchant Password", Type: FieldPassword, Placeholder: "Your Pathao merchant password", Required: true},
		{Key: "storeId", Label: "Store ID (optional)", Type: FieldText, Placeholder: "Leave empty to use your default store", HelpText: "Numeric ID of the pickup store. If you have several stores, enter the one parcels should be collected from.", Required: false},
	}
}

func (p *Pathao) Help() []string {
	return []string{
		"Log in to your Pathao merchant panel and open the Developer API section to get your Client ID and Client Secret.",
		"The email and password are the ones you use to log in to the Pathao merchant panel.",
		"Store ID is optional: leave it empty to use your default Pathao store, or enter the ID of a specific pickup store.",
		"Use live credentials with live Pathao, not sandbox/test credentials.",
		"We check the login with Pathao before saving.",
	}
}

func (p *Pathao) DocsURL() string { return "" }

func (p *Pathao) Identity(c map[string]string) string { return c["clientId"] + "|" + c["username"] }
func (p *Pathao) Hint(c map[string]string) string {
	u := c["username"]
	at := strings.LastIndex(u, "@")
	if at < 1 {
		return maskTail(u)
	}
	return u[:1] + "••••" + u[at:] // a••••@gmail.com
}

type pathaoTokenResponse struct {
	AccessToken string `json:"access_token"`
}

func pathaoCredsOK(c map[string]string, keys ...string) bool {
	for _, k := range keys {
		v := c[k]
		if v == "" || strings.ContainsAny(v, "\r\n\x00") {
			return false
		}
	}
	return true
}

// token requests an access token. It returns ErrInvalidCredentials only when
// Pathao positively rejects the login (HTTP 400/401/403).
func (p *Pathao) token(ctx context.Context, c map[string]string) (string, error) {
	if !pathaoCredsOK(c, "clientId", "clientSecret", "username", "password") {
		return "", ErrInvalidCredentials
	}
	payload, err := json.Marshal(map[string]string{
		"client_id":     c["clientId"],
		"client_secret": c["clientSecret"],
		"username":      c["username"],
		"password":      c["password"],
		"grant_type":    "password",
	})
	if err != nil {
		return "", ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/aladdin/api/v1/issue-token", bytes.NewReader(payload))
	if err != nil {
		return "", ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", ErrUnavailable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))

	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrInvalidCredentials
	case http.StatusOK:
	default:
		return "", ErrUnavailable
	}
	var out pathaoTokenResponse
	if json.Unmarshal(body, &out) != nil || out.AccessToken == "" ||
		strings.ContainsAny(out.AccessToken, "\r\n\x00 ") {
		return "", ErrUnavailable
	}
	return out.AccessToken, nil
}

// Verify confirms the login with a token, then checks the Store ID (or that a
// default store can be chosen) against GET /stores, so a wrong store is caught
// when the seller connects instead of on the first parcel.
func (p *Pathao) Verify(ctx context.Context, c map[string]string) error {
	tok, err := p.token(ctx, c)
	if err != nil {
		return err
	}
	stores, err := p.listStores(ctx, tok)
	if err != nil {
		return err
	}
	_, err = resolveStore(stores, c["storeId"])
	return err
}

// ---- stores ----

type pathaoStore struct {
	ID      json.Number     `json:"store_id"` // string or number in the docs
	Name    string          `json:"store_name"`
	Active  json.RawMessage `json:"is_active"`
	Default json.RawMessage `json:"is_default_store"`
}

func truthy(raw json.RawMessage) bool {
	v := strings.Trim(string(raw), `" `)
	return v == "1" || v == "true"
}

func (s pathaoStore) id() int {
	n, err := strconv.Atoi(s.ID.String())
	if err != nil {
		return 0
	}
	return n
}

func (p *Pathao) listStores(ctx context.Context, tok string) ([]pathaoStore, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/aladdin/api/v1/stores", nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrInvalidCredentials
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}
	var out struct {
		Data *struct {
			Data []pathaoStore `json:"data"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &out) != nil || out.Data == nil {
		return nil, ErrUnavailable
	}
	return out.Data.Data, nil
}

// resolveStore returns the store id to book from. A given Store ID must be an
// active store on the account; with none given the default (or only active)
// store is used.
func resolveStore(stores []pathaoStore, wanted string) (int, error) {
	var active []pathaoStore
	for _, s := range stores {
		if s.id() > 0 && truthy(s.Active) {
			active = append(active, s)
		}
	}
	list := func() string {
		var parts []string
		for i, s := range active {
			if i == 5 {
				break
			}
			parts = append(parts, s.ID.String()+" ("+strings.TrimSpace(s.Name)+")")
		}
		return strings.Join(parts, ", ")
	}
	bad := func(msg string) error { return &ErrInvalidField{Key: "storeId", Message: msg} }

	if wanted != "" {
		n, err := strconv.Atoi(wanted)
		if err != nil || n <= 0 {
			return 0, bad("Store ID must be a number, like 123456.")
		}
		for _, s := range active {
			if s.id() == n {
				return n, nil
			}
		}
		for _, s := range stores {
			if s.id() == n {
				return 0, bad("This Pathao store is not active yet. New stores are usually approved within an hour.")
			}
		}
		if len(active) == 0 {
			return 0, bad("This Store ID is not on your Pathao account, and the account has no active store.")
		}
		return 0, bad("This Store ID is not on your Pathao account. Your active stores: " + list() + ".")
	}
	for _, s := range active {
		if truthy(s.Default) {
			return s.id(), nil
		}
	}
	if len(active) == 1 {
		return active[0].id(), nil
	}
	if len(active) == 0 {
		return 0, bad("Your Pathao account has no active store. Create one in the Pathao merchant panel first.")
	}
	return 0, bad("Your account has several stores and no default. Enter a Store ID: " + list() + ".")
}

// ---- shipping ----

const pathaoTrackURL = "https://merchant.pathao.com/tracking"

type pathaoOrderBody struct {
	StoreID          int     `json:"store_id"`
	MerchantOrderID  string  `json:"merchant_order_id"`
	RecipientName    string  `json:"recipient_name"`
	RecipientPhone   string  `json:"recipient_phone"`
	RecipientAddress string  `json:"recipient_address"`
	DeliveryType     int     `json:"delivery_type"` // 48 = normal delivery
	ItemType         int     `json:"item_type"`     // 2 = parcel
	ItemQuantity     int     `json:"item_quantity"`
	ItemWeight       float64 `json:"item_weight"` // kg, 0.5-10
	AmountToCollect  int     `json:"amount_to_collect"`
	Special          string  `json:"special_instruction,omitempty"`
}

type pathaoOrderResponse struct {
	Message string              `json:"message"`
	Code    json.Number         `json:"code"`
	Errors  map[string][]string `json:"errors"`
	Data    *struct {
		ConsignmentID string `json:"consignment_id"`
	} `json:"data"`
}

// CreateShipment books one parcel with POST /aladdin/api/v1/orders.
// Success requires a consignment_id; anything else is never reported as
// success. A fixed 0.5 kg weight and normal (48h) delivery are used until the
// order model carries weight.
func (p *Pathao) CreateShipment(ctx context.Context, c map[string]string, in ShipmentRequest) (*ShipmentResult, error) {
	tok, err := p.token(ctx, c)
	if err != nil {
		return nil, err
	}
	storeID, err := strconv.Atoi(c["storeId"])
	if err != nil || storeID <= 0 {
		// No (valid) Store ID saved: use the account's default store.
		stores, lerr := p.listStores(ctx, tok)
		if lerr != nil {
			return nil, lerr
		}
		storeID, err = resolveStore(stores, "")
		if err != nil {
			if bad, ok := err.(*ErrInvalidField); ok {
				return nil, &ErrShipmentRejected{Message: bad.Message}
			}
			return nil, err
		}
	}
	payload, err := json.Marshal(pathaoOrderBody{
		StoreID: storeID, MerchantOrderID: in.Invoice,
		RecipientName: in.Name, RecipientPhone: in.Phone, RecipientAddress: in.Address,
		DeliveryType: 48, ItemType: 2, ItemQuantity: 1, ItemWeight: 0.5,
		AmountToCollect: int(in.CODAmount + 0.5), Special: in.Note,
	})
	if err != nil {
		return nil, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/aladdin/api/v1/orders", bytes.NewReader(payload))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrInvalidCredentials
	}
	var out pathaoOrderResponse
	jsonErr := json.Unmarshal(body, &out)

	if jsonErr == nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated) &&
		out.Data != nil && out.Data.ConsignmentID != "" {
		id := out.Data.ConsignmentID
		u := pathaoTrackURL + "?consignment_id=" + urlQueryEscape(id) + "&phone=" + urlQueryEscape(in.Phone)
		return &ShipmentResult{ConsignmentID: id, TrackingCode: id, TrackingURL: u}, nil
	}
	// 4xx with a readable reason: the parcel was refused, nothing was created.
	if jsonErr == nil && resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		if msg := pathaoReason(out); msg != "" {
			return nil, &ErrShipmentRejected{Message: msg}
		}
	}
	return nil, ErrUnavailable
}

func pathaoReason(r pathaoOrderResponse) string {
	var parts []string
	keys := make([]string, 0, len(r.Errors))
	for k := range r.Errors {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		parts = append(parts, strings.Join(r.Errors[k], " "))
	}
	if len(parts) == 0 && strings.TrimSpace(r.Message) != "" {
		parts = append(parts, strings.TrimSpace(r.Message))
	}
	msg := strings.TrimSpace(strings.Join(parts, " "))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	if msg != "" && strings.Contains(strings.ToLower(msg), "store") {
		msg += " Check the Store ID under Courier Services, Update Credentials."
	}
	return msg
}

var _ Provider = (*Pathao)(nil)
var _ Shipper = (*Pathao)(nil)
