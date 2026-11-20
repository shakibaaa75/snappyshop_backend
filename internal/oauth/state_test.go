package oauth

import "testing"

func TestPendingLinkRoundTripAndIsolation(t *testing.T) {
	s := NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	tok, err := s.SealLink(PendingLink{Provider: "google", Subject: "g-1", UserID: "u1", Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.OpenLink(tok)
	if err != nil || l.UserID != "u1" || l.Subject != "g-1" || l.Email != "a@example.com" {
		t.Fatalf("round trip: %+v %v", l, err)
	}
	// A link token is not an OAuth state token, and the other way round.
	if _, err := s.Open(tok); err == nil {
		t.Fatal("link token must not open as state")
	}
	st, _ := s.Seal(StateData{Provider: "google", State: "s", Nonce: "n"})
	if _, err := s.OpenLink(st); err == nil {
		t.Fatal("state token must not open as link")
	}
	// Tampering or another key is rejected.
	if _, err := NewSigner([]byte("another-secret-another-secret-12")).OpenLink(tok); err == nil {
		t.Fatal("wrong key must fail")
	}
	if _, err := s.OpenLink(tok + "x"); err == nil {
		t.Fatal("tampered token must fail")
	}
}
