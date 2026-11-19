package customdomain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Attacher tells whatever serves the storefront about a seller's domain.
// DNS verification only proves the seller owns the domain; the frontend host
// (Vercel) still has to be told to accept requests for it and to issue its
// HTTPS certificate. Attach and Detach must be safe to repeat.
type Attacher interface {
	Attach(ctx context.Context, domain string) error
	Detach(ctx context.Context, domain string) error
}

// NoopAttacher does nothing. Use it locally, or on a host that needs no
// per-domain setup (for example a VPS running Caddy with on-demand TLS).
type NoopAttacher struct{}

func (NoopAttacher) Attach(context.Context, string) error { return nil }
func (NoopAttacher) Detach(context.Context, string) error { return nil }

// VercelAttacher adds and removes domains on one Vercel project.
type VercelAttacher struct {
	Token   string // Vercel API token
	Project string // project ID or name
	TeamID  string // optional; needed when the project belongs to a team
	BaseURL string // optional; tests only. Default https://api.vercel.com
	Client  *http.Client
}

// NewVercelAttacher returns an attacher with a sensible HTTP timeout.
func NewVercelAttacher(token, project, teamID string) *VercelAttacher {
	return &VercelAttacher{
		Token: token, Project: project, TeamID: teamID,
		Client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (v *VercelAttacher) url(path string) string {
	base := strings.TrimRight(v.BaseURL, "/")
	if base == "" {
		base = "https://api.vercel.com"
	}
	u := base + path
	if v.TeamID != "" {
		u += "?teamId=" + url.QueryEscape(v.TeamID)
	}
	return u
}

func (v *VercelAttacher) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, v.url(path), rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+v.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := v.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, b, nil
}

func (v *VercelAttacher) domainPath(domain string) string {
	return "/v9/projects/" + url.PathEscape(v.Project) + "/domains/" + url.PathEscape(domain)
}

// Attach adds the domain to the project. A domain that is already on this
// project counts as success.
func (v *VercelAttacher) Attach(ctx context.Context, domain string) error {
	body, _ := json.Marshal(map[string]string{"name": domain})
	status, resp, err := v.do(ctx, http.MethodPost, "/v10/projects/"+url.PathEscape(v.Project)+"/domains", body)
	if err != nil {
		return fmt.Errorf("vercel: add domain: %w", err)
	}
	if status >= 200 && status < 300 {
		return nil
	}
	if status == http.StatusConflict {
		// Conflict can mean "already on this project" (fine) or "used by
		// another project" (not fine). Ask which.
		if st, _, err := v.do(ctx, http.MethodGet, v.domainPath(domain), nil); err == nil && st == http.StatusOK {
			return nil
		}
	}
	return vercelError("add domain", status, resp)
}

// Detach removes the domain from the project. A domain that is not there
// counts as success.
func (v *VercelAttacher) Detach(ctx context.Context, domain string) error {
	status, resp, err := v.do(ctx, http.MethodDelete, v.domainPath(domain), nil)
	if err != nil {
		return fmt.Errorf("vercel: remove domain: %w", err)
	}
	if (status >= 200 && status < 300) || status == http.StatusNotFound {
		return nil
	}
	return vercelError("remove domain", status, resp)
}

func vercelError(what string, status int, body []byte) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error.Code != "" || e.Error.Message != "" {
		return fmt.Errorf("vercel: %s: HTTP %d %s: %s", what, status, e.Error.Code, e.Error.Message)
	}
	return fmt.Errorf("vercel: %s: HTTP %d", what, status)
}
