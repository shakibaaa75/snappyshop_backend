// Package courier defines the interface every courier provider adapter
// implements, plus a registry of the adapters that really exist.
//
// Everything specific to one courier (URLs, auth headers, response shapes,
// error mapping) lives in that courier's adapter file. Nothing outside this
// package should know how Steadfast, Pathao, etc. authenticate.
//
// Only providers with verified official API documentation are registered.
// A provider that is not registered cannot be connected, and the UI only
// lists registered providers.
package courier

import (
	"context"
	"errors"
	"sort"
)

var (
	// ErrInvalidCredentials means the courier positively rejected the keys.
	ErrInvalidCredentials = errors.New("courier: credentials rejected")
	// ErrUnavailable means we could not get a trustworthy answer (network
	// error, timeout, 5xx, rate limit, or a response we do not recognise).
	// It says nothing about whether the credentials are valid.
	ErrUnavailable = errors.New("courier: provider unavailable")
)

// ErrInvalidField means the credentials were accepted but one field is wrong
// (for example a Store ID that is not on the account). Message is safe to
// show the seller and never contains secrets.
type ErrInvalidField struct{ Key, Message string }

func (e *ErrInvalidField) Error() string { return "courier: invalid field " + e.Key }

// Field types understood by the frontend form.
const (
	FieldText     = "text"
	FieldPassword = "password"
)

// CredentialField describes one input the seller must fill in.
type CredentialField struct {
	Key         string
	Label       string
	Type        string
	Placeholder string
	HelpText    string
	Required    bool
}

// Provider is a courier adapter.
type Provider interface {
	ID() string
	Name() string
	Fields() []CredentialField
	// Help is a short list of steps explaining where to find the credentials.
	Help() []string
	// DocsURL is an official documentation link, or "" if there is none.
	DocsURL() string

	// Identity returns the value that identifies the courier account
	// (for example the API key). It is hashed for duplicate detection and
	// never stored or shown. Must be pure: no network calls.
	Identity(creds map[string]string) string
	// Hint returns a masked, display-safe label for the account.
	Hint(creds map[string]string) string
	// Verify checks the credentials against the real courier API. It returns
	// nil only on positive confirmation, ErrInvalidCredentials when rejected,
	// and ErrUnavailable otherwise. Errors must never contain secrets.
	Verify(ctx context.Context, creds map[string]string) error
}

// Registry holds the implemented providers.
type Registry struct{ byID map[string]Provider }

func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{byID: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		r.byID[p.ID()] = p
	}
	return r
}

func (r *Registry) Get(id string) (Provider, bool) {
	p, ok := r.byID[id]
	return p, ok
}

// List returns providers sorted by name.
func (r *Registry) List() []Provider {
	out := make([]Provider, 0, len(r.byID))
	for _, p := range r.byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// maskTail returns "••••" plus the last four characters (or fewer if short).
func maskTail(s string) string {
	if len(s) <= 6 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}
