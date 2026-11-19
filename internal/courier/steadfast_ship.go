package courier

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// steadfastTrackURL is the public tracking page for a tracking code.
const steadfastTrackURL = "https://steadfast.com.bd/t/"

type steadfastCreateBody struct {
	Invoice          string  `json:"invoice"`
	RecipientName    string  `json:"recipient_name"`
	RecipientPhone   string  `json:"recipient_phone"`
	RecipientAddress string  `json:"recipient_address"`
	CODAmount        float64 `json:"cod_amount"`
	Note             string  `json:"note,omitempty"`
}

type steadfastCreateResponse struct {
	Status      json.Number     `json:"status"`
	Message     string          `json:"message"`
	Errors      json.RawMessage `json:"errors"`
	Consignment *struct {
		ConsignmentID json.Number `json:"consignment_id"`
		TrackingCode  string      `json:"tracking_code"`
	} `json:"consignment"`
}

// CreateShipment books one parcel with POST /create_order (Steadfast API V1).
// Success requires a consignment with a tracking code; anything else is never
// reported as success.
func (s *Steadfast) CreateShipment(ctx context.Context, c map[string]string, in ShipmentRequest) (*ShipmentResult, error) {
	key, secret := c["apiKey"], c["secretKey"]
	if key == "" || secret == "" || strings.ContainsAny(key+secret, "\r\n\x00") {
		return nil, ErrInvalidCredentials
	}
	payload, err := json.Marshal(steadfastCreateBody{
		Invoice: in.Invoice, RecipientName: in.Name, RecipientPhone: in.Phone,
		RecipientAddress: in.Address, CODAmount: in.CODAmount, Note: in.Note,
	})
	if err != nil {
		return nil, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/create_order", bytes.NewReader(payload))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Api-Key", key)
	req.Header.Set("Secret-Key", secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrInvalidCredentials
	}
	var out steadfastCreateResponse
	jsonErr := json.Unmarshal(body, &out)

	if resp.StatusCode == http.StatusOK && jsonErr == nil && out.Status.String() == "200" &&
		out.Consignment != nil && out.Consignment.TrackingCode != "" {
		return &ShipmentResult{
			ConsignmentID: out.Consignment.ConsignmentID.String(),
			TrackingCode:  out.Consignment.TrackingCode,
			TrackingURL:   steadfastTrackURL + url.PathEscape(out.Consignment.TrackingCode),
		}, nil
	}
	if jsonErr == nil && (out.Status.String() == "401" || out.Status.String() == "403") {
		return nil, ErrInvalidCredentials
	}
	// 4xx with a readable reason: the parcel was refused, nothing was created.
	if jsonErr == nil && resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		if msg := steadfastReason(out); msg != "" {
			return nil, &ErrShipmentRejected{Message: msg}
		}
	}
	return nil, ErrUnavailable
}

// steadfastReason builds a short, seller-safe explanation from the response.
func steadfastReason(r steadfastCreateResponse) string {
	var parts []string
	var fields map[string][]string
	if len(r.Errors) > 0 && json.Unmarshal(r.Errors, &fields) == nil {
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, strings.Join(fields[k], " "))
		}
	}
	if len(parts) == 0 && strings.TrimSpace(r.Message) != "" {
		parts = append(parts, strings.TrimSpace(r.Message))
	}
	msg := strings.Join(parts, " ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return strings.TrimSpace(msg)
}

var _ Shipper = (*Steadfast)(nil)
