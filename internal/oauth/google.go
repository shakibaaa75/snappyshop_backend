package oauth

import (
	"context"
	"net/url"
	"strings"
)

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleJWKSURL  = "https://www.googleapis.com/oauth2/v3/certs"
)

// Google is "Continue with Google".
type Google struct {
	clientID     string
	clientSecret string
	redirectURI  string
	keys         *jwks
}

func NewGoogle(clientID, clientSecret, redirectURI string) *Google {
	return &Google{clientID: clientID, clientSecret: clientSecret, redirectURI: redirectURI, keys: newJWKS(googleJWKSURL)}
}

// AuthURL is where the browser is sent to pick a Google account.
func (g *Google) AuthURL(state, nonce, challenge string) string {
	q := url.Values{}
	q.Set("client_id", g.clientID)
	q.Set("redirect_uri", g.redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("prompt", "select_account")
	return googleAuthURL + "?" + q.Encode()
}

// Exchange turns the callback code into a verified profile.
func (g *Google) Exchange(ctx context.Context, code, verifier, nonce string) (*Profile, error) {
	raw, err := exchangeCode(ctx, googleTokenURL, url.Values{
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {g.redirectURI},
	})
	if err != nil {
		return nil, err
	}
	c, err := verifyIDToken(ctx, raw, g.keys,
		[]string{"https://accounts.google.com", "accounts.google.com"}, g.clientID, nonce)
	if err != nil {
		return nil, err
	}
	return &Profile{
		Provider:      "google",
		Subject:       c.Subject,
		Email:         strings.TrimSpace(c.Email),
		EmailVerified: bool(c.EmailVerified),
		Name:          strings.TrimSpace(c.Name),
	}, nil
}
