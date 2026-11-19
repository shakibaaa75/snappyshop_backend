package customdomain

import (
	"context"
	"net"
	"strings"
	"time"
)

// twoLevelSuffixes are common public suffixes with two labels. A name with one
// label in front of one of these (mystore.com.bd) is a root domain.
var twoLevelSuffixes = map[string]bool{
	"com.bd": true, "net.bd": true, "org.bd": true, "edu.bd": true,
	"gov.bd": true, "co.bd": true, "ac.bd": true, "info.bd": true,
	"co.uk": true, "org.uk": true, "com.au": true, "co.in": true,
	"com.pk": true, "co.nz": true, "com.sg": true, "com.my": true,
	"co.za": true, "com.br": true,
	// Free domain providers: mystore.dpdns.org is a root domain here too.
	"dpdns.org": true, "qzz.io": true, "us.kg": true, "xx.kg": true,
}

// IsApex reports whether domain is a root domain such as mystore.com or
// mystore.com.bd, as opposed to a subdomain such as shop.mystore.com. Root
// domains cannot use a CNAME record, so they are pointed with A records.
func IsApex(domain string) bool {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	labels := strings.Split(domain, ".")
	for _, l := range labels {
		if l == "" {
			return false
		}
	}
	switch len(labels) {
	case 2:
		return true
	case 3:
		return twoLevelSuffixes[labels[1]+"."+labels[2]]
	}
	return false
}

// PublicResolver returns a resolver that asks one DNS server directly
// (for example "1.1.1.1:53"). An empty value or "system" uses the machine's
// own resolver.
func PublicResolver(server string) *net.Resolver {
	server = strings.TrimSpace(server)
	if server == "" || strings.EqualFold(server, "system") {
		return net.DefaultResolver
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, network, server)
		},
	}
}

// DevChecker accepts every domain without looking at DNS.
// LOCAL TESTING ONLY: main.go never uses it in production.
type DevChecker struct{}

// Check reports the domain as owned and routed.
func (DevChecker) Check(_ context.Context, _, _ string) Result {
	return Result{Owned: true, Routed: true}
}
