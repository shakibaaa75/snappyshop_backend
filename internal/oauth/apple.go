package oauth

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	appleAuthURL  = "https://appleid.apple.com/auth/authorize"
	appleTokenURL = "https://appleid.apple.com/auth/token"
	appleJWKSURL  = "https://appleid.apple.com/auth/keys"
	appleIssuer   = "https://appleid.apple.com"
)

// Apple is "Continue with Apple". Apple wants a client secret that is itself
// a short-lived JWT signed with the .p8 key from the developer account.
type Apple struct {
	clientID    string // the Services ID, e.g. com.shopbd.web
	teamID      string
	keyID       string
	key         *ecdsa.PrivateKey
	redirectURI string
	keys        *jwks
}

// NewApple parses the .p8 private key (PEM text; "\n" written as a literal
// backslash-n, as env files often do, is accepted).
func NewApple(clientID, teamID, keyID, privateKeyPEM, redirectURI string) (*Apple, error) {
	pem := strings.ReplaceAll(privateKeyPEM, `\n`, "\n")
	key, err := jwt.ParseECPrivateKeyFromPEM([]byte(pem))
	if err != nil {
		return nil, errors.New("oauth: APPLE_PRIVATE_KEY is not a valid .p8 key")
	}
	return &Apple{
		clientID: clientID, teamID: teamID, keyID: keyID, key: key,
		redirectURI: redirectURI, keys: newJWKS(appleJWKSURL),
	}, nil
}

// AuthURL is where the browser is sent. Apple answers with a POST to the
// redirect URI (response_mode=form_post is required to receive name + email).
func (a *Apple) AuthURL(state, nonce string) string {
	q := url.Values{}
	q.Set("client_id", a.clientID)
	q.Set("redirect_uri", a.redirectURI)
	q.Set("response_type", "code")
	q.Set("response_mode", "form_post")
	q.Set("scope", "name email")
	q.Set("state", state)
	q.Set("nonce", nonce)
	return appleAuthURL + "?" + q.Encode()
}

func (a *Apple) clientSecret() (string, error) {
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.RegisteredClaims{
		Issuer:    a.teamID,
		Subject:   a.clientID,
		Audience:  jwt.ClaimStrings{appleIssuer},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
	})
	t.Header["kid"] = a.keyID
	return t.SignedString(a.key)
}

// Exchange turns the callback code into a verified profile. Apple never puts
// the name in the ID token; the caller adds it from the first-login form field.
func (a *Apple) Exchange(ctx context.Context, code, nonce string) (*Profile, error) {
	secret, err := a.clientSecret()
	if err != nil {
		return nil, err
	}
	raw, err := exchangeCode(ctx, appleTokenURL, url.Values{
		"client_id":     {a.clientID},
		"client_secret": {secret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {a.redirectURI},
	})
	if err != nil {
		return nil, err
	}
	c, err := verifyIDToken(ctx, raw, a.keys, []string{appleIssuer}, a.clientID, nonce)
	if err != nil {
		return nil, err
	}
	return &Profile{
		Provider:      "apple",
		Subject:       c.Subject,
		Email:         strings.TrimSpace(c.Email),
		EmailVerified: bool(c.EmailVerified),
	}, nil
}
