// Package storage talks to Cloudflare R2 (S3-compatible object storage).
//
// It only creates short-lived presigned upload links; the browser then sends
// the file straight to R2, so image bytes never pass through this server.
// It uses the standard library only (AWS Signature V4 is signed by hand), so
// no new dependency is needed.
package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config holds the R2 settings (all from environment variables).
type Config struct {
	AccountID       string // Cloudflare account ID
	Bucket          string // e.g. "snappyy"
	AccessKeyID     string // R2 API token: Access Key ID
	SecretAccessKey string // R2 API token: Secret Access Key
	PublicBaseURL   string // public URL of the bucket, e.g. https://cdn.example.com
}

// R2 creates presigned links for one bucket.
type R2 struct {
	cfg     Config
	host    string
	now     func() time.Time
	client  *http.Client
	rewrite func(string) string // tests only: redirect the signed URL
}

// New validates the settings. Callers treat a missing config as "uploads off"
// and never call New in that case.
func New(c Config) (*R2, error) {
	c.AccountID = strings.TrimSpace(c.AccountID)
	c.Bucket = strings.TrimSpace(c.Bucket)
	c.PublicBaseURL = strings.TrimRight(strings.TrimSpace(c.PublicBaseURL), "/")
	switch {
	case c.AccountID == "" || strings.ContainsAny(c.AccountID, "/ :"):
		return nil, errors.New("storage: R2_ACCOUNT_ID is missing or invalid")
	case c.Bucket == "" || strings.ContainsAny(c.Bucket, "/ :"):
		return nil, errors.New("storage: R2_BUCKET is missing or invalid")
	case c.AccessKeyID == "" || c.SecretAccessKey == "":
		return nil, errors.New("storage: R2_ACCESS_KEY_ID and R2_SECRET_ACCESS_KEY are required")
	}
	u, err := url.Parse(c.PublicBaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("storage: R2_PUBLIC_URL must be a full URL such as https://cdn.example.com")
	}
	return &R2{cfg: c, host: c.AccountID + ".r2.cloudflarestorage.com", now: time.Now,
		client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// PresignPut returns a URL the browser can PUT the file to. The upload only
// works with exactly this Content-Type header, until ttl runs out.
func (s *R2) PresignPut(key, contentType string, ttl time.Duration) string {
	return sign(signParams{
		method:       "PUT",
		host:         s.host,
		path:         "/" + s.cfg.Bucket + "/" + key,
		region:       "auto",
		accessKey:    s.cfg.AccessKeyID,
		secretKey:    s.cfg.SecretAccessKey,
		now:          s.now(),
		expires:      ttl,
		headers:      map[string]string{"content-type": contentType},
		unsignedBody: true,
	})
}

// PublicURL is the address shoppers load the image from.
func (s *R2) PublicURL(key string) string { return s.cfg.PublicBaseURL + "/" + key }

// OwnsURL reports whether u points into this bucket's public address.
// Use it to reject image URLs that were not uploaded through us.
func (s *R2) OwnsURL(u string) bool { return strings.HasPrefix(u, s.cfg.PublicBaseURL+"/") }

// ImmutableCache is stored with every processed image. Keys are random and
// never reused, so a cached copy can never go stale.
const ImmutableCache = "public, max-age=31536000, immutable"

// ValidKey reports whether key looks like one we generate:
// <folder>/<owner>/<32 hex>.<ext>. It rejects traversal, encoded characters,
// backslashes, query strings and anything else outside that shape.
func ValidKey(key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts[:2] {
		if p == "" || safeSegment(p) != p {
			return false
		}
	}
	name := parts[2]
	if len(name) < 34 || name[32] != '.' {
		return false
	}
	for _, c := range name[:32] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	switch name[33:] {
	case "webp", "jpg", "png", "mp4", "webm":
		return true
	}
	return false
}

// KeyFromURL returns the object key for a public URL of this bucket.
func (s *R2) KeyFromURL(u string) (string, bool) {
	if !s.OwnsURL(u) {
		return "", false
	}
	key := strings.TrimPrefix(u, s.cfg.PublicBaseURL+"/")
	if !ValidKey(key) {
		return "", false
	}
	return key, true
}

// PutObject uploads body from the backend. The browser never talks to R2.
// Errors never contain the signed URL.
func (s *R2) PutObject(ctx context.Context, key, contentType string, body []byte) error {
	if !ValidKey(key) {
		return errors.New("storage: invalid object key")
	}
	u := sign(signParams{
		method: "PUT", host: s.host, path: "/" + s.cfg.Bucket + "/" + key, region: "auto",
		accessKey: s.cfg.AccessKeyID, secretKey: s.cfg.SecretAccessKey, now: s.now(), expires: time.Minute,
		headers:      map[string]string{"content-type": contentType, "cache-control": ImmutableCache},
		unsignedBody: true,
	})
	if s.rewrite != nil {
		u = s.rewrite(u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return errors.New("storage: build request")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Cache-Control", ImmutableCache)
	return s.do(req, "put")
}

// DeleteObject removes one object. Deleting a missing key is not an error.
func (s *R2) DeleteObject(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return errors.New("storage: invalid object key")
	}
	u := sign(signParams{
		method: "DELETE", host: s.host, path: "/" + s.cfg.Bucket + "/" + key, region: "auto",
		accessKey: s.cfg.AccessKeyID, secretKey: s.cfg.SecretAccessKey, now: s.now(), expires: time.Minute,
		unsignedBody: true,
	})
	if s.rewrite != nil {
		u = s.rewrite(u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return errors.New("storage: build request")
	}
	return s.do(req, "delete")
}

func (s *R2) do(req *http.Request, op string) error {
	resp, err := s.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // url.Error embeds the signed URL; drop it
		}
		return fmt.Errorf("storage: %s failed: %v", op, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 && !(op == "delete" && resp.StatusCode == http.StatusNotFound) {
		return fmt.Errorf("storage: %s failed: status %d", op, resp.StatusCode)
	}
	return nil
}

/* ---------------------------- AWS SigV4 (query) -------------------------- */

type signParams struct {
	method, host, path, region string
	accessKey, secretKey       string
	now                        time.Time
	expires                    time.Duration
	headers                    map[string]string // extra signed headers (lowercase names)
	unsignedBody               bool              // adds X-Amz-Content-Sha256=UNSIGNED-PAYLOAD
}

const service = "s3"

func sign(p signParams) string {
	t := p.now.UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	scope := day + "/" + p.region + "/" + service + "/aws4_request"

	hdrs := map[string]string{"host": p.host}
	for k, v := range p.headers {
		hdrs[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	names := make([]string, 0, len(hdrs))
	for k := range hdrs {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + hdrs[k] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	q := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    p.accessKey + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       strconv.Itoa(int(p.expires.Seconds())),
		"X-Amz-SignedHeaders": signedHeaders,
	}
	// Presigned URLs never hash the body: the canonical request always ends in
	// UNSIGNED-PAYLOAD. R2 also wants it repeated as a query parameter.
	const payload = "UNSIGNED-PAYLOAD"
	if p.unsignedBody {
		q["X-Amz-Content-Sha256"] = payload
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, uriEncode(k, true)+"="+uriEncode(q[k], true))
	}
	canonQuery := strings.Join(parts, "&")
	canonPath := uriEncode(p.path, false)

	canonReq := strings.Join([]string{p.method, canonPath, canonQuery, canonHeaders.String(), signedHeaders, payload}, "\n")
	sum := sha256.Sum256([]byte(canonReq))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	k := hmacSHA256([]byte("AWS4"+p.secretKey), day)
	k = hmacSHA256(k, p.region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))

	return "https://" + p.host + canonPath + "?" + canonQuery + "&X-Amz-Signature=" + sig
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// uriEncode is the AWS flavour of percent-encoding. Slashes stay in paths.
func uriEncode(s string, encodeSlash bool) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&15])
		}
	}
	return b.String()
}
