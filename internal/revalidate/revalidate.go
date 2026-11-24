// Package revalidate tells the Next.js storefront to drop its cached copy of
// a store, so a publish or product change shows up at once instead of when
// Next's own timer expires.
//
// It is best effort and never slows a request: Notify only queues the slug.
// A background worker sends the calls. If Next is down or the secret is not
// set, nothing breaks; Next simply refreshes on its normal timer.
package revalidate

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Path is the route on the Next.js app (app/internal/revalidate/route.ts).
const Path = "/internal/revalidate"

const (
	queueSize = 256
	maxBatch  = 50
	// batchWait lets several changes in a row (a seller editing five
	// products) become one call.
	batchWait   = 300 * time.Millisecond
	callTimeout = 3 * time.Second
)

// Client sends revalidate calls. A nil *Client is valid and does nothing.
type Client struct {
	url    string
	secret string
	http   *http.Client
	log    *slog.Logger

	ch   chan string
	once sync.Once
}

// New returns a client, or nil when baseURL or secret is empty (feature off).
func New(baseURL, secret string, log *slog.Logger) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	secret = strings.TrimSpace(secret)
	if baseURL == "" || secret == "" {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		url:    baseURL + Path,
		secret: secret,
		http:   &http.Client{Timeout: callTimeout},
		log:    log,
		ch:     make(chan string, queueSize),
	}
}

// Start runs the background worker until ctx ends. Call it once at startup.
func (c *Client) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.once.Do(func() { go c.run(ctx) })
}

// Notify queues stores to refresh. It never blocks: when the queue is full
// the slug is dropped (Next's timer still refreshes it).
func (c *Client) Notify(slugs ...string) {
	if c == nil {
		return
	}
	for _, s := range slugs {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		select {
		case c.ch <- s:
		default:
			c.log.Warn("revalidate: queue full, slug dropped", "slug", s)
		}
	}
}

func (c *Client) run(ctx context.Context) {
	for {
		var first string
		select {
		case <-ctx.Done():
			return
		case first = <-c.ch:
		}
		set := map[string]struct{}{first: {}}
		timer := time.NewTimer(batchWait)
	collect:
		for len(set) < maxBatch {
			select {
			case s := <-c.ch:
				set[s] = struct{}{}
			case <-timer.C:
				break collect
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
		timer.Stop()
		slugs := make([]string, 0, len(set))
		for s := range set {
			slugs = append(slugs, s)
		}
		c.send(ctx, slugs)
	}
}

func (c *Client) send(ctx context.Context, slugs []string) {
	body, _ := json.Marshal(map[string][]string{"slugs": slugs})
	for attempt := 0; attempt < 2; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		req, err := http.NewRequestWithContext(cctx, http.MethodPost, c.url, bytes.NewReader(body))
		if err != nil {
			cancel()
			return
		}
		req.Header.Set("Content-Type", "application/json")
		// Both headers are sent so the Next route can read either one.
		req.Header.Set("Authorization", "Bearer "+c.secret)
		req.Header.Set("x-revalidate-secret", c.secret)
		resp, err := c.http.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return
			}
			c.log.Warn("revalidate: Next answered with an error", "status", resp.StatusCode, "slugs", slugs)
			if resp.StatusCode/100 == 4 {
				return // wrong secret or route: retrying will not help
			}
		} else {
			c.log.Warn("revalidate: call failed", "error", err.Error(), "slugs", slugs)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
