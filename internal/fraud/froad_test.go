package fraud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizePhone(t *testing.T) {
	ok := map[string]string{
		"01712345678": "01712345678", "+8801712345678": "01712345678",
		"8801712-345678": "01712345678", "01712 345 678": "01712345678", "1712345678": "01712345678",
	}
	for in, want := range ok {
		if got, good := NormalizePhone(in); !good || got != want {
			t.Errorf("%q -> %q,%v want %q", in, got, good, want)
		}
	}
	for _, in := range []string{"", "0171234567", "01212345678", "02712345678", "abc", "017123456789"} {
		if _, good := NormalizePhone(in); good {
			t.Errorf("%q should be invalid", in)
		}
	}
}

func srv(status int, body string, seen *http.Request) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestFraudBDSuccess(t *testing.T) {
	var seen http.Request
	s := srv(200, `{"status":true,"data":{"Summaries":{
	 "Pathao":{"data_type":"rating","customer_rating":"good_customer","risk_level":"low","total":0},
	 "Steadfast":{"data_type":"delivery","total":8,"success":7,"cancel":1}},
	 "totalSummary":{"total":8,"success":7,"cancel":1}}}`, &seen)
	defer s.Close()
	res, err := NewFraudBD(s.URL, "k", nil).Check(context.Background(), "01712345678")
	if err != nil {
		t.Fatal(err)
	}
	if seen.Header.Get("api_key") != "k" || seen.URL.Path != "/api/check-courier-info" {
		t.Fatalf("bad request: %v %v", seen.Header, seen.URL)
	}
	if res.Total != 8 || res.Success != 7 || res.Rating != "good_customer" || len(res.Couriers) != 2 {
		t.Fatalf("bad result %+v", res)
	}
}

func TestFraudBDErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{401, `{"status":false}`, ErrRejected},
		{403, `{"status":false,"message":"No active plan found"}`, ErrRejected},
		{429, `{}`, ErrQuota},
		{200, `{"status":false,"message":"Usage limit (daily) reached. Please upgrade your plan."}`, ErrQuota},
		{500, `oops`, ErrUnavailable},
		{200, `not json`, ErrUnavailable},
	}
	for _, c := range cases {
		s := srv(c.status, c.body, nil)
		_, err := NewFraudBD(s.URL, "k", nil).Check(context.Background(), "01712345678")
		s.Close()
		if !errors.Is(err, c.want) {
			t.Errorf("%d %s: got %v want %v", c.status, c.body, err, c.want)
		}
	}
}
