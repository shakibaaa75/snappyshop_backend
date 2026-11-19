package customdomain

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	good := map[string]string{
		"shop.example.com":              "shop.example.com",
		"  Shop.Example.COM  ":          "shop.example.com",
		"https://www.brand.com.bd/":     "www.brand.com.bd",
		"http://brand.com/shop?x=1#top": "brand.com",
		"brand.com.":                    "brand.com",
		"my-shop.brand.com":             "my-shop.brand.com",
		"xn--mnchen-3ya.de":             "xn--mnchen-3ya.de",
		"a1.b2.c3.example.org":          "a1.b2.c3.example.org",
	}
	for in, want := range good {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	bad := []string{
		"", "   ", "localhost", "brand", "127.0.0.1", "::1",
		"*.brand.com", "my_shop.brand.com", "brand.com:8080", "user@brand.com",
		"-brand.com", "brand-.com", "brand..com", ".brand.com",
		"münchen.de", "brand.c", "brand.123", "printer.local", "x.internal",
		"site.test", "site.example", "hidden.onion",
		strings.Repeat("a", 64) + ".com",
		strings.Repeat("a.", 130) + "com",
	}
	for _, in := range bad {
		if got, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q; want an error", in, got)
		}
	}
}

func TestIsPlatformHost(t *testing.T) {
	platform := []string{"shopbd.com", "app.shopbd.io"}
	yes := []string{"shopbd.com", "www.shopbd.com", "a.b.shopbd.com", "app.shopbd.io", "SHOPBD.COM."}
	no := []string{"notshopbd.com", "shopbd.com.evil.com", "shopbd.io", "brand.com"}
	for _, h := range yes {
		if !IsPlatformHost(h, platform) {
			t.Errorf("IsPlatformHost(%q) = false; want true", h)
		}
	}
	for _, h := range no {
		if IsPlatformHost(h, platform) {
			t.Errorf("IsPlatformHost(%q) = true; want false", h)
		}
	}
	if IsPlatformHost("brand.com", []string{"", "  "}) {
		t.Error("blank platform hosts must be ignored")
	}
}

func TestNewTokenIsRandom(t *testing.T) {
	a, err := NewToken()
	if err != nil || len(a) != 32 {
		t.Fatalf("NewToken() = %q, %v", a, err)
	}
	b, _ := NewToken()
	if a == b {
		t.Error("two tokens were identical")
	}
}

type fakeResolver struct {
	txt    map[string][]string
	txtErr error
	cname  map[string]string
	hosts  map[string][]string
}

func (f fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if f.txtErr != nil {
		return nil, f.txtErr
	}
	v, ok := f.txt[name]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return v, nil
}
func (f fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	if v, ok := f.cname[host]; ok {
		return v, nil
	}
	return host + ".", nil
}
func (f fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	return f.hosts[host], nil
}

func TestCheckOwnership(t *testing.T) {
	const domain, token = "shop.brand.com", "abc123"
	name := TXTName(domain)
	if name != "_shopbd-verify.shop.brand.com" {
		t.Fatalf("TXTName = %q", name)
	}

	cases := []struct {
		name  string
		res   fakeResolver
		owned bool
	}{
		{"correct", fakeResolver{txt: map[string][]string{name: {"shopbd-verify=abc123"}}}, true},
		{"quoted and padded", fakeResolver{txt: map[string][]string{name: {` "shopbd-verify=abc123" `}}}, true},
		{"among other records", fakeResolver{txt: map[string][]string{name: {"v=spf1 -all", "shopbd-verify=abc123"}}}, true},
		{"wrong token", fakeResolver{txt: map[string][]string{name: {"shopbd-verify=zzz"}}}, false},
		{"another sellers token", fakeResolver{txt: map[string][]string{name: {"shopbd-verify=abc1234"}}}, false},
		{"missing", fakeResolver{txt: map[string][]string{}}, false},
		{"dns down", fakeResolver{txtErr: errors.New("timeout")}, false},
	}
	for _, c := range cases {
		got := (&Checker{Resolver: c.res}).Check(context.Background(), domain, token)
		if got.Owned != c.owned {
			t.Errorf("%s: Owned = %v; want %v (%s)", c.name, got.Owned, c.owned, got.Message)
		}
		if !c.owned && got.Message == "" {
			t.Errorf("%s: a failed check must explain itself", c.name)
		}
	}
}

func TestCheckRouting(t *testing.T) {
	const domain = "shop.brand.com"
	owned := map[string][]string{TXTName(domain): {"shopbd-verify=t"}}

	// Nothing configured: nothing to compare, so it counts as routed.
	if r := (&Checker{Resolver: fakeResolver{txt: owned}}).Check(context.Background(), domain, "t"); !r.Routed {
		t.Error("no routing target configured should count as routed")
	}

	// CNAME to the platform target (case and trailing dot ignored).
	c := &Checker{
		Resolver:    fakeResolver{txt: owned, cname: map[string]string{domain: "Stores.ShopBD.com."}},
		CNAMETarget: "stores.shopbd.com",
	}
	if r := c.Check(context.Background(), domain, "t"); !r.Routed || !r.Owned {
		t.Errorf("CNAME match: %+v", r)
	}

	// A record to a platform IP.
	c = &Checker{
		Resolver: fakeResolver{txt: owned, hosts: map[string][]string{domain: {"203.0.113.9", "198.51.100.7"}}},
		IPs:      []string{"198.51.100.7"},
	}
	if r := c.Check(context.Background(), domain, "t"); !r.Routed {
		t.Errorf("A record match: %+v", r)
	}

	// Points somewhere else: owned but not routed.
	c = &Checker{
		Resolver:    fakeResolver{txt: owned, hosts: map[string][]string{domain: {"203.0.113.9"}}},
		CNAMETarget: "stores.shopbd.com",
		IPs:         []string{"198.51.100.7"},
	}
	if r := c.Check(context.Background(), domain, "t"); r.Routed || !r.Owned {
		t.Errorf("elsewhere: %+v", r)
	}
}
