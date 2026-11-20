// Package oauth implements "Sign in with Google" and "Sign in with Apple"
// (OpenID Connect, authorization-code flow). It only talks to the providers;
// creating the user and the session is done by service.AuthService.
package oauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	stateAudience = "shopbd-oauth-state"
	stateTTL      = 10 * time.Minute
)

// Profile is what a provider tells us about the person who signed in.
type Profile struct {
	Provider      string
	Subject       string // the provider's stable ID for this person
	Email         string
	EmailVerified bool
	Name          string
}

// StateData is kept in a signed, short-lived cookie while the person is at the
// provider. Nothing is stored on the server.
type StateData struct {
	Provider string
	State    string // must come back unchanged in the callback URL
	Nonce    string // must come back inside the ID token
	Verifier string // PKCE code verifier (Google)
	Next     string // where to send the person after login
}

type stateClaims struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v,omitempty"`
	Next     string `json:"x,omitempty"`
	jwt.RegisteredClaims
}

// Signer seals and opens StateData.
type Signer struct {
	key []byte
	now func() time.Time
}

// NewSigner derives its own key from the JWT secret, so a state cookie can
// never be used as an access token, and the other way round.
func NewSigner(secret []byte) *Signer {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("shopbd/oauth-state/v1"))
	return &Signer{key: mac.Sum(nil), now: time.Now}
}

func (s *Signer) Seal(d StateData) (string, error) {
	now := s.now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, stateClaims{
		Provider: d.Provider, State: d.State, Nonce: d.Nonce, Verifier: d.Verifier, Next: d.Next,
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{stateAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(stateTTL)),
		},
	}).SignedString(s.key)
}

func (s *Signer) Open(token string) (*StateData, error) {
	c := &stateClaims{}
	_, err := jwt.ParseWithClaims(token, c,
		func(*jwt.Token) (any, error) { return s.key, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithAudience(stateAudience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.now),
	)
	if err != nil {
		return nil, errors.New("oauth: invalid state")
	}
	return &StateData{Provider: c.Provider, State: c.State, Nonce: c.Nonce, Verifier: c.Verifier, Next: c.Next}, nil
}

// Random returns n random bytes as URL-safe text.
func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("oauth: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Challenge is the PKCE S256 challenge for a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// SameString compares secrets in constant time.
func SameString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

/* ---- pending account link ---- */

const (
	linkAudience = "shopbd-oauth-link"
	linkTTL      = 10 * time.Minute
)

// PendingLink is kept in a signed, short-lived cookie after Google / Apple has
// vouched for an email that already belongs to an account. It records what the
// provider confirmed, but grants nothing: the person must still prove they own
// the existing account (password or active session) before it is linked.
type PendingLink struct {
	Provider string
	Subject  string // the provider's ID for this person
	UserID   string // the existing account to link to
	Email    string // shown to the person so they know which account
	Name     string
}

type linkClaims struct {
	Provider string `json:"p"`
	Subject  string `json:"s"`
	UserID   string `json:"u"`
	Email    string `json:"e"`
	Name     string `json:"n,omitempty"`
	jwt.RegisteredClaims
}

// SealLink signs a PendingLink. It uses a different audience than the OAuth
// state cookie, so one can never be presented as the other.
func (s *Signer) SealLink(l PendingLink) (string, error) {
	now := s.now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, linkClaims{
		Provider: l.Provider, Subject: l.Subject, UserID: l.UserID, Email: l.Email, Name: l.Name,
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{linkAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(linkTTL)),
		},
	}).SignedString(s.key)
}

func (s *Signer) OpenLink(token string) (*PendingLink, error) {
	c := &linkClaims{}
	_, err := jwt.ParseWithClaims(token, c,
		func(*jwt.Token) (any, error) { return s.key, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithAudience(linkAudience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.now),
	)
	if err != nil || c.Provider == "" || c.Subject == "" || c.UserID == "" {
		return nil, errors.New("oauth: invalid link token")
	}
	return &PendingLink{Provider: c.Provider, Subject: c.Subject, UserID: c.UserID, Email: c.Email, Name: c.Name}, nil
}
