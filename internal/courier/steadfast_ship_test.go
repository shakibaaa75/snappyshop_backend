package courier

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
)

func shipReq() ShipmentRequest {
	return ShipmentRequest{Invoice: "ORD-1001", Name: "Rahim", Phone: "01712345678", Address: "House 1, Road 2, Dhaka", CODAmount: 1250, Note: ""}
}

func TestSteadfastCreateShipmentSuccess(t *testing.T) {
	var got steadfastCreateBody
	var path, key string
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("Api-Key")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Write([]byte(`{"status":200,"message":"ok","consignment":{"consignment_id":1424107,"tracking_code":"15BAEB8A"}}`))
	})
	res, err := p.CreateShipment(context.Background(), creds(), shipReq())
	if err != nil {
		t.Fatal(err)
	}
	if path != "/create_order" || key != "key-ABCD1234" {
		t.Fatalf("bad request %s %s", path, key)
	}
	if got.Invoice != "ORD-1001" || got.CODAmount != 1250 || got.RecipientPhone != "01712345678" {
		t.Fatalf("bad body %+v", got)
	}
	if res.TrackingCode != "15BAEB8A" || res.ConsignmentID != "1424107" || res.TrackingURL != "https://steadfast.com.bd/t/15BAEB8A" {
		t.Fatalf("bad result %+v", res)
	}
}

func TestSteadfastCreateShipmentErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		check  func(error) bool
	}{
		{"401", 401, `{}`, func(e error) bool { return errors.Is(e, ErrInvalidCredentials) }},
		{"body 401", 200, `{"status":401}`, func(e error) bool { return errors.Is(e, ErrInvalidCredentials) }},
		{"422 field errors", 422, `{"status":400,"errors":{"recipient_phone":["The phone must be 11 digits."]}}`, func(e error) bool {
			r, ok := IsShipmentRejected(e)
			return ok && r.Message == "The phone must be 11 digits."
		}},
		{"400 message", 400, `{"status":400,"message":"Invoice already exists"}`, func(e error) bool {
			r, ok := IsShipmentRejected(e)
			return ok && r.Message == "Invoice already exists"
		}},
		{"500", 500, `oops`, func(e error) bool { return errors.Is(e, ErrUnavailable) }},
		{"429", 429, `{"message":"slow"}`, func(e error) bool { return errors.Is(e, ErrUnavailable) }},
		{"200 no consignment", 200, `{"status":200}`, func(e error) bool { return errors.Is(e, ErrUnavailable) }},
		{"200 empty tracking", 200, `{"status":200,"consignment":{"consignment_id":1,"tracking_code":""}}`, func(e error) bool { return errors.Is(e, ErrUnavailable) }},
		{"html", 200, `<html>`, func(e error) bool { return errors.Is(e, ErrUnavailable) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			})
			_, err := p.CreateShipment(context.Background(), creds(), shipReq())
			if err == nil || !c.check(err) {
				t.Fatalf("unexpected: %v", err)
			}
		})
	}
}
