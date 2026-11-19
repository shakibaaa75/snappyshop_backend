package oauth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

/* ---- the provider's public keys ---- */

// jwks downloads and caches a provider's signing keys.
type jwks struct {
	url string

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func newJWKS(url string) *jwks { return &jwks{url: url} }

func (j *jwks) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if k, ok := j.keys[kid]; ok && time.Since(j.fetched) < time.Hour {
		return k, nil
	}
	// Unknown key: providers rotate keys, so refresh once, but never more than
	// once a minute so bad tokens cannot make us hammer the provider.
	if j.keys != nil && time.Since(j.fetched) < time.Minute {
		if k, ok := j.keys[kid]; ok {
			return k, nil
		}
		return nil, errors.New("oauth: unknown signing key")
	}
	if err := j.refresh(ctx); err != nil {
		return nil, err
	}
	if k, ok := j.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("oauth: unknown signing key")
}

func (j *jwks) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		return err
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: fetch keys: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("oauth: fetch keys: status %d", res.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&doc); err != nil {
		return fmt.Errorf("oauth: parse keys: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("oauth: provider returned no usable keys")
	}
	j.keys, j.fetched = keys, time.Now()
	return nil
}

/* ---- ID token ---- */

// flexBool reads both true and "true" (Apple sends the string form).
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	*b = flexBool(strings.Trim(string(data), `"`) == "true")
	return nil
}

type idClaims struct {
	Nonce         string   `json:"nonce"`
	Email         string   `json:"email"`
	EmailVerified flexBool `json:"email_verified"`
	Name          string   `json:"name"`
	jwt.RegisteredClaims
}

// verifyIDToken checks the signature, issuer, audience, expiry and nonce.
func verifyIDToken(ctx context.Context, raw string, keys *jwks, issuers []string, audience, nonce string) (*idClaims, error) {
	c := &idClaims{}
	_, err := jwt.ParseWithClaims(raw, c,
		func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			if kid == "" {
				return nil, errors.New("missing kid")
			}
			return keys.key(ctx, kid)
		},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("oauth: id token: %w", err)
	}
	okIssuer := false
	for _, iss := range issuers {
		if c.Issuer == iss {
			okIssuer = true
		}
	}
	if !okIssuer {
		return nil, errors.New("oauth: id token: wrong issuer")
	}
	if c.Subject == "" {
		return nil, errors.New("oauth: id token: no subject")
	}
	if nonce == "" || !SameString(c.Nonce, nonce) {
		return nil, errors.New("oauth: id token: nonce mismatch")
	}
	return c, nil
}

// exchangeCode trades the one-time code for the ID token.
func exchangeCode(ctx context.Context, endpoint string, form url.Values) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth: token request: %w", err)
	}
	defer res.Body.Close()

	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("oauth: token response (status %d): %w", res.StatusCode, err)
	}
	if res.StatusCode != http.StatusOK || out.IDToken == "" {
		return "", fmt.Errorf("oauth: token exchange failed: %s %s", out.Error, out.Desc)
	}
	return out.IDToken, nil
}
