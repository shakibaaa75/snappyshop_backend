// Package customdomain holds the pure rules for a seller's own domain: what a
// valid domain looks like, and how we check the DNS records that prove the
// seller owns it and has pointed it at us. It uses only the standard library,
// and the DNS lookups go through a small interface so tests never touch the
// network.
package customdomain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"time"
)

// VerifyPrefix is the DNS label the seller adds a TXT record under to prove
// they own the domain: _shopbd-verify.<domain>.
const VerifyPrefix = "_shopbd-verify."

// TXTValuePrefix starts the value of that TXT record.
const TXTValuePrefix = "shopbd-verify="

// lookupTimeout bounds one full ownership/routing check.
const lookupTimeout = 6 * time.Second

// reservedSuffixes can never be claimed: they are not on the public internet
// or are reserved for documentation and testing.
var reservedSuffixes = []string{
	"localhost", "local", "internal", "intranet", "lan", "home", "corp",
	"test", "example", "invalid", "onion", "arpa",
}

// Normalize turns what a seller typed into a bare lowercase hostname, or
// explains in plain words why it cannot be used. It accepts a pasted URL
// ("https://Shop.Example.com/") and strips the scheme, path and a trailing dot.
func Normalize(input string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(input))
	if s == "" {
		return "", errors.New("Enter your domain, for example shop.yourbrand.com")
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if strings.Contains(s, "@") {
		return "", errors.New("Enter only the domain name, without a username")
	}
	if strings.Contains(s, ":") {
		return "", errors.New("Enter the domain without a port number")
	}
	s = strings.TrimSuffix(s, ".")

	if s == "" || len(s) > 253 {
		return "", errors.New("That domain is not valid")
	}
	if strings.ContainsAny(s, "*_ ") {
		return "", errors.New("Wildcards, spaces and underscores are not allowed in a domain")
	}
	for _, r := range s {
		if r > 127 {
			return "", errors.New("Use the ASCII (punycode) form of the domain, for example xn--...")
		}
	}
	if net.ParseIP(s) != nil {
		return "", errors.New("Enter a domain name, not an IP address")
	}

	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return "", errors.New("Enter a full domain such as shop.yourbrand.com")
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return "", errors.New("That domain is not valid")
		}
		if l[0] == '-' || l[len(l)-1] == '-' {
			return "", errors.New("A domain part cannot start or end with a dash")
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return "", errors.New("A domain can only contain letters, numbers, dots and dashes")
			}
		}
	}
	tld := labels[len(labels)-1]
	if len(tld) < 2 || allDigits(tld) {
		return "", errors.New("That domain does not end in a valid extension such as .com")
	}
	for _, r := range reservedSuffixes {
		if tld == r {
			return "", errors.New("That domain cannot be used on the internet")
		}
	}
	return s, nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// IsPlatformHost reports whether host is one of the platform's own hosts, or a
// subdomain of one. Sellers must not claim those.
func IsPlatformHost(host string, platformHosts []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range platformHosts {
		p = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(p), "."))
		if p == "" {
			continue
		}
		if host == p || strings.HasSuffix(host, "."+p) {
			return true
		}
	}
	return false
}

// NewToken returns a fresh random verification token.
func NewToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TXTName is the DNS name the ownership record goes on.
func TXTName(domain string) string { return VerifyPrefix + domain }

// TXTValue is the exact value of the ownership record.
func TXTValue(token string) string { return TXTValuePrefix + token }

// Resolver is the part of *net.Resolver the checks use.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// Result is what one DNS check found.
type Result struct {
	// Owned is true when the ownership TXT record is present and correct.
	Owned bool
	// Routed is true when the domain points at the platform (CNAME or A
	// record). It is true when no routing target is configured. It is only
	// informational: a domain behind a proxy such as Cloudflare can work while
	// this stays false.
	Routed bool
	// Message explains a failed ownership check in plain words.
	Message string
}

// Checker runs the DNS checks.
type Checker struct {
	Resolver    Resolver
	CNAMETarget string   // e.g. "stores.shopbd.com"; "" = not configured
	IPs         []string // platform IPs for root-domain A records
}

// NewChecker returns a Checker that uses the system DNS resolver.
func NewChecker(cnameTarget string, ips []string) *Checker {
	return &Checker{Resolver: net.DefaultResolver, CNAMETarget: cnameTarget, IPs: ips}
}

// Check looks up the ownership record and the routing records for domain.
func (c *Checker) Check(ctx context.Context, domain, token string) Result {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	res := Result{Routed: c.routed(ctx, domain)}

	records, err := c.Resolver.LookupTXT(ctx, TXTName(domain))
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && (dnsErr.IsNotFound) {
			res.Message = "We could not find the verification record yet. DNS changes can take a few minutes, sometimes longer."
		} else {
			res.Message = "We could not reach your DNS right now. Please try again in a minute."
		}
		return res
	}
	want := TXTValue(token)
	for _, r := range records {
		if strings.TrimSpace(strings.Trim(strings.TrimSpace(r), `"`)) == want {
			res.Owned = true
			return res
		}
	}
	res.Message = "We found a verification record, but its value does not match. Copy the value again exactly as shown."
	return res
}

func (c *Checker) routed(ctx context.Context, domain string) bool {
	target := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(c.CNAMETarget), "."))
	if target == "" && len(c.IPs) == 0 {
		return true // nothing to compare against
	}
	if target != "" {
		if cname, err := c.Resolver.LookupCNAME(ctx, domain); err == nil {
			if strings.ToLower(strings.TrimSuffix(cname, ".")) == target {
				return true
			}
		}
	}
	if len(c.IPs) > 0 {
		addrs, err := c.Resolver.LookupHost(ctx, domain)
		if err == nil {
			for _, a := range addrs {
				for _, want := range c.IPs {
					if a == want {
						return true
					}
				}
			}
		}
	}
	return false
}
