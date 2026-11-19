package courier

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Official doc shape: store_id may be a string or number, is_active 1/0.
const storesJSON = `{"message":"Store list fetched.","type":"success","code":200,"data":{"data":[
 {"store_id":"111","store_name":"Main","is_active":1,"is_default_store":true},
 {"store_id":222,"store_name":"Second","is_active":1,"is_default_store":false},
 {"store_id":333,"store_name":"Pending","is_active":0,"is_default_store":false}]}}`

func pathaoCreds() map[string]string {
	return map[string]string{"clientId": "cid", "clientSecret": "csec", "username": "a@b.com", "password": "pw", "storeId": "222"}
}

func pathaoServe(t *testing.T, h http.HandlerFunc) *Pathao {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewPathao(srv.URL, nil)
}

func TestPathaoVerifySendsPasswordGrant(t *testing.T) {
	var got map[string]string
	var path string
	p := pathaoServe(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stores") {
			w.Write([]byte(storesJSON))
			return
		}
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"token_type":"Bearer","access_token":"tok123"}`))
	})
	if err := p.Verify(context.Background(), pathaoCreds()); err != nil {
		t.Fatal(err)
	}
	if path != "/aladdin/api/v1/issue-token" || got["grant_type"] != "password" || got["client_id"] != "cid" || got["username"] != "a@b.com" {
		t.Fatalf("bad request %s %v", path, got)
	}
}

func TestPathaoVerifyErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{400, `{"error":"invalid_client"}`, ErrInvalidCredentials},
		{401, ``, ErrInvalidCredentials},
		{500, `oops`, ErrUnavailable},
		{429, ``, ErrUnavailable},
		{200, `{"hello":"x"}`, ErrUnavailable},
		{200, `<html>`, ErrUnavailable},
	}
	for _, c := range cases {
		p := pathaoServe(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(c.status); w.Write([]byte(c.body)) })
		if err := p.Verify(context.Background(), pathaoCreds()); !errors.Is(err, c.want) {
			t.Fatalf("status %d: got %v want %v", c.status, err, c.want)
		}
	}
}

func TestPathaoVerifyRejectsHeaderInjection(t *testing.T) {
	c := pathaoCreds()
	c["password"] = "x\r\ny"
	if err := NewPathao("http://127.0.0.1:1", nil).Verify(context.Background(), c); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal(err)
	}
}

func pathaoShipReq() ShipmentRequest {
	return ShipmentRequest{Invoice: "ORD-1", Name: "Rahim", Phone: "01712345678", Address: "House 1, Road 2, Dhanmondi, Dhaka", CODAmount: 1200.4, Note: "call first"}
}

func TestPathaoCreateShipmentSuccess(t *testing.T) {
	var order map[string]any
	var auth string
	p := pathaoServe(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "issue-token") {
			w.Write([]byte(`{"access_token":"tok123"}`))
			return
		}
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &order)
		w.Write([]byte(`{"message":"Order Created Successfully","type":"success","code":200,"data":{"consignment_id":"DZ010523G674QD","order_status":"Pending"}}`))
	})
	res, err := p.CreateShipment(context.Background(), pathaoCreds(), pathaoShipReq())
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok123" || order["store_id"].(float64) != 222 || order["merchant_order_id"] != "ORD-1" || order["amount_to_collect"].(float64) != 1200 {
		t.Fatalf("bad order %v auth %q", order, auth)
	}
	if res.ConsignmentID != "DZ010523G674QD" || res.TrackingCode != "DZ010523G674QD" ||
		!strings.Contains(res.TrackingURL, "consignment_id=DZ010523G674QD") || !strings.Contains(res.TrackingURL, "phone=01712345678") {
		t.Fatalf("bad result %+v", res)
	}
}

func TestPathaoCreateShipmentFailures(t *testing.T) {
	run := func(status int, body string) error {
		p := pathaoServe(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "issue-token") {
				w.Write([]byte(`{"access_token":"t"}`))
				return
			}
			w.WriteHeader(status)
			w.Write([]byte(body))
		})
		_, err := p.CreateShipment(context.Background(), pathaoCreds(), pathaoShipReq())
		return err
	}
	if err := run(422, `{"message":"x","errors":{"recipient_address":["The recipient address must be at least 10 characters."]}}`); err == nil {
		t.Fatal("want rejection")
	} else if rej, ok := IsShipmentRejected(err); !ok || !strings.Contains(rej.Message, "at least 10") {
		t.Fatalf("got %v", err)
	}
	if err := run(401, `{}`); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
	if err := run(500, `boom`); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if err := run(200, `{"code":200,"data":{}}`); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("200 without consignment must not be success: %v", err)
	}
	if err := run(429, `{"message":"slow down"}`); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func storeServer(t *testing.T, stores string, onOrder func(map[string]any)) *Pathao {
	return pathaoServe(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "issue-token"):
			w.Write([]byte(`{"access_token":"t"}`))
		case strings.HasSuffix(r.URL.Path, "/stores"):
			w.Write([]byte(stores))
		default:
			var o map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &o)
			if onOrder != nil {
				onOrder(o)
			}
			w.Write([]byte(`{"code":200,"data":{"consignment_id":"C1"}}`))
		}
	})
}

func TestPathaoVerifyChecksStoreID(t *testing.T) {
	cases := []struct {
		store, wantSub string
	}{
		{"222", ""},                   // active, listed (number id)
		{"111", ""},                   // active, listed (string id)
		{"", ""},                      // default store exists
		{"999", "not on your Pathao"}, // wrong store: the reported bug
		{"333", "not active yet"},     // exists but pending approval
		{"abc", "must be a number"},
	}
	for _, c := range cases {
		cr := pathaoCreds()
		cr["storeId"] = c.store
		err := storeServer(t, storesJSON, nil).Verify(context.Background(), cr)
		if c.wantSub == "" {
			if err != nil {
				t.Fatalf("store %q: %v", c.store, err)
			}
			continue
		}
		var bad *ErrInvalidField
		if !errors.As(err, &bad) || bad.Key != "storeId" || !strings.Contains(bad.Message, c.wantSub) {
			t.Fatalf("store %q: got %v", c.store, err)
		}
	}
}

func TestPathaoWrongStoreMessageListsActiveStores(t *testing.T) {
	cr := pathaoCreds()
	cr["storeId"] = "999"
	err := storeServer(t, storesJSON, nil).Verify(context.Background(), cr)
	var bad *ErrInvalidField
	if !errors.As(err, &bad) || !strings.Contains(bad.Message, "111 (Main)") || strings.Contains(bad.Message, "333") {
		t.Fatalf("got %v", err)
	}
}

func TestPathaoNoStoreIDUsesDefaultStore(t *testing.T) {
	var got float64
	p := storeServer(t, storesJSON, func(o map[string]any) { got, _ = o["store_id"].(float64) })
	cr := pathaoCreds()
	delete(cr, "storeId")
	if _, err := p.CreateShipment(context.Background(), cr, pathaoShipReq()); err != nil {
		t.Fatal(err)
	}
	if got != 111 {
		t.Fatalf("booked from store %v, want default 111", got)
	}
}

func TestPathaoSeveralStoresNoDefaultNeedsChoice(t *testing.T) {
	two := `{"data":{"data":[{"store_id":1,"store_name":"A","is_active":1,"is_default_store":false},{"store_id":2,"store_name":"B","is_active":1,"is_default_store":false}]}}`
	cr := pathaoCreds()
	cr["storeId"] = ""
	if err := storeServer(t, two, nil).Verify(context.Background(), cr); err == nil {
		t.Fatal("want a store choice error")
	}
	_, err := storeServer(t, two, nil).CreateShipment(context.Background(), cr, pathaoShipReq())
	if _, ok := IsShipmentRejected(err); !ok {
		t.Fatalf("got %v", err)
	}
}

func TestPathaoStoresUnrecognisedIsUnavailable(t *testing.T) {
	err := storeServer(t, `{"hello":"x"}`, nil).Verify(context.Background(), pathaoCreds())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestPathaoWrongStoreRejectionGetsHint(t *testing.T) {
	p := pathaoServe(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "issue-token") {
			w.Write([]byte(`{"access_token":"t"}`))
			return
		}
		w.WriteHeader(422)
		w.Write([]byte(`{"message":"Wrong Store selected","type":"error","code":422}`))
	})
	_, err := p.CreateShipment(context.Background(), pathaoCreds(), pathaoShipReq())
	rej, ok := IsShipmentRejected(err)
	if !ok || !strings.Contains(rej.Message, "Wrong Store selected") || !strings.Contains(rej.Message, "Update Credentials") {
		t.Fatalf("got %v", err)
	}
}
