package service

import (
	"strings"
	"testing"
	"time"
)

func TestSafeImageURL(t *testing.T) {
	good := []string{
		"", "https://cdn.example.com/a.png", "http://localhost:9000/a.jpg",
		"data:image/png;base64,AAAA", "DATA:image/webp;base64,AAAA",
	}
	bad := []string{
		"javascript:alert(1)", "data:text/html;base64,AAAA", "data:image/svg+xml;base64,AAAA",
		"//evil.com/a.png", "ftp://x/a.png", "https://x.com/a b.png", "https://x.com/\"onerror=1",
	}
	for _, u := range good {
		if !safeImageURL(u) {
			t.Errorf("should accept %q", u)
		}
	}
	for _, u := range bad {
		if safeImageURL(u) {
			t.Errorf("should reject %q", u)
		}
	}
}

func newGateSvc(secret string) *AdminAuthService {
	return &AdminAuthService{
		email: "admin@example.com", password: "a-long-admin-password",
		jwtSecret: []byte(secret), now: time.Now, otps: map[string]adminOTP{},
	}
}

func TestAdminGateTokenHMAC(t *testing.T) {
	s := newGateSvc(strings.Repeat("k", 32))
	tok, _, err := s.IssueGateToken()
	if err != nil {
		t.Fatal(err)
	}
	if !s.ValidGateToken(tok) {
		t.Fatal("fresh token must be valid")
	}
	// A different secret must not accept it.
	if newGateSvc(strings.Repeat("z", 32)).ValidGateToken(tok) {
		t.Fatal("token signed with another secret must be rejected")
	}
	// Tampering with the signature must fail.
	if s.ValidGateToken("00" + tok[2:]) {
		t.Fatal("tampered token must be rejected")
	}
	// Expired token must fail.
	expired := newGateSvc(strings.Repeat("k", 32))
	expired.now = func() time.Time { return time.Now().Add(-9 * time.Hour) }
	old, _, _ := expired.IssueGateToken()
	if s.ValidGateToken(old) {
		t.Fatal("expired token must be rejected")
	}
}

func TestRandomDigits(t *testing.T) {
	c, err := randomDigits(6)
	if err != nil || len(c) != 6 {
		t.Fatalf("bad code %q err=%v", c, err)
	}
	for _, r := range c {
		if r < '0' || r > '9' {
			t.Fatalf("non-digit in %q", c)
		}
	}
}
